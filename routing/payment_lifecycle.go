package routing

import (
	"fmt"
	"time"

	"github.com/davecgh/go-spew/spew"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

// paymentLifecycle holds all information about the current state of a payment
// needed to resume if from any point.
type paymentLifecycle struct {
	router        *ChannelRouter
	totalAmount   lnwire.MilliSatoshi
	feeLimit      lnwire.MilliSatoshi
	paymentHash   lntypes.Hash
	paySession    PaymentSession
	timeoutChan   <-chan time.Time
	currentHeight int32
}

// resumePayment resumes the paymentLifecycle from the current state.
func (p *paymentLifecycle) resumePayment() ([32]byte, *route.Route, error) {
	shardHandler := &shardHandler{
		router:      p.router,
		paymentHash: p.paymentHash,
	}

	// We'll continue until either our payment succeeds, or we encounter a
	// critical error during path finding.
	for {
		// We start every iteration by fetching the lastest state of
		// the payment from the ControlTower. This ensures that we will
		// act on the latest available information, whether we are
		// resuming an existing payment or just sent a new attempt.
		payment, err := p.router.cfg.Control.FetchPayment(
			p.paymentHash,
		)
		if err != nil {
			return [32]byte{}, nil, err
		}

		// Go through the HTLCs for this payment, determining if there
		// are any in flight or settled.
		var (
			attempt *channeldb.HTLCAttemptInfo
			settle  *channeldb.HTLCAttempt
		)
		for _, a := range payment.HTLCs {
			a := a

			// We have a settled HTLC, and should return when all
			// shards are back.
			if a.Settle != nil {
				settle = &a
				continue
			}

			// This HTLC already failed, ignore.
			if a.Failure != nil {
				continue
			}

			// HTLC was neither setteld nor failed, it is still in
			// flight.
			attempt = &a.HTLCAttemptInfo
			break
		}

		// Terminal state, return the preimage and the route taken.
		if attempt == nil && settle != nil {
			return settle.Settle.Preimage, &settle.Route, nil
		}

		// If this payment had no existing payment attempt, we create
		// and send one now.
		if attempt == nil {
			// Before we attempt this next payment, we'll check to see if either
			// we've gone past the payment attempt timeout, or the router is
			// exiting. In either case, we'll stop this payment attempt short. If a
			// timeout is not applicable, timeoutChan will be nil.
			select {
			case <-p.timeoutChan:
				// Mark the payment as failed because of the
				// timeout.
				err := p.router.cfg.Control.Fail(
					p.paymentHash, channeldb.FailureReasonTimeout,
				)
				if err != nil {
					return [32]byte{}, nil, err
				}

				errStr := fmt.Sprintf("payment attempt not completed " +
					"before timeout")

				return [32]byte{}, nil, newErr(ErrPaymentAttemptTimeout, errStr)

			// The payment will be resumed from the current state
			// after restart.
			case <-p.router.quit:
				return [32]byte{}, nil, ErrRouterShuttingDown

			// Fall through if we haven't hit our time limit or are
			// exiting.
			default:
			}

			// Create a new payment attempt from the given payment session.
			rt, err := p.paySession.RequestRoute(
				p.totalAmount, p.feeLimit, 0, uint32(p.currentHeight),
			)
			if err != nil {
				log.Warnf("Failed to find route for payment %x: %v",
					p.paymentHash, err)

				// Convert error to payment-level failure.
				failure := errorToPaymentFailure(err)

				// If we're unable to successfully make a payment using
				// any of the routes we've found, then mark the payment
				// as permanently failed.
				saveErr := p.router.cfg.Control.Fail(
					p.paymentHash, failure,
				)
				if saveErr != nil {
					return [32]byte{}, nil, saveErr
				}

				// Terminal state, return.
				return [32]byte{}, nil, err
			}

			// With the route in hand, launch a new shard.
			var outcome *launchOutcome
			attempt, outcome, err = shardHandler.launchShard(rt)
			if err != nil {
				return [32]byte{}, nil, err
			}

			// We ew encountered a non-critical error when
			// launching the shard, handle it
			if outcome.err != nil {
				// We must inspect the error to know whether it
				// was critical or not, to decide whether we
				// should continue trying.
				err = shardHandler.handleSendError(
					attempt, outcome.err,
				)
				if err != nil {
					return [32]byte{}, nil, err
				}

				// Error was handled successfully, continue to
				// make a new attempt.
				continue
			}
		}

		// Whether this was an existing attempt or one we just sent,
		// we'll now collect its result. We ignore the result for now
		// if it is a success, as we will look it up in the control
		// tower on the next loop iteration.
		result, err := shardHandler.collectResult(attempt)
		if err != nil {
			return [32]byte{}, nil, err
		}

		if result.err != nil {
			// We must inspect the error to know whether it was
			// critical or not, to decide whether we should
			// continue trying.
			err = shardHandler.handleSendError(attempt, result.err)
			if err != nil {
				return [32]byte{}, nil, err
			}

			// Error was handled successfully, continue to make a
			// new attempt.
			continue
		}
	}
}

// shardHandler holds what is necessary to send and collect the result of
// shards.
type shardHandler struct {
	paymentHash lntypes.Hash
	router      *ChannelRouter
}

// launchOutcome is a type returned from launchShard that indicates whether the
// shard was successfully send onto the network.
type launchOutcome struct {
	// err is non-nil if a non-critical error was encountered when trying
	// to send the shard, and we successfully updated the control tower to
	// reflect this error. This can be errors like not enough local
	// balance for the given route etc.
	err error
}

// launchShard creates and sends an HTLC attempt along the given route,
// registering it with the control tower before sending it. It returns the
// HTLCAttemptInfo that was created for the shard, along with a launchOutcome.
// The launchOutcome is used to indicate whether the attempt was successfully
// sent. If the launchOutcome wraps a non-nil error, it means that the attempt
// was not sent onto the network, so no result will be available in the future
// for it.
func (p *shardHandler) launchShard(rt *route.Route) (*channeldb.HTLCAttemptInfo,
	*launchOutcome, error) {

	// Using the route received from the payment session, create a new
	// shard to send.
	firstHop, htlcAdd, attempt, err := p.createNewPaymentAttempt(
		rt,
	)
	if err != nil {
		return nil, nil, err
	}

	// Before sending this HTLC to the switch, we checkpoint the fresh
	// paymentID and route to the DB. This lets us know on startup the ID
	// of the payment that we attempted to send, such that we can query the
	// Switch for its whereabouts. The route is needed to handle the result
	// when it eventually comes back.
	err = p.router.cfg.Control.RegisterAttempt(p.paymentHash, attempt)
	if err != nil {
		return nil, nil, err
	}

	// Now that the attempt is created and checkpointed to the DB, we send
	// it.
	sendErr := p.sendPaymentAttempt(attempt, firstHop, htlcAdd)
	if sendErr != nil {
		// TODO(joostjager): Distinguish unexpected internal errors
		// from real send errors.
		err := p.failAttempt(attempt, sendErr)
		if err != nil {
			return nil, nil, err
		}

		// Return a launchOutcome indicating the shard failed.
		return attempt, &launchOutcome{
			err: sendErr,
		}, nil
	}

	return attempt, &launchOutcome{}, nil
}

// shardResult holds the resulting outcome of a shard sent.
type shardResult struct {
	// preimage is the payment preimage in case of a settled HTLC. Only set
	// if err is non-nil.
	preimage lntypes.Preimage

	// err indicates that the shard failed.
	err error
}

// collectResult waits for the result for the given attempt to be available
// from the Switch, then records the attempt outcome with the control tower. A
// shardResult is returned, indicating the final outcome of this HTLC attempt.
func (p *shardHandler) collectResult(attempt *channeldb.HTLCAttemptInfo) (
	*shardResult, error) {

	// Regenerate the circuit for this attempt.
	_, circuit, err := generateSphinxPacket(
		&attempt.Route, p.paymentHash[:],
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
		attempt.AttemptID, p.paymentHash, errorDecryptor,
	)
	switch {

	// If this attempt ID is unknown to the Switch, it means it was never
	// checkpointed and forwarded by the switch before a restart. In this
	// case we can safely send a new payment attempt, and wait for its
	// result to be available.
	case err == htlcswitch.ErrPaymentIDNotFound:
		log.Debugf("Payment ID %v for hash %x not found in "+
			"the Switch, retrying.", attempt.AttemptID,
			p.paymentHash)

		cErr := p.failAttempt(attempt, err)
		if cErr != nil {
			return nil, cErr
		}

		return &shardResult{
			err: err,
		}, nil

	// A critical, unexpected error was encountered.
	case err != nil:
		log.Errorf("Failed getting result for attemptID %d "+
			"from switch: %v", attempt.AttemptID, err)

		return nil, err
	}

	// The switch knows about this payment, we'll wait for a result to be
	// available.
	var (
		result *htlcswitch.PaymentResult
		ok     bool
	)

	select {
	case result, ok = <-resultChan:
		if !ok {
			return nil, htlcswitch.ErrSwitchExiting
		}

	case <-p.router.quit:
		return nil, ErrRouterShuttingDown
	}

	// In case of a payment failure, fail the attempt with the control
	// tower and return.
	if result.Error != nil {
		log.Errorf("Attempt to send payment %x failed: %v",
			p.paymentHash, result.Error)

		err := p.failAttempt(attempt, result.Error)
		if err != nil {
			return nil, err
		}

		return &shardResult{
			err: result.Error,
		}, nil
	}

	// We successfully got a payment result back from the switch.
	log.Debugf("Payment %x succeeded with pid=%v",
		p.paymentHash, attempt.AttemptID)

	// Report success to mission control.
	err = p.router.cfg.MissionControl.ReportPaymentSuccess(
		attempt.AttemptID, &attempt.Route,
	)
	if err != nil {
		log.Errorf("Error reporting payment success to mc: %v",
			err)
	}

	// In case of success we atomically store settle result to the DB move
	// the shard to the settled state.
	err = p.router.cfg.Control.SettleAttempt(
		p.paymentHash, attempt.AttemptID,
		&channeldb.HTLCSettleInfo{
			Preimage:   result.Preimage,
			SettleTime: p.router.cfg.Clock.Now(),
		},
	)
	if err != nil {
		log.Errorf("Unable to succeed payment attempt: %v", err)
		return nil, err
	}

	return &shardResult{
		preimage: result.Preimage,
	}, nil
}

// errorToPaymentFailure takes a path finding error and converts it into a
// payment-level failure.
func errorToPaymentFailure(err error) channeldb.FailureReason {
	switch err {
	case
		errNoTlvPayload,
		errNoPaymentAddr,
		errNoPathFound,
		errEmptyPaySession:

		return channeldb.FailureReasonNoRoute

	case errInsufficientBalance:
		return channeldb.FailureReasonInsufficientBalance
	}

	return channeldb.FailureReasonError
}

// createNewPaymentAttempt creates a new payment attempt from the given route.
func (p *shardHandler) createNewPaymentAttempt(rt *route.Route) (
	lnwire.ShortChannelID, *lnwire.UpdateAddHTLC,
	*channeldb.HTLCAttemptInfo, error) {

	// Generate a new key to be used for this attempt.
	sessionKey, err := generateNewSessionKey()
	if err != nil {
		return lnwire.ShortChannelID{}, nil, nil, err
	}

	// Generate the raw encoded sphinx packet to be included along
	// with the htlcAdd message that we send directly to the
	// switch.
	onionBlob, _, err := generateSphinxPacket(
		rt, p.paymentHash[:], sessionKey,
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
		PaymentHash: p.paymentHash,
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
	attemptID, err := p.router.cfg.NextPaymentID()
	if err != nil {
		return lnwire.ShortChannelID{}, nil, nil, err
	}

	// We now have all the information needed to populate
	// the current attempt information.
	attempt := &channeldb.HTLCAttemptInfo{
		AttemptID:   attemptID,
		AttemptTime: p.router.cfg.Clock.Now(),
		SessionKey:  sessionKey,
		Route:       *rt,
	}

	return firstHop, htlcAdd, attempt, nil
}

// sendPaymentAttempt attempts to send the current attempt to the switch.
func (p *shardHandler) sendPaymentAttempt(
	attempt *channeldb.HTLCAttemptInfo, firstHop lnwire.ShortChannelID,
	htlcAdd *lnwire.UpdateAddHTLC) error {

	log.Tracef("Attempting to send payment %x (pid=%v), "+
		"using route: %v", p.paymentHash, attempt.AttemptID,
		newLogClosure(func() string {
			return spew.Sdump(attempt.Route)
		}),
	)

	// Send it to the Switch. When this method returns we assume
	// the Switch successfully has persisted the payment attempt,
	// such that we can resume waiting for the result after a
	// restart.
	err := p.router.cfg.Payer.SendHTLC(
		firstHop, attempt.AttemptID, htlcAdd,
	)
	if err != nil {
		log.Errorf("Failed sending attempt %d for payment "+
			"%x to switch: %v", attempt.AttemptID,
			p.paymentHash, err)
		return err
	}

	log.Debugf("Payment %x (pid=%v) successfully sent to switch, route: %v",
		p.paymentHash, attempt.AttemptID, &attempt.Route)

	return nil
}

// handleSendError inspects the given error from the Switch and determines
// whether we should make another payment attempt.
func (p *shardHandler) handleSendError(attempt *channeldb.HTLCAttemptInfo,
	sendErr error) error {

	reason := p.router.processSendError(
		attempt.AttemptID, &attempt.Route, sendErr,
	)
	if reason == nil {
		return nil
	}

	log.Debugf("Payment %x failed: final_outcome=%v, raw_err=%v",
		p.paymentHash, *reason, sendErr)

	// Mark the payment failed with no route.
	//
	// TODO(halseth): make payment codes for the actual reason we don't
	// continue path finding.
	err := p.router.cfg.Control.Fail(
		p.paymentHash, *reason,
	)
	if err != nil {
		return err
	}

	// Terminal state, return the error we encountered.
	return sendErr
}

// failAttempt calls control tower to fail the current payment attempt.
func (p *shardHandler) failAttempt(attempt *channeldb.HTLCAttemptInfo,
	sendError error) error {

	failInfo := marshallError(
		sendError,
		p.router.cfg.Clock.Now(),
	)

	return p.router.cfg.Control.FailAttempt(
		p.paymentHash, attempt.AttemptID,
		failInfo,
	)
}

// marshallError marshall an error as received from the switch to a structure
// that is suitable for database storage.
func marshallError(sendError error, time time.Time) *channeldb.HTLCFailInfo {
	response := &channeldb.HTLCFailInfo{
		FailTime: time,
	}

	switch sendError {

	case htlcswitch.ErrPaymentIDNotFound:
		response.Reason = channeldb.HTLCFailInternal
		return response

	case htlcswitch.ErrUnreadableFailureMessage:
		response.Reason = channeldb.HTLCFailUnreadable
		return response
	}

	rtErr, ok := sendError.(htlcswitch.ClearTextError)
	if !ok {
		response.Reason = channeldb.HTLCFailInternal
		return response
	}

	message := rtErr.WireMessage()
	if message != nil {
		response.Reason = channeldb.HTLCFailMessage
		response.Message = message
	} else {
		response.Reason = channeldb.HTLCFailUnknown
	}

	// If the ClearTextError received is a ForwardingError, the error
	// originated from a node along the route, not locally on our outgoing
	// link. We set failureSourceIdx to the index of the node where the
	// failure occurred. If the error is not a ForwardingError, the failure
	// occurred at our node, so we leave the index as 0 to indicate that
	// we failed locally.
	fErr, ok := rtErr.(*htlcswitch.ForwardingError)
	if ok {
		response.FailureSourceIndex = uint32(fErr.FailureSourceIdx)
	}

	return response
}
