package routing

import (
	"fmt"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/davecgh/go-spew/spew"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/routing/shards"
)

// errShardHandlerExiting is returned from the shardHandler when it exits.
var errShardHandlerExiting = fmt.Errorf("shard handler exiting")

// paymentLifecycle holds all information about the current state of a payment
// needed to resume if from any point.
type paymentLifecycle struct {
	router        *ChannelRouter
	feeLimit      lnwire.MilliSatoshi
	identifier    lntypes.Hash
	paySession    PaymentSession
	shardTracker  shards.ShardTracker
	timeoutChan   <-chan time.Time
	currentHeight int32
}

// payemntState holds a number of key insights learned from a given MPPayment
// that we use to determine what to do on each payment loop iteration.
type paymentState struct {
	numShardsInFlight int
	remainingAmt      lnwire.MilliSatoshi
	remainingFees     lnwire.MilliSatoshi

	// terminate indicates the payment is in its final stage and no more
	// shards should be launched. This value is true if we have an HTLC
	// settled or the payment has an error.
	terminate bool
}

// terminated returns a bool to indicate there are no further actions needed
// and we should return what we have, either the payment preimage or the
// payment error.
func (ps paymentState) terminated() bool {
	// If the payment is in final stage and we have no in flight shards to
	// wait result for, we consider the whole action terminated.
	return ps.terminate && ps.numShardsInFlight == 0
}

// needWaitForShards returns a bool to specify whether we need to wait for the
// outcome of the shardHandler.
func (ps paymentState) needWaitForShards() bool {
	// If we have in flight shards and the payment is in final stage, we
	// need to wait for the outcomes from the shards. Or if we have no more
	// money to be sent, we need to wait for the already launched shards.
	if ps.numShardsInFlight == 0 {
		return false
	}
	return ps.terminate || ps.remainingAmt == 0
}

// fetchPaymentState will query the db for the latest payment state
// information we need to act on every iteration of the payment loop and update
// the paymentState.
func (p *paymentLifecycle) fetchPaymentState() (*channeldb.MPPayment,
	*paymentState, error) {

	// Fetch the latest payment from db.
	payment, err := p.router.cfg.Control.FetchPayment(p.identifier)
	if err != nil {
		return nil, nil, err
	}

	// Fetch the total amount and fees that has already been sent in
	// settled and still in-flight shards.
	sentAmt, fees := payment.SentAmt()

	// Sanity check we haven't sent a value larger than the payment amount.
	totalAmt := payment.Info.Value
	if sentAmt > totalAmt {
		return nil, nil, fmt.Errorf("amount sent %v exceeds "+
			"total amount %v", sentAmt, totalAmt)
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

	// Update the payment state.
	state := &paymentState{
		numShardsInFlight: len(payment.InFlightHTLCs()),
		remainingAmt:      totalAmt - sentAmt,
		remainingFees:     feeBudget,
		terminate:         terminate,
	}

	return payment, state, nil
}

// resumePayment resumes the paymentLifecycle from the current state.
func (p *paymentLifecycle) resumePayment() ([32]byte, *route.Route, error) {
	shardHandler := &shardHandler{
		router:       p.router,
		identifier:   p.identifier,
		shardTracker: p.shardTracker,
		shardErrors:  make(chan error),
		quit:         make(chan struct{}),
		paySession:   p.paySession,
	}

	// When the payment lifecycle loop exits, we make sure to signal any
	// sub goroutine of the shardHandler to exit, then wait for them to
	// return.
	defer shardHandler.stop()

	// If we had any existing attempts outstanding, we'll start by spinning
	// up goroutines that'll collect their results and deliver them to the
	// lifecycle loop below.
	payment, _, err := p.fetchPaymentState()
	if err != nil {
		return [32]byte{}, nil, err
	}

	for _, a := range payment.InFlightHTLCs() {
		a := a

		log.Infof("Resuming payment shard %v for payment %v",
			a.AttemptID, p.identifier)

		shardHandler.collectResultAsync(&a.HTLCAttemptInfo)
	}

	// We'll continue until either our payment succeeds, or we encounter a
	// critical error during path finding.
lifecycle:
	for {
		// Start by quickly checking if there are any outcomes already
		// available to handle before we reevaluate our state.
		if err := shardHandler.checkShards(); err != nil {
			return [32]byte{}, nil, err
		}

		// We update the payment state on every iteration. Since the
		// payment state is affected by multiple goroutines (ie,
		// collectResultAsync), it is NOT guaranteed that we always
		// have the latest state here. This is fine as long as the
		// state is consistent as a whole.
		payment, currentState, err := p.fetchPaymentState()
		if err != nil {
			return [32]byte{}, nil, err
		}

		log.Debugf("Payment %v in state terminate=%v, "+
			"active_shards=%v, rem_value=%v, fee_limit=%v",
			p.identifier, currentState.terminate,
			currentState.numShardsInFlight,
			currentState.remainingAmt, currentState.remainingFees,
		)

		// TODO(yy): sanity check all the states to make sure
		// everything is expected.
		switch {
		// We have a terminal condition and no active shards, we are
		// ready to exit.
		case currentState.terminated():
			// Find the first successful shard and return
			// the preimage and route.
			for _, a := range payment.HTLCs {
				if a.Settle == nil {
					continue
				}

				err := p.router.cfg.Control.DeleteFailedAttempts(
					p.identifier,
				)
				if err != nil {
					log.Errorf("Error deleting failed "+
						"payment attempts for "+
						"payment %v: %v", p.identifier,
						err)
				}

				return a.Settle.Preimage, &a.Route, nil
			}

			// Payment failed.
			return [32]byte{}, nil, *payment.FailureReason

		// If we either reached a terminal error condition (but had
		// active shards still) or there is no remaining value to send,
		// we'll wait for a shard outcome.
		case currentState.needWaitForShards():
			// We still have outstanding shards, so wait for a new
			// outcome to be available before re-evaluating our
			// state.
			if err := shardHandler.waitForShard(); err != nil {
				return [32]byte{}, nil, err
			}
			continue lifecycle
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
			saveErr := p.router.cfg.Control.FailPayment(
				p.identifier, channeldb.FailureReasonTimeout,
			)
			if saveErr != nil {
				return [32]byte{}, nil, saveErr
			}

			continue lifecycle

		case <-p.router.quit:
			return [32]byte{}, nil, ErrRouterShuttingDown

		// Fall through if we haven't hit our time limit.
		default:
		}

		// Create a new payment attempt from the given payment session.
		rt, err := p.paySession.RequestRoute(
			currentState.remainingAmt, currentState.remainingFees,
			uint32(currentState.numShardsInFlight),
			uint32(p.currentHeight),
		)
		if err != nil {
			log.Warnf("Failed to find route for payment %v: %v",
				p.identifier, err)

			routeErr, ok := err.(noRouteError)
			if !ok {
				return [32]byte{}, nil, err
			}

			// There is no route to try, and we have no active
			// shards. This means that there is no way for us to
			// send the payment, so mark it failed with no route.
			if currentState.numShardsInFlight == 0 {
				failureCode := routeErr.FailureReason()
				log.Debugf("Marking payment %v permanently "+
					"failed with no route: %v",
					p.identifier, failureCode)

				saveErr := p.router.cfg.Control.FailPayment(
					p.identifier, failureCode,
				)
				if saveErr != nil {
					return [32]byte{}, nil, saveErr
				}

				continue lifecycle
			}

			// We still have active shards, we'll wait for an
			// outcome to be available before retrying.
			if err := shardHandler.waitForShard(); err != nil {
				return [32]byte{}, nil, err
			}
			continue lifecycle
		}

		log.Tracef("Found route: %s", spew.Sdump(rt.Hops))

		// If this route will consume the last remaining amount to send
		// to the receiver, this will be our last shard (for now).
		lastShard := rt.ReceiverAmt() == currentState.remainingAmt

		// We found a route to try, launch a new shard.
		attempt, outcome, err := shardHandler.launchShard(rt, lastShard)
		switch {
		// We may get a terminal error if we've processed a shard with
		// a terminal state (settled or permanent failure), while we
		// were pathfinding. We know we're in a terminal state here,
		// so we can continue and wait for our last shards to return.
		case err == channeldb.ErrPaymentTerminal:
			log.Infof("Payment %v in terminal state, abandoning "+
				"shard", p.identifier)

			continue lifecycle

		case err != nil:
			return [32]byte{}, nil, err
		}

		// If we encountered a non-critical error when launching the
		// shard, handle it.
		if outcome.err != nil {
			log.Warnf("Failed to launch shard %v for "+
				"payment %v: %v", attempt.AttemptID,
				p.identifier, outcome.err)

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
			continue lifecycle
		}

		// Now that the shard was successfully sent, launch a go
		// routine that will handle its result when its back.
		shardHandler.collectResultAsync(attempt)
	}
}

// shardHandler holds what is necessary to send and collect the result of
// shards.
type shardHandler struct {
	identifier   lntypes.Hash
	router       *ChannelRouter
	shardTracker shards.ShardTracker
	paySession   PaymentSession

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
		return errShardHandlerExiting

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
			return errShardHandlerExiting

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
// registering it with the control tower before sending it. The lastShard
// argument should be true if this shard will consume the remainder of the
// amount to send. It returns the HTLCAttemptInfo that was created for the
// shard, along with a launchOutcome.  The launchOutcome is used to indicate
// whether the attempt was successfully sent. If the launchOutcome wraps a
// non-nil error, it means that the attempt was not sent onto the network, so
// no result will be available in the future for it.
func (p *shardHandler) launchShard(rt *route.Route,
	lastShard bool) (*channeldb.HTLCAttemptInfo, *launchOutcome, error) {

	// Using the route received from the payment session, create a new
	// shard to send.
	firstHop, htlcAdd, attempt, err := p.createNewPaymentAttempt(
		rt, lastShard,
	)
	if err != nil {
		return nil, nil, err
	}

	// Before sending this HTLC to the switch, we checkpoint the fresh
	// paymentID and route to the DB. This lets us know on startup the ID
	// of the payment that we attempted to send, such that we can query the
	// Switch for its whereabouts. The route is needed to handle the result
	// when it eventually comes back.
	err = p.router.cfg.Control.RegisterAttempt(p.identifier, attempt)
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
// given HTLC attempt to be available then handle its result. It will fail the
// payment with the control tower if a terminal error is encountered.
func (p *shardHandler) collectResultAsync(attempt *channeldb.HTLCAttemptInfo) {
	// errToSend is the error to be sent to sh.shardErrors.
	var errToSend error

	// handleResultErr is a function closure must be called using defer. It
	// finishes collecting result by updating the payment state and send
	// the error (or nil) to sh.shardErrors.
	handleResultErr := func() {
		// Send the error or quit.
		select {
		case p.shardErrors <- errToSend:
		case <-p.router.quit:
		case <-p.quit:
		}

		p.wg.Done()
	}

	p.wg.Add(1)
	go func() {
		defer handleResultErr()

		// Block until the result is available.
		result, err := p.collectResult(attempt)
		if err != nil {
			if err != ErrRouterShuttingDown &&
				err != htlcswitch.ErrSwitchExiting &&
				err != errShardHandlerExiting {

				log.Errorf("Error collecting result for "+
					"shard %v for payment %v: %v",
					attempt.AttemptID, p.identifier, err)
			}

			// Overwrite the param errToSend and return so that the
			// defer function will use the param to proceed.
			errToSend = err
			return
		}

		// If a non-critical error was encountered handle it and mark
		// the payment failed if the failure was terminal.
		if result.err != nil {
			// Overwrite the param errToSend and return so that the
			// defer function will use the param to proceed. Notice
			// that the errToSend could be nil here.
			errToSend = p.handleSendError(attempt, result.err)
			return
		}
	}()
}

// collectResult waits for the result for the given attempt to be available
// from the Switch, then records the attempt outcome with the control tower. A
// shardResult is returned, indicating the final outcome of this HTLC attempt.
func (p *shardHandler) collectResult(attempt *channeldb.HTLCAttemptInfo) (
	*shardResult, error) {

	// We'll retrieve the hash specific to this shard from the
	// shardTracker, since it will be needed to regenerate the circuit
	// below.
	hash, err := p.shardTracker.GetHash(attempt.AttemptID)
	if err != nil {
		return nil, err
	}

	// Regenerate the circuit for this attempt.
	_, circuit, err := generateSphinxPacket(
		&attempt.Route, hash[:], attempt.SessionKey(),
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
	resultChan, err := p.router.cfg.Payer.GetAttemptResult(
		attempt.AttemptID, p.identifier, errorDecryptor,
	)
	switch {
	// If this attempt ID is unknown to the Switch, it means it was never
	// checkpointed and forwarded by the switch before a restart. In this
	// case we can safely send a new payment attempt, and wait for its
	// result to be available.
	case err == htlcswitch.ErrPaymentIDNotFound:
		log.Debugf("Attempt ID %v for payment %v not found in "+
			"the Switch, retrying.", attempt.AttemptID,
			p.identifier)

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
		p.identifier, attempt.AttemptID)

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
		p.identifier, attempt.AttemptID,
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
func (p *shardHandler) createNewPaymentAttempt(rt *route.Route, lastShard bool) (
	lnwire.ShortChannelID, *lnwire.UpdateAddHTLC,
	*channeldb.HTLCAttemptInfo, error) {

	// Generate a new key to be used for this attempt.
	sessionKey, err := generateNewSessionKey()
	if err != nil {
		return lnwire.ShortChannelID{}, nil, nil, err
	}

	// We generate a new, unique payment ID that we will use for
	// this HTLC.
	attemptID, err := p.router.cfg.NextPaymentID()
	if err != nil {
		return lnwire.ShortChannelID{}, nil, nil, err
	}

	// Request a new shard from the ShardTracker. If this is an AMP
	// payment, and this is the last shard, the outstanding shards together
	// with this one will be enough for the receiver to derive all HTLC
	// preimages. If this a non-AMP payment, the ShardTracker will return a
	// simple shard with the payment's static payment hash.
	shard, err := p.shardTracker.NewShard(attemptID, lastShard)
	if err != nil {
		return lnwire.ShortChannelID{}, nil, nil, err
	}

	// It this shard carries MPP or AMP options, add them to the last hop
	// on the route.
	hop := rt.Hops[len(rt.Hops)-1]
	if shard.MPP() != nil {
		hop.MPP = shard.MPP()
	}

	if shard.AMP() != nil {
		hop.AMP = shard.AMP()
	}

	// Generate the raw encoded sphinx packet to be included along
	// with the htlcAdd message that we send directly to the
	// switch.
	hash := shard.Hash()
	onionBlob, _, err := generateSphinxPacket(rt, hash[:], sessionKey)
	if err != nil {
		return lnwire.ShortChannelID{}, nil, nil, err
	}

	// Craft an HTLC packet to send to the layer 2 switch. The
	// metadata within this packet will be used to route the
	// payment through the network, starting with the first-hop.
	htlcAdd := &lnwire.UpdateAddHTLC{
		Amount:      rt.TotalAmount,
		Expiry:      rt.TotalTimeLock,
		PaymentHash: hash,
	}
	copy(htlcAdd.OnionBlob[:], onionBlob)

	// Attempt to send this payment through the network to complete
	// the payment. If this attempt fails, then we'll continue on
	// to the next available route.
	firstHop := lnwire.NewShortChanIDFromInt(
		rt.Hops[0].ChannelID,
	)

	// We now have all the information needed to populate the current
	// attempt information.
	attempt := channeldb.NewHtlcAttemptInfo(
		attemptID, sessionKey, *rt, p.router.cfg.Clock.Now(), &hash,
	)

	return firstHop, htlcAdd, attempt, nil
}

// sendPaymentAttempt attempts to send the current attempt to the switch.
func (p *shardHandler) sendPaymentAttempt(
	attempt *channeldb.HTLCAttemptInfo, firstHop lnwire.ShortChannelID,
	htlcAdd *lnwire.UpdateAddHTLC) error {

	log.Tracef("Attempting to send payment %v (pid=%v), "+
		"using route: %v", p.identifier, attempt.AttemptID,
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
			p.identifier, err)
		return err
	}

	log.Debugf("Payment %v (pid=%v) successfully sent to switch, route: %v",
		p.identifier, attempt.AttemptID, &attempt.Route)

	return nil
}

// handleSendError inspects the given error from the Switch and determines
// whether we should make another payment attempt, or if it should be
// considered a terminal error. Terminal errors will be recorded with the
// control tower. It analyzes the sendErr for the payment attempt received from
// the switch and updates mission control and/or channel policies. Depending on
// the error type, the error is either the final outcome of the payment or we
// need to continue with an alternative route. A final outcome is indicated by
// a non-nil reason value.
func (p *shardHandler) handleSendError(attempt *channeldb.HTLCAttemptInfo,
	sendErr error) error {

	internalErrorReason := channeldb.FailureReasonError

	// failPayment is a helper closure that fails the payment via the
	// router's control tower, which marks the payment as failed in db.
	failPayment := func(reason *channeldb.FailureReason,
		sendErr error) error {

		log.Infof("Payment %v failed: final_outcome=%v, raw_err=%v",
			p.identifier, *reason, sendErr)

		// Fail the payment via control tower.
		if err := p.router.cfg.Control.FailPayment(
			p.identifier, *reason,
		); err != nil {
			log.Errorf("unable to report failure to control "+
				"tower: %v", err)

			return &internalErrorReason
		}

		return reason
	}

	// reportFail is a helper closure that reports the failure to the
	// mission control, which helps us to decide whether we want to retry
	// the payment or not. If a non nil reason is returned from mission
	// control, it will further fail the payment via control tower.
	reportFail := func(srcIdx *int, msg lnwire.FailureMessage) error {
		// Report outcome to mission control.
		reason, err := p.router.cfg.MissionControl.ReportPaymentFail(
			attempt.AttemptID, &attempt.Route, srcIdx, msg,
		)
		if err != nil {
			log.Errorf("Error reporting payment result to mc: %v",
				err)

			reason = &internalErrorReason
		}

		// Exit early if there's no reason.
		if reason == nil {
			return nil
		}

		return failPayment(reason, sendErr)
	}

	if sendErr == htlcswitch.ErrUnreadableFailureMessage {
		log.Tracef("Unreadable failure when sending htlc")

		return reportFail(nil, nil)
	}

	// If the error is a ClearTextError, we have received a valid wire
	// failure message, either from our own outgoing link or from a node
	// down the route. If the error is not related to the propagation of
	// our payment, we can stop trying because an internal error has
	// occurred.
	rtErr, ok := sendErr.(htlcswitch.ClearTextError)
	if !ok {
		return failPayment(&internalErrorReason, sendErr)
	}

	// failureSourceIdx is the index of the node that the failure occurred
	// at. If the ClearTextError received is not a ForwardingError the
	// payment error occurred at our node, so we leave this value as 0
	// to indicate that the failure occurred locally. If the error is a
	// ForwardingError, it did not originate at our node, so we set
	// failureSourceIdx to the index of the node where the failure occurred.
	failureSourceIdx := 0
	source, ok := rtErr.(*htlcswitch.ForwardingError)
	if ok {
		failureSourceIdx = source.FailureSourceIdx
	}

	// Extract the wire failure and apply channel update if it contains one.
	// If we received an unknown failure message from a node along the
	// route, the failure message will be nil.
	failureMessage := rtErr.WireMessage()
	err := p.handleFailureMessage(
		&attempt.Route, failureSourceIdx, failureMessage,
	)
	if err != nil {
		return failPayment(&internalErrorReason, sendErr)
	}

	log.Tracef("Node=%v reported failure when sending htlc",
		failureSourceIdx)

	return reportFail(&failureSourceIdx, failureMessage)
}

// handleFailureMessage tries to apply a channel update present in the failure
// message if any.
func (p *shardHandler) handleFailureMessage(rt *route.Route,
	errorSourceIdx int, failure lnwire.FailureMessage) error {

	if failure == nil {
		return nil
	}

	// It makes no sense to apply our own channel updates.
	if errorSourceIdx == 0 {
		log.Errorf("Channel update of ourselves received")

		return nil
	}

	// Extract channel update if the error contains one.
	update := p.router.extractChannelUpdate(failure)
	if update == nil {
		return nil
	}

	// Parse pubkey to allow validation of the channel update. This should
	// always succeed, otherwise there is something wrong in our
	// implementation. Therefore return an error.
	errVertex := rt.Hops[errorSourceIdx-1].PubKeyBytes
	errSource, err := btcec.ParsePubKey(errVertex[:])
	if err != nil {
		log.Errorf("Cannot parse pubkey: idx=%v, pubkey=%v",
			errorSourceIdx, errVertex)

		return err
	}

	var (
		isAdditionalEdge bool
		policy           *channeldb.CachedEdgePolicy
	)

	// Before we apply the channel update, we need to decide whether the
	// update is for additional (ephemeral) edge or normal edge stored in
	// db.
	//
	// Note: the p.paySession might be nil here if it's called inside
	// SendToRoute where there's no payment lifecycle.
	if p.paySession != nil {
		policy = p.paySession.GetAdditionalEdgePolicy(
			errSource, update.ShortChannelID.ToUint64(),
		)
		if policy != nil {
			isAdditionalEdge = true
		}
	}

	// Apply channel update to additional edge policy.
	if isAdditionalEdge {
		if !p.paySession.UpdateAdditionalEdge(
			update, errSource, policy) {

			log.Debugf("Invalid channel update received: node=%v",
				errVertex)
		}
		return nil
	}

	// Apply channel update to the channel edge policy in our db.
	if !p.router.applyChannelUpdate(update) {
		log.Debugf("Invalid channel update received: node=%v",
			errVertex)
	}
	return nil
}

// failAttempt calls control tower to fail the current payment attempt.
func (p *shardHandler) failAttempt(attempt *channeldb.HTLCAttemptInfo,
	sendError error) (*channeldb.HTLCAttempt, error) {

	log.Warnf("Attempt %v for payment %v failed: %v", attempt.AttemptID,
		p.identifier, sendError)

	failInfo := marshallError(
		sendError,
		p.router.cfg.Clock.Now(),
	)

	// Now that we are failing this payment attempt, cancel the shard with
	// the ShardTracker such that it can derive the correct hash for the
	// next attempt.
	if err := p.shardTracker.CancelShard(attempt.AttemptID); err != nil {
		return nil, err
	}

	return p.router.cfg.Control.FailAttempt(
		p.identifier, attempt.AttemptID,
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
