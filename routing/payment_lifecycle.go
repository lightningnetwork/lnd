package routing

import (
	"fmt"
	"time"

	"github.com/davecgh/go-spew/spew"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

// errNoRoute is returned when all routes from the payment session have been
// attempted.
type errNoRoute struct {
	// lastError is the error encountered during the last payment attempt,
	// if at least one attempt has been made.
	lastError error
}

// Error returns a string representation of the error.
func (e errNoRoute) Error() string {
	return fmt.Sprintf("unable to route payment to destination: %v",
		e.lastError)
}

// paymentShard is a type that wraps an attempt that is part of a (potentially)
// larger payment.
type paymentShard struct {
	*channeldb.PaymentAttemptInfo

	ResultChan chan *RouteResult
}

// paymentShards holds a set of active payment shards.
type paymentShards struct {
	shards     map[uint64]*paymentShard
	totalValue lnwire.MilliSatoshi
}

// addShard adds the given shard to the set of active payment shards.
func (p *paymentShards) addShard(s *paymentShard) {
	if p.shards == nil {
		p.shards = make(map[uint64]*paymentShard)
	}

	// Add the shard and update the total value of the set.
	p.shards[s.PaymentID] = s
	p.totalValue += s.Route.Amt()
}

// removeShard removes the given payment shard from the set.
func (p *paymentShards) removeShard(s *paymentShard) {
	// Remove and updat the total value.
	delete(p.shards, s.PaymentID)
	p.totalValue -= s.Route.Amt()
}

// shardResult is a struct that holds the payment result reported from the
// Switch for an attempt we made, together with a reference to the shard we
// sent for this attempt.
type shardResult struct {
	*paymentShard
	*htlcswitch.PaymentResult
}

// paymentLifecycle holds all information about the current state of a payment
// needed to resume if from any point.
type paymentLifecycle struct {
	router         *ChannelRouter
	payment        *LightningPayment
	paySession     PaymentSession
	timeoutChan    <-chan time.Time
	currentHeight  int32
	finalCLTVDelta uint16
	existingShards *paymentShards
	lastError      error

	// quit is closed to signal sub goroutines of the payment lifecycle to
	// stop.
	quit chan struct{}
}

// resumePayment resumes the paymentLifecycle from the current state.
func (p *paymentLifecycle) resumePayment() ([32]byte, *route.Route, error) {
	// When the payment lifecycle loop exits, we make sure to signal any
	// sub goroutine to exit.
	defer close(p.quit)

	// Cancel the payment session when we exit to signal underlying
	// goroutines can exit.
	defer p.paySession.Cancel()

	// The active set of shards start out as existingShards.
	shards := p.existingShards

	// Each time we send a new payment shard, we'll spin up a goroutine
	// that will collect the result. Either a payment result will be
	// returned, or a critical error signaling that we should immediately
	// exit.
	shardResults := make(chan *shardResult)
	criticalErr := make(chan error, 1)

	// If we had any existing shards outstanding, resume their goroutines
	// such that the final result will be given to the lifecycle loop
	// below.
	for _, s := range shards.shards {
		go p.collectShard(s, shardResults, criticalErr)
	}

	type paymentFailure struct {
		failureCode channeldb.FailureReason
		err         error
	}

	var (
		success         = false
		terminalFailure *paymentFailure
		routeFailure    *paymentFailure
	)

	// We'll continue until either our payment succeeds, or we encounter a
	// critical error during path finding.
	for {

		var failure *paymentFailure
		if terminalFailure != nil {
			failure = terminalFailure
		} else if routeFailure != nil {
			failure = routeFailure
		}

		if len(shards.shards) == 0 && (success || failure != nil) {
			log.Debugf("Payment done")
			// We are done! Get the final attempt results from the
			// database.
			attempts, err := p.router.cfg.Control.GetAttempts(
				p.payment.PaymentHash,
			)
			if err != nil {
				log.Errorf("Unable to succeed payment "+
					"attempt: %v", err)
				return [32]byte{}, nil, err
			}

			// Find the first successful shard and return the
			// preimage and route.
			for _, a := range attempts {
				log.Debugf("johan checking attempt %v", a.Preimage)
				if a.Failure != nil {
					continue
				}

				log.Debugf("johan found success %v", a.Preimage)
				return *a.Preimage, &a.Route, nil
			}

			if failure != nil {
				// If we're unable to successfully make a payment using
				// any of the routes we've found, then mark the payment
				// as permanently failed.
				saveErr := p.router.cfg.Control.Fail(
					p.payment.PaymentHash, failure.failureCode,
				)
				if saveErr != nil {
					return [32]byte{}, nil, saveErr
				}

				// Terminal state, return.
				return [32]byte{}, nil, failure.err
			}

			// TODO: payment level failure must stay.
			return [32]byte{}, nil, fmt.Errorf("No successful "+
				"attempts: %v", attempts[0].Failure)

		}

		var (
			newRoute chan *RouteIntent
			routeErr chan error
		)

		// If we are not done and there is still value to be sent,
		// request more routes to send shards along.
		remValue := p.payment.Amount - shards.totalValue
		if !success && routeFailure == nil && remValue > 0 {
			log.Debugf("Payment not done, requesting route")
			// When the facts change, I change my mind.
			newRoute, routeErr = p.paySession.RequestRoute(
				remValue, p.payment, uint32(p.currentHeight),
				p.finalCLTVDelta,
			)
		}

		// Wait for an exit condition to be reached, or a shard result
		// to be available.
		select {

		// One of the shard goroutines reported a critical error. Exit
		// immediately.
		case err := <-criticalErr:
			return [32]byte{}, nil, err

		// The router is exiting.
		case <-p.router.quit:
			// The payment will be resumed from the current state
			// after restart.
			return [32]byte{}, nil, ErrRouterShuttingDown

		// Before we attempt this next payment, we'll check to see if either
		// we've gone past the payment attempt timeout, or the router is
		// exiting. In either case, we'll stop this payment attempt short. If a
		// timeout is not applicable, timeoutChan will be nil.
		case <-p.timeoutChan:
			errStr := fmt.Sprintf("payment attempt not completed " +
				"before timeout")

			terminalFailure = &paymentFailure{
				failureCode: channeldb.FailureReasonTimeout,
				err:         newErr(ErrPaymentAttemptTimeout, errStr),
			}

		case err := <-routeErr:
			log.Warnf("Failed to find route for payment %x: %v",
				p.payment.PaymentHash, err)

			// Convert error to payment-level failure.
			routeFailure = &paymentFailure{
				failureCode: errorToPaymentFailure(err),
				err:         err,
			}

			// If there was an error already recorded for this
			// payment, we'll return that.
			if p.lastError != nil {
				routeFailure.err = errNoRoute{lastError: p.lastError}
			}

		case rt := <-newRoute:
			// Using the route received from the payment session,
			// create a new shard to send.
			firstHop, htlcAdd, attempt, err := p.createNewPaymentAttempt(
				rt.Route,
			)
			// With SendToRoute, it can happen that the route exceeds protocol
			// constraints. Mark the payment as failed with an internal error.
			if err == route.ErrMaxRouteHopsExceeded ||
				err == sphinx.ErrMaxRoutingInfoSizeExceeded {

				log.Debugf("Invalid route provided for payment %x: %v",
					p.payment.PaymentHash, err)

				terminalFailure = &paymentFailure{
					failureCode: channeldb.FailureReasonError,
					err:         err,
				}
				break
			}

			// In any case, don't continue if there is an error.
			if err != nil {
				return [32]byte{}, nil, err
			}

			// Before sending this HTLC to the switch, we checkpoint the
			// fresh paymentID and route to the DB. This lets us know on
			// startup the ID of the payment that we attempted to send,
			// such that we can query the Switch for its whereabouts. The
			// route is needed to handle the result when it eventually
			// comes back.
			err = p.router.cfg.Control.RegisterAttempt(p.payment.PaymentHash, attempt)
			if err != nil {
				return [32]byte{}, nil, err
			}

			// Now that the attempt was successfully checkpointed
			// to the control tower, add it to our set of active
			// shards.
			s := &paymentShard{
				attempt,
				rt.ResultChan,
			}
			shards.addShard(s)

			// Now that the attempt is created and checkpointed to
			// the DB, we send it.
			sendErr := p.sendPaymentAttempt(
				s.PaymentAttemptInfo, firstHop, htlcAdd,
			)
			if sendErr != nil {
				// Mark the attempt failed.
				// TODO: make fiail method on shard that also sends result on channel.
				err := p.router.cfg.Control.FailAttempt(
					p.payment.PaymentHash,
					s.PaymentAttemptInfo,
					channeldb.AttemptFailureReasonUnknown,
				)
				if err != nil {
					return [32]byte{}, nil, err
				}

				rt.ResultChan <- &RouteResult{
					Err: sendErr,
				}

				// We must inspect the error to know whether it
				// was critical or not, to decide whether we
				// should continue trying.
				reason := p.router.processSendError(
					s.PaymentID, &s.Route, sendErr,
				)
				if reason != nil {
					log.Debugf("Payment %x failed: final_outcome=%v, raw_err=%v",
						p.payment.PaymentHash, *reason, sendErr)

					// TODO: must wait for shards before failing.
					// Mark the payment failed with no route.
					//
					// TODO(halseth): make payment codes for the actual reason we don't
					// continue path finding.
					err := p.router.cfg.Control.Fail(
						p.payment.PaymentHash, *reason,
					)
					if err != nil {
						return [32]byte{}, nil, err
					}

					// Terminal state, return the error we encountered.

					// set terminal error?
					return [32]byte{}, nil, sendErr
				}
				// Save the forwarding error so it can be returned if
				// this turns out to be the last attempt.
				p.lastError = sendErr

				// Error was handled successfully, remove the
				// shard from our set of active shards indicate
				// we want to make a new attempt.
				shards.removeShard(s)
				continue
			}

			// Now that the shard was sent, spin up a goroutine
			// that will forward the result to the lifecycle loop
			// when available.
			go p.collectShard(s, shardResults, criticalErr)

		// A result for one of the shards is available.
		case s := <-shardResults:
			log.Debugf("got shard resutl")
			routeFailure = nil
			result := s.PaymentResult

			// In case of a payment failure, we use the error to decide
			// whether we should retry.
			if result.Error != nil {
				log.Errorf("Attempt to send payment %x failed: %v",
					p.payment.PaymentHash, result.Error)

				// Mark the attempt failed.
				err := p.router.cfg.Control.FailAttempt(
					p.payment.PaymentHash,
					s.PaymentAttemptInfo,
					channeldb.AttemptFailureReasonUnknown,
				)
				if err != nil {
					return [32]byte{}, nil, err
				}

				s.ResultChan <- &RouteResult{
					Err: result.Error,
				}

				// We must inspect the error to know whether it was
				// critical or not, to decide whether we should
				// continue trying.
				sendErr := result.Error
				reason := p.router.processSendError(
					s.PaymentID, &s.Route, sendErr,
				)

				if reason != nil {
					log.Debugf("Payment %x failed: final_outcome=%v, raw_err=%v",
						p.payment.PaymentHash, *reason, sendErr)

					// TODO: wait for attempts

					// Mark the payment failed with no route.
					//
					// TODO(halseth): make payment codes for the actual reason we don't
					// continue path finding.
					err := p.router.cfg.Control.Fail(
						p.payment.PaymentHash, *reason,
					)
					if err != nil {
						return [32]byte{}, nil, err
					}

					// Terminal state, return the error we encountered.
					return [32]byte{}, nil, sendErr
				}

				// MSUT FAIL ATTEMPT HERE ALSO!

				// egt rid of last err?
				// Save the forwarding error so it can be returned if
				// this turns out to be the last attempt.
				p.lastError = sendErr

				// Error was handled successfully, reset the attempt to
				// indicate we want to make a new attempt.
				shards.removeShard(s.paymentShard)

				continue
			}

			// We successfully got a payment result back from the switch.
			log.Debugf("Payment %x succeeded with pid=%v",
				p.payment.PaymentHash, s.PaymentID)

			// Report success to mission control.
			err := p.router.cfg.MissionControl.ReportPaymentSuccess(
				s.PaymentID, &s.Route,
			)
			if err != nil {
				log.Errorf("Error reporting payment success to mc: %v",
					err)
			}

			// In case of success we atomically store the db payment and
			// move the payment to the success state.
			err = p.router.cfg.Control.SettleAttempt(
				p.payment.PaymentHash, s.PaymentAttemptInfo,
				result.Preimage,
			)
			if err != nil {
				log.Errorf("Unable to succeed payment "+
					"attempt: %v", err)
				return [32]byte{}, nil, err
			}
			shards.removeShard(s.paymentShard)

			// TOOD: unify with settle/fail attempt
			s.ResultChan <- &RouteResult{
				Preimage: result.Preimage,
			}

			// Since the assumption is that the whole payment is
			// successful when one shard finishes, mark us done to
			// wait for any outstanding shards.
			success = true
		}
	}
}

// collectShard waits for a result to be available for the given shard, and
// delivers it on the resultChan.
func (p *paymentLifecycle) collectShard(s *paymentShard,
	resultChan chan<- *shardResult, errChan chan error) {

	result, err := p.waitForPaymentResult(s.PaymentAttemptInfo)
	if err != nil {
		select {
		case errChan <- err:
		case <-p.quit:
		}
		return
	}

	// Notify about the result available for this shard.
	res := &shardResult{
		paymentShard:  s,
		PaymentResult: result,
	}

	select {
	case resultChan <- res:
	case <-p.quit:
	}
}

// waitForPaymentResult blocks until a result for the given attempt is returned
// from the switch.
func (p *paymentLifecycle) waitForPaymentResult(
	attempt *channeldb.PaymentAttemptInfo) (*htlcswitch.PaymentResult, error) {

	// If this was a resumed attempt, we must regenerate the
	// circuit. We don't need to check for errors resulting
	// from an invalid route, because the sphinx packet has
	// been successfully generated before.
	_, circuit, err := generateSphinxPacket(
		&attempt.Route, p.payment.PaymentHash[:],
		attempt.SessionKey,
	)
	if err != nil {
		return nil, err
	}

	// Using the created circuit, initialize the error decrypter so we can
	// parse+decode any failures incurred by this payment within the
	// switch.
	errorDecryptor := &htlcswitch.SphinxErrorDecrypter{
		OnionErrorDecrypter: sphinx.NewOnionErrorDecrypter(circuit),
	}

	// Now ask the switch to return the result of the payment when
	// available.
	resultChan, err := p.router.cfg.Payer.GetPaymentResult(
		attempt.PaymentID, p.payment.PaymentHash, errorDecryptor,
	)
	if err != nil {
		return nil, err
	}

	// The switch knows about this payment, we'll wait for a result
	// to be available.
	select {
	case result, ok := <-resultChan:
		if !ok {
			return nil, htlcswitch.ErrSwitchExiting
		}

		return result, nil

	case <-p.router.quit:
		return nil, ErrRouterShuttingDown
	}
}

// errorToPaymentFailure takes a path finding error and converts it into a
// payment-level failure.
func errorToPaymentFailure(err error) channeldb.FailureReason {
	switch err {
	case
		errNoTlvPayload,
		errNoPaymentAddr,
		errNoPathFound,
		errPrebuiltRouteTried:

		return channeldb.FailureReasonNoRoute

	case errInsufficientBalance:
		return channeldb.FailureReasonInsufficientBalance
	}

	return channeldb.FailureReasonError
}

// createNewPaymentAttempt creates a new payment attempt from the given route.
func (p *paymentLifecycle) createNewPaymentAttempt(rt *route.Route) (
	lnwire.ShortChannelID, *lnwire.UpdateAddHTLC,
	*channeldb.PaymentAttemptInfo, error) {

	// Generate a new key to be used for this attempt.
	sessionKey, err := generateNewSessionKey()
	if err != nil {
		return lnwire.ShortChannelID{}, nil, nil, err
	}

	// Generate the raw encoded sphinx packet to be included along
	// with the htlcAdd message that we send directly to the
	// switch.
	onionBlob, _, err := generateSphinxPacket(
		rt, p.payment.PaymentHash[:], sessionKey,
	)
	if err != nil {
		return lnwire.ShortChannelID{}, nil, nil, err
	}

	// Craft an HTLC packet to send to the layer 2 switch. The
	// metadata within this packet will be used to route the
	// payment through the network, starting with the first-hop.
	htlcAdd := &lnwire.UpdateAddHTLC{
		Amount:      rt.TotalAmount,
		Expiry:      rt.TotalTimeLock,
		PaymentHash: p.payment.PaymentHash,
	}
	copy(htlcAdd.OnionBlob[:], onionBlob)

	// Attempt to send this payment through the network to complete
	// the payment. If this attempt fails, then we'll continue on
	// to the next available route.
	firstHop := lnwire.NewShortChanIDFromInt(
		rt.Hops[0].ChannelID,
	)

	// We generate a new, unique payment ID that we will use for
	// this HTLC.
	paymentID, err := p.router.cfg.NextPaymentID()
	if err != nil {
		return lnwire.ShortChannelID{}, nil, nil, err
	}

	// We now have all the information needed to populate
	// the current attempt information.
	attempt := &channeldb.PaymentAttemptInfo{
		PaymentID:  paymentID,
		SessionKey: sessionKey,
		Route:      *rt,
	}

	return firstHop, htlcAdd, attempt, nil
}

// sendPaymentAttempt attempts to send the current attempt to the switch.
func (p *paymentLifecycle) sendPaymentAttempt(
	attempt *channeldb.PaymentAttemptInfo, firstHop lnwire.ShortChannelID,
	htlcAdd *lnwire.UpdateAddHTLC) error {

	log.Tracef("Attempting to send payment %x (pid=%v), "+
		"using route: %v", p.payment.PaymentHash, attempt.PaymentID,
		newLogClosure(func() string {
			return spew.Sdump(attempt.Route)
		}),
	)

	// Send it to the Switch. When this method returns we assume
	// the Switch successfully has persisted the payment attempt,
	// such that we can resume waiting for the result after a
	// restart.
	err := p.router.cfg.Payer.SendHTLC(
		firstHop, attempt.PaymentID, htlcAdd,
	)
	if err != nil {
		log.Errorf("Failed sending attempt %d for payment "+
			"%x to switch: %v", attempt.PaymentID,
			p.payment.PaymentHash, err)
		return err
	}

	log.Debugf("Payment %x (pid=%v) successfully sent to switch, route: %v",
		p.payment.PaymentHash, attempt.PaymentID, &attempt.Route)

	return nil
}
