package routing

import (
	"fmt"
	"sync"
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

// payemntState holds a number of key insights learned from a given MPPayment
// that we use to determine what to do on each payment loop iteration.
type paymentState struct {
	numShardsInFlight int
	remainingAmt      lnwire.MilliSatoshi
	remainingFees     lnwire.MilliSatoshi
	terminate         bool
}

// paymentState uses the passed payment to find the latest information we need
// to act on every iteration of the payment loop.
func (p *paymentLifecycle) paymentState(payment *channeldb.MPPayment) (
	*paymentState, error) {

	// Fetch the total amount and fees that has already been sent in
	// settled and still in-flight shards.
	sentAmt, fees := payment.SentAmt()

	// Sanity check we haven't sent a value larger than the payment amount.
	if sentAmt > p.totalAmount {
		return nil, fmt.Errorf("amount sent %v exceeds "+
			"total amount %v", sentAmt, p.totalAmount)
	}

	// We'll subtract the used fee from our fee budget, but allow the fees
	// of the already sent shards to exceed our budget (can happen after
	// restarts).
	feeBudget := p.feeLimit
	if fees <= feeBudget {
		feeBudget -= fees
	} else {
		feeBudget = 0
	}

	// Get any terminal info for this payment.
	settle, failure := payment.TerminalInfo()

	// If either an HTLC settled, or the payment has a payment level
	// failure recorded, it means we should terminate the moment all shards
	// have returned with a result.
	terminate := settle != nil || failure != nil

	activeShards := payment.InFlightHTLCs()
	return &paymentState{
		numShardsInFlight: len(activeShards),
		remainingAmt:      p.totalAmount - sentAmt,
		remainingFees:     feeBudget,
		terminate:         terminate,
	}, nil
}

// resumePayment resumes the paymentLifecycle from the current state.
func (p *paymentLifecycle) resumePayment() ([32]byte, *route.Route, error) {
	shardHandler := &shardHandler{
		router:      p.router,
		paymentHash: p.paymentHash,
		shardErrors: make(chan error),
		quit:        make(chan struct{}),
	}

	// When the payment lifecycle loop exits, we make sure to signal any
	// sub goroutine of the shardHandler to exit, then wait for them to
	// return.
	defer shardHandler.stop()

	// If we had any existing attempts outstanding, we'll start by spinning
	// up goroutines that'll collect their results and deliver them to the
	// lifecycle loop below.
	payment, err := p.router.cfg.Control.FetchPayment(
		p.paymentHash,
	)
	if err != nil {
		return [32]byte{}, nil, err
	}

	for _, a := range payment.InFlightHTLCs() {
		a := a

		log.Debugf("Resuming payment shard %v for hash %v",
			a.AttemptID, p.paymentHash)

		shardHandler.collectResultAsync(&a.HTLCAttemptInfo)
	}

	// We'll continue until either our payment succeeds, or we encounter a
	// critical error during path finding.
	for {
		// Start by quickly checking if there are any outcomes already
		// available to handle before we reevaluate our state.
		if err := shardHandler.checkShards(); err != nil {
			return [32]byte{}, nil, err
		}

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

		// Using this latest state of the payment, calculate
		// information about our active shards and terminal conditions.
		state, err := p.paymentState(payment)
		if err != nil {
			return [32]byte{}, nil, err
		}

		log.Debugf("Payment %v in state terminate=%v, "+
			"active_shards=%v, rem_value=%v, fee_limit=%v",
			p.paymentHash, state.terminate, state.numShardsInFlight,
			state.remainingAmt, state.remainingFees)

		switch {

		// We have a terminal condition and no active shards, we are
		// ready to exit.
		case state.terminate && state.numShardsInFlight == 0:
			// Find the first successful shard and return
			// the preimage and route.
			for _, a := range payment.HTLCs {
				if a.Settle != nil {
					return a.Settle.Preimage, &a.Route, nil
				}
			}

			// Payment failed.
			return [32]byte{}, nil, *payment.FailureReason

		// If we either reached a terminal error condition (but had
		// active shards still) or there is no remaining value to send,
		// we'll wait for a shard outcome.
		case state.terminate || state.remainingAmt == 0:
			// We still have outstanding shards, so wait for a new
			// outcome to be available before re-evaluating our
			// state.
			if err := shardHandler.waitForShard(); err != nil {
				return [32]byte{}, nil, err
			}
			continue
		}

		// Before we attempt any new shard, we'll check to see if
		// either we've gone past the payment attempt timeout, or the
		// router is exiting. In either case, we'll stop this payment
		// attempt short. If a timeout is not applicable, timeoutChan
		// will be nil.
		select {
		case <-p.timeoutChan:
			log.Warnf("payment attempt not completed before " +
				"timeout")

			// By marking the payment failed with the control
			// tower, no further shards will be launched and we'll
			// return with an error the moment all active shards
			// have finished.
			saveErr := p.router.cfg.Control.Fail(
				p.paymentHash, channeldb.FailureReasonTimeout,
			)
			if saveErr != nil {
				return [32]byte{}, nil, saveErr
			}

			continue

		case <-p.router.quit:
			return [32]byte{}, nil, ErrRouterShuttingDown

		// Fall through if we haven't hit our time limit.
		default:
		}

		// Create a new payment attempt from the given payment session.
		rt, err := p.paySession.RequestRoute(
			state.remainingAmt, state.remainingFees,
			uint32(state.numShardsInFlight), uint32(p.currentHeight),
		)
		if err != nil {
			log.Warnf("Failed to find route for payment %v: %v",
				p.paymentHash, err)

			routeErr, ok := err.(noRouteError)
			if !ok {
				return [32]byte{}, nil, err
			}

			// There is no route to try, and we have no active
			// shards. This means that there is no way for us to
			// send the payment, so mark it failed with no route.
			if state.numShardsInFlight == 0 {
				failureCode := routeErr.FailureReason()
				log.Debugf("Marking payment %v permanently "+
					"failed with no route: %v",
					p.paymentHash, failureCode)

				saveErr := p.router.cfg.Control.Fail(
					p.paymentHash, failureCode,
				)
				if saveErr != nil {
					return [32]byte{}, nil, saveErr
				}

				continue
			}

			// We still have active shards, we'll wait for an
			// outcome to be available before retrying.
			if err := shardHandler.waitForShard(); err != nil {
				return [32]byte{}, nil, err
			}
			continue
		}

		// We found a route to try, launch a new shard.
		attempt, outcome, err := shardHandler.launchShard(rt)
		if err != nil {
			return [32]byte{}, nil, err
		}

		// If we encountered a non-critical error when launching the
		// shard, handle it.
		if outcome.err != nil {
			log.Warnf("Failed to launch shard %v for "+
				"payment %v: %v", attempt.AttemptID,
				p.paymentHash, outcome.err)

			// We must inspect the error to know whether it was
			// critical or not, to decide whether we should
			// continue trying.
			err := shardHandler.handleSendError(
				attempt, outcome.err,
			)
			if err != nil {
				return [32]byte{}, nil, err
			}

			// Error was handled successfully, continue to make a
			// new attempt.
			continue
		}

		// Now that the shard was successfully sent, launch a go
		// routine that will handle its result when its back.
		shardHandler.collectResultAsync(attempt)
	}
}

// shardHandler holds what is necessary to send and collect the result of
// shards.
type shardHandler struct {
	paymentHash lntypes.Hash
	router      *ChannelRouter

	// shardErrors is a channel where errors collected by calling
	// collectResultAsync will be delivered. These results are meant to be
	// inspected by calling waitForShard or checkShards, and the channel
	// doesn't need to be initiated if the caller is using the sync
	// collectResult directly.
	shardErrors chan error

	// quit is closed to signal the sub goroutines of the payment lifecycle
	// to stop.
	quit chan struct{}
	wg   sync.WaitGroup
}

// stop signals any active shard goroutine to exit and waits for them to exit.
func (p *shardHandler) stop() {
	close(p.quit)
	p.wg.Wait()
}

// waitForShard blocks until any of the outstanding shards return.
func (p *shardHandler) waitForShard() error {
	select {
	case err := <-p.shardErrors:
		return err

	case <-p.quit:
		return fmt.Errorf("shard handler quitting")

	case <-p.router.quit:
		return ErrRouterShuttingDown
	}
}

// checkShards is a non-blocking method that check if any shards has finished
// their execution.
func (p *shardHandler) checkShards() error {
	for {
		select {
		case err := <-p.shardErrors:
			if err != nil {
				return err
			}

		case <-p.quit:
			return fmt.Errorf("shard handler quitting")

		case <-p.router.quit:
			return ErrRouterShuttingDown

		default:
			return nil
		}
	}
}

// launchOutcome is a type returned from launchShard that indicates whether the
// shard was successfully send onto the network.
type launchOutcome struct {
	// err is non-nil if a non-critical error was encountered when trying
	// to send the shard, and we successfully updated the control tower to
	// reflect this error. This can be errors like not enough local
	// balance for the given route etc.
	err error

	// attempt is the attempt structure as recorded in the database.
	attempt *channeldb.HTLCAttempt
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
		htlcAttempt, err := p.failAttempt(attempt, sendErr)
		if err != nil {
			return nil, nil, err
		}

		// Return a launchOutcome indicating the shard failed.
		return attempt, &launchOutcome{
			attempt: htlcAttempt,
			err:     sendErr,
		}, nil
	}

	return attempt, &launchOutcome{}, nil
}

// shardResult holds the resulting outcome of a shard sent.
type shardResult struct {
	// attempt is the attempt structure as recorded in the database.
	attempt *channeldb.HTLCAttempt

	// err indicates that the shard failed.
	err error
}

// collectResultAsync launches a goroutine that will wait for the result of the
// given HTLC attempt to be available then handle its result. Note that it will
// fail the payment with the control tower if a terminal error is encountered.
func (p *shardHandler) collectResultAsync(attempt *channeldb.HTLCAttemptInfo) {
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()

		// Block until the result is available.
		result, err := p.collectResult(attempt)
		if err != nil {
			if err != ErrRouterShuttingDown &&
				err != htlcswitch.ErrSwitchExiting {

				log.Errorf("Error collecting result for "+
					"shard %v for payment %v: %v",
					attempt.AttemptID, p.paymentHash, err)
			}

			select {
			case p.shardErrors <- err:
			case <-p.router.quit:
			case <-p.quit:
			}
			return
		}

		// If a non-critical error was encountered handle it and mark
		// the payment failed if the failure was terminal.
		if result.err != nil {
			err := p.handleSendError(attempt, result.err)
			if err != nil {
				select {
				case p.shardErrors <- err:
				case <-p.router.quit:
				case <-p.quit:
				}
				return
			}
		}

		select {
		case p.shardErrors <- nil:
		case <-p.router.quit:
		case <-p.quit:
		}
	}()
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
		log.Debugf("Payment ID %v for hash %v not found in "+
			"the Switch, retrying.", attempt.AttemptID,
			p.paymentHash)

		attempt, cErr := p.failAttempt(attempt, err)
		if cErr != nil {
			return nil, cErr
		}

		return &shardResult{
			attempt: attempt,
			err:     err,
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

	case <-p.quit:
		return nil, fmt.Errorf("shard handler exiting")
	}

	// In case of a payment failure, fail the attempt with the control
	// tower and return.
	if result.Error != nil {
		attempt, err := p.failAttempt(attempt, result.Error)
		if err != nil {
			return nil, err
		}

		return &shardResult{
			attempt: attempt,
			err:     result.Error,
		}, nil
	}

	// We successfully got a payment result back from the switch.
	log.Debugf("Payment %v succeeded with pid=%v",
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
	htlcAttempt, err := p.router.cfg.Control.SettleAttempt(
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
		attempt: htlcAttempt,
	}, nil
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

	log.Tracef("Attempting to send payment %v (pid=%v), "+
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
			"%v to switch: %v", attempt.AttemptID,
			p.paymentHash, err)
		return err
	}

	log.Debugf("Payment %v (pid=%v) successfully sent to switch, route: %v",
		p.paymentHash, attempt.AttemptID, &attempt.Route)

	return nil
}

// handleSendError inspects the given error from the Switch and determines
// whether we should make another payment attempt, or if it should be
// considered a terminal error. Terminal errors will be recorded with the
// control tower.
func (p *shardHandler) handleSendError(attempt *channeldb.HTLCAttemptInfo,
	sendErr error) error {

	reason := p.router.processSendError(
		attempt.AttemptID, &attempt.Route, sendErr,
	)
	if reason == nil {
		return nil
	}

	log.Debugf("Payment %v failed: final_outcome=%v, raw_err=%v",
		p.paymentHash, *reason, sendErr)

	err := p.router.cfg.Control.Fail(p.paymentHash, *reason)
	if err != nil {
		return err
	}

	return nil
}

// failAttempt calls control tower to fail the current payment attempt.
func (p *shardHandler) failAttempt(attempt *channeldb.HTLCAttemptInfo,
	sendError error) (*channeldb.HTLCAttempt, error) {

	log.Warnf("Attempt %v for payment %v failed: %v", attempt.AttemptID,
		p.paymentHash, sendError)

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
