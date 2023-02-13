package routing

import (
	"errors"
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
var errShardHandlerExiting = errors.New("shard handler exiting")

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

	// shardErrors is a channel where errors collected by calling
	// collectResultAsync will be delivered. These results are meant to be
	// inspected by calling waitForShard or checkShards, and the channel
	// doesn't need to be initiated if the caller is using the sync
	// collectResult directly.
	// TODO(yy): delete.
	shardErrors chan error

	// quit is closed to signal the sub goroutines of the payment lifecycle
	// to stop.
	quit chan struct{}
	wg   sync.WaitGroup
}

// newPaymentLifecycle initiates a new payment lifecycle and returns it.
func newPaymentLifecycle(r *ChannelRouter, feeLimit lnwire.MilliSatoshi,
	identifier lntypes.Hash, paySession PaymentSession,
	shardTracker shards.ShardTracker, timeout time.Duration,
	currentHeight int32) *paymentLifecycle {

	p := &paymentLifecycle{
		router:        r,
		feeLimit:      feeLimit,
		identifier:    identifier,
		paySession:    paySession,
		shardTracker:  shardTracker,
		currentHeight: currentHeight,
		quit:          make(chan struct{}),
	}

	// If a timeout is specified, create a timeout channel. If no timeout is
	// specified, the channel is left nil and will never abort the payment
	// loop.
	if timeout != 0 {
		p.timeoutChan = time.After(timeout)
	}

	return p
}

// calcFeeBudget returns the available fee to be used for sending HTLC
// attempts.
func (p *paymentLifecycle) calcFeeBudget(
	feesPaid lnwire.MilliSatoshi) lnwire.MilliSatoshi {

	feeBudget := p.feeLimit
	if feesPaid <= feeBudget {
		feeBudget -= feesPaid
	} else {
		feeBudget = 0
	}

	return feeBudget
}

// resumePayment resumes the paymentLifecycle from the current state.
func (p *paymentLifecycle) resumePayment() ([32]byte, *route.Route, error) {
	// When the payment lifecycle loop exits, we make sure to signal any
	// sub goroutine of the HTLC attempt to exit, then wait for them to
	// return.
	defer p.stop()

	// If we had any existing attempts outstanding, we'll start by spinning
	// up goroutines that'll collect their results and deliver them to the
	// lifecycle loop below.
	payment, err := p.router.cfg.Control.FetchPayment(p.identifier)
	if err != nil {
		return [32]byte{}, nil, err
	}

	for _, a := range payment.InFlightHTLCs() {
		a := a

		log.Infof("Resuming payment shard %v for payment %v",
			a.AttemptID, p.identifier)

		p.collectResultAsync(&a)
	}

	// exitWithErr is a helper closure that logs and returns an error.
	exitWithErr := func(err error) ([32]byte, *route.Route, error) {
		log.Errorf("Payment %v with status=%v failed: %v",
			p.identifier, payment.GetStatus(), err)
		return [32]byte{}, nil, err
	}

	// We'll continue until either our payment succeeds, or we encounter a
	// critical error during path finding.
lifecycle:
	for {
		// Start by quickly checking if there are any outcomes already
		// available to handle before we reevaluate our state.
		if err := p.checkShards(); err != nil {
			return exitWithErr(err)
		}

		// We update the payment state on every iteration. Since the
		// payment state is affected by multiple goroutines (ie,
		// collectResultAsync), it is NOT guaranteed that we always
		// have the latest state here. This is fine as long as the
		// state is consistent as a whole.

		// Fetch the latest payment from db.
		payment, err := p.router.cfg.Control.FetchPayment(p.identifier)
		if err != nil {
			return exitWithErr(err)
		}

		ps := payment.GetState()
		remainingFees := p.calcFeeBudget(ps.FeesPaid)

		log.Debugf("Payment %v in state terminate=%v, "+
			"active_shards=%v, rem_value=%v, fee_limit=%v",
			p.identifier, payment.Terminated(),
			ps.NumAttemptsInFlight, ps.RemainingAmt, remainingFees)

		// TODO(yy): sanity check all the states to make sure
		// everything is expected.
		// We have a terminal condition and no active shards, we are
		// ready to exit.
		if payment.Terminated() {
			// Find the first successful shard and return
			// the preimage and route.
			for _, a := range payment.GetHTLCs() {
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
			return exitWithErr(*payment.GetFailureReason())
		}

		// If we either reached a terminal error condition (but had
		// active shards still) or there is no remaining value to send,
		// we'll wait for a shard outcome.
		wait, err := payment.NeedWaitAttempts()
		if err != nil {
			return exitWithErr(err)
		}

		if wait {
			// We still have outstanding shards, so wait for a new
			// outcome to be available before re-evaluating our
			// state.
			if err := p.waitForShard(); err != nil {
				return exitWithErr(err)
			}
			continue lifecycle
		}

		// Before we attempt any new shard, we'll check to see if
		// either we've gone past the payment attempt timeout, or the
		// router is exiting. In either case, we'll stop this payment
		// attempt short. If a timeout is not applicable, timeoutChan
		// will be nil.
		if err := p.checkTimeout(); err != nil {
			return exitWithErr(err)
		}

		// Now request a route to be used to create our HTLC attempt.
		rt, err := p.requestRoute(ps)
		if err != nil {
			return exitWithErr(err)
		}

		// NOTE: might cause an infinite loop, see notes in
		// `requestRoute` for details.
		if rt == nil {
			continue lifecycle
		}

		log.Tracef("Found route: %s", spew.Sdump(rt.Hops))

		// We found a route to try, create a new HTLC attempt to try.
		attempt, err := p.registerAttempt(rt, ps.RemainingAmt)
		if err != nil {
			return exitWithErr(err)
		}

		// Once the attempt is created, send it to the htlcswitch.
		result, err := p.sendAttempt(attempt)
		if err != nil {
			return exitWithErr(err)
		}

		// Now that the shard was successfully sent, launch a go
		// routine that will handle its result when its back.
		if result.err == nil {
			p.collectResultAsync(attempt)
		}
	}
}

// checkTimeout checks whether the payment has reached its timeout.
func (p *paymentLifecycle) checkTimeout() error {
	select {
	case <-p.timeoutChan:
		log.Warnf("payment attempt not completed before timeout")

		// By marking the payment failed, depending on whether it has
		// inflight HTLCs or not, its status will now either be
		// `StatusInflight` or `StatusFailed`. In either case, no more
		// HTLCs will be attempted.
		err := p.router.cfg.Control.FailPayment(
			p.identifier, channeldb.FailureReasonTimeout,
		)
		if err != nil {
			return fmt.Errorf("FailPayment got %w", err)
		}

	case <-p.router.quit:
		return fmt.Errorf("check payment timeout got: %w",
			ErrRouterShuttingDown)

	// Fall through if we haven't hit our time limit.
	default:
	}

	return nil
}

// requestRoute is responsible for finding a route to be used to create an HTLC
// attempt.
func (p *paymentLifecycle) requestRoute(
	ps *channeldb.MPPaymentState) (*route.Route, error) {

	remainingFees := p.calcFeeBudget(ps.FeesPaid)

	// Create a new payment attempt from the given payment session.
	rt, err := p.paySession.RequestRoute(
		ps.RemainingAmt, remainingFees,
		uint32(ps.NumAttemptsInFlight), uint32(p.currentHeight),
	)

	// Exit early if there's no error.
	if err == nil {
		return rt, nil
	}

	// Otherwise we need to handle the error.
	log.Warnf("Failed to find route for payment %v: %v", p.identifier, err)

	// If the error belongs to `noRouteError` set, it means a non-critical
	// error has happened during path finding and we might be able to find
	// another route during next HTLC attempt. Otherwise, we'll return the
	// critical error found.
	var routeErr noRouteError
	if !errors.As(err, &routeErr) {
		return nil, fmt.Errorf("requestRoute got: %w", err)
	}

	// There is no route to try, and we have no active shards. This means
	// that there is no way for us to send the payment, so mark it failed
	// with no route.
	//
	// NOTE: if we have zero `numShardsInFlight`, it means all the HTLC
	// attempts have failed. Otherwise, if there are still inflight
	// attempts, we might enter an infinite loop in our lifecycle if
	// there's still remaining amount since we will keep adding new HTLC
	// attempts and they all fail with `noRouteError`.
	//
	// TODO(yy): further check the error returned here. It's the
	// `paymentSession`'s responsibility to find a route for us with best
	// effort. When it cannot find a path, we need to treat it as a
	// terminal condition and fail the payment no matter it has inflight
	// HTLCs or not.
	if ps.NumAttemptsInFlight == 0 {
		failureCode := routeErr.FailureReason()
		log.Debugf("Marking payment %v permanently failed with no "+
			"route: %v", p.identifier, failureCode)

		err := p.router.cfg.Control.FailPayment(
			p.identifier, failureCode,
		)
		if err != nil {
			return nil, fmt.Errorf("FailPayment got: %w", err)
		}
	}

	return nil, nil
}

// stop signals any active shard goroutine to exit and waits for them to exit.
func (p *paymentLifecycle) stop() {
	close(p.quit)
	p.wg.Wait()
}

// waitForShard blocks until any of the outstanding shards return.
func (p *paymentLifecycle) waitForShard() error {
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
func (p *paymentLifecycle) checkShards() error {
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

// attemptResult is returned from launchShard that indicates whether the HTLC
// was successfully sent to the network.
type attemptResult struct {
	// err is non-nil if a non-critical error was encountered when trying
	// to send the attempt, and we successfully updated the control tower
	// to reflect this error. This can be errors like not enough local
	// balance for the given route etc.
	err error

	// attempt is the attempt structure as recorded in the database.
	attempt *channeldb.HTLCAttempt
}

// collectResultAsync launches a goroutine that will wait for the result of the
// given HTLC attempt to be available then handle its result. It will fail the
// payment with the control tower if a terminal error is encountered.
func (p *paymentLifecycle) collectResultAsync(attempt *channeldb.HTLCAttempt) {
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
		_, err := p.collectResult(attempt)
		if err != nil {
			log.Errorf("Error collecting result for attempt %v "+
				"in payment %v: %v", attempt.AttemptID,
				p.identifier, err)

			errToSend = err
			return
		}
	}()
}

// collectResult waits for the result for the given attempt to be available
// from the Switch, then records the attempt outcome with the control tower.
// An attemptResult is returned, indicating the final outcome of this HTLC
// attempt.
func (p *paymentLifecycle) collectResult(attempt *channeldb.HTLCAttempt) (
	*attemptResult, error) {

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
	// Handle the switch error.
	if err != nil {
		log.Errorf("Failed getting result for attemptID %d "+
			"from switch: %v", attempt.AttemptID, err)

		return p.handleSwitchErr(attempt, err)
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
		return p.handleSwitchErr(attempt, result.Error)
	}

	// We successfully got a payment result back from the switch.
	log.Debugf("Payment %v succeeded with pid=%v",
		p.identifier, attempt.AttemptID)

	// Report success to mission control.
	err = p.router.cfg.MissionControl.ReportPaymentSuccess(
		attempt.AttemptID, &attempt.Route,
	)
	if err != nil {
		log.Errorf("Error reporting payment success to mc: %v", err)
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
		log.Errorf("Unable to settle payment attempt: %v", err)
		return nil, err
	}

	return &attemptResult{
		attempt: htlcAttempt,
	}, nil
}

// registerAttempt is responsible for creating and saving an HTLC attempt in db
// by using the route info provided. The `remainingAmt` is used to decide
// whether this is the last attempt.
func (p *paymentLifecycle) registerAttempt(rt *route.Route,
	remainingAmt lnwire.MilliSatoshi) (*channeldb.HTLCAttempt, error) {

	// If this route will consume the last remeining amount to send
	// to the receiver, this will be our last shard (for now).
	isLastAttempt := rt.ReceiverAmt() == remainingAmt

	// Using the route received from the payment session, create a new
	// shard to send.
	attempt, err := p.createNewPaymentAttempt(rt, isLastAttempt)
	if err != nil {
		return nil, err
	}

	// Before sending this HTLC to the switch, we checkpoint the fresh
	// paymentID and route to the DB. This lets us know on startup the ID
	// of the payment that we attempted to send, such that we can query the
	// Switch for its whereabouts. The route is needed to handle the result
	// when it eventually comes back.
	err = p.router.cfg.Control.RegisterAttempt(
		p.identifier, &attempt.HTLCAttemptInfo,
	)

	return attempt, err
}

// createNewPaymentAttempt creates a new payment attempt from the given route.
func (p *paymentLifecycle) createNewPaymentAttempt(rt *route.Route,
	lastShard bool) (*channeldb.HTLCAttempt, error) {

	// Generate a new key to be used for this attempt.
	sessionKey, err := generateNewSessionKey()
	if err != nil {
		return nil, err
	}

	// We generate a new, unique payment ID that we will use for
	// this HTLC.
	attemptID, err := p.router.cfg.NextPaymentID()
	if err != nil {
		return nil, err
	}

	// Request a new shard from the ShardTracker. If this is an AMP
	// payment, and this is the last shard, the outstanding shards together
	// with this one will be enough for the receiver to derive all HTLC
	// preimages. If this a non-AMP payment, the ShardTracker will return a
	// simple shard with the payment's static payment hash.
	shard, err := p.shardTracker.NewShard(attemptID, lastShard)
	if err != nil {
		return nil, err
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

	hash := shard.Hash()

	// We now have all the information needed to populate the current
	// attempt information.
	attempt := channeldb.NewHtlcAttempt(
		attemptID, sessionKey, *rt, p.router.cfg.Clock.Now(), &hash,
	)

	return attempt, nil
}

// sendAttempt attempts to send the current attempt to the switch.
func (p *paymentLifecycle) sendAttempt(
	attempt *channeldb.HTLCAttempt) (*attemptResult, error) {

	log.Debugf("Attempting to send payment %v (pid=%v)", p.identifier,
		attempt.AttemptID)

	rt := attempt.Route

	// Attempt to send this payment through the network to complete
	// the payment. If this attempt fails, then we'll continue on
	// to the next available route.
	firstHop := lnwire.NewShortChanIDFromInt(rt.Hops[0].ChannelID)

	// Craft an HTLC packet to send to the layer 2 switch. The
	// metadata within this packet will be used to route the
	// payment through the network, starting with the first-hop.
	htlcAdd := &lnwire.UpdateAddHTLC{
		Amount:      rt.TotalAmount,
		Expiry:      rt.TotalTimeLock,
		PaymentHash: *attempt.Hash,
	}

	// Generate the raw encoded sphinx packet to be included along
	// with the htlcAdd message that we send directly to the
	// switch.
	onionBlob, _, err := generateSphinxPacket(
		&rt, attempt.Hash[:], attempt.SessionKey(),
	)
	if err != nil {
		log.Errorf("Failed to create onion blob: attempt=%d in "+
			"payment=%v, err:%v", attempt.AttemptID,
			p.identifier, err)

		return p.failAttempt(attempt.AttemptID, err)
	}

	copy(htlcAdd.OnionBlob[:], onionBlob)

	// Send it to the Switch. When this method returns we assume
	// the Switch successfully has persisted the payment attempt,
	// such that we can resume waiting for the result after a
	// restart.
	err = p.router.cfg.Payer.SendHTLC(firstHop, attempt.AttemptID, htlcAdd)
	if err != nil {
		log.Errorf("Failed sending attempt %d for payment %v to "+
			"switch: %v", attempt.AttemptID, p.identifier, err)

		// Handle the switch error.
		return p.handleSwitchErr(attempt, err)
	}

	log.Debugf("Attempt %v for %v successfully sent to switch, route: %v",
		attempt.AttemptID, p.identifier, &attempt.Route)

	return &attemptResult{
		attempt: attempt,
	}, nil
}

// failAttemptAndPayment fails both the payment and its attempt via the
// router's control tower, which marks the payment as failed in db.
func (p *paymentLifecycle) failPaymentAndAttempt(
	attemptID uint64, reason *channeldb.FailureReason,
	sendErr error) (*attemptResult, error) {

	log.Errorf("Payment %v failed: final_outcome=%v, raw_err=%v",
		p.identifier, *reason, sendErr)

	// Fail the payment via control tower.
	//
	// NOTE: we must fail the payment first before failing the attempt.
	// Otherwise, once the attempt is marked as failed, another goroutine
	// might make another attempt while we are failing the payment.
	err := p.router.cfg.Control.FailPayment(p.identifier, *reason)
	if err != nil {
		log.Errorf("Unable to fail payment: %v", err)
		return nil, err
	}

	// Fail the attempt.
	return p.failAttempt(attemptID, sendErr)
}

// handleSwitchErr inspects the given error from the Switch and determines
// whether we should make another payment attempt, or if it should be
// considered a terminal error. Terminal errors will be recorded with the
// control tower. It analyzes the sendErr for the payment attempt received from
// the switch and updates mission control and/or channel policies. Depending on
// the error type, the error is either the final outcome of the payment or we
// need to continue with an alternative route. A final outcome is indicated by
// a non-nil reason value.
func (p *paymentLifecycle) handleSwitchErr(attempt *channeldb.HTLCAttempt,
	sendErr error) (*attemptResult, error) {

	internalErrorReason := channeldb.FailureReasonError
	attemptID := attempt.AttemptID

	// reportAndFail is a helper closure that reports the failure to the
	// mission control, which helps us to decide whether we want to retry
	// the payment or not. If a non nil reason is returned from mission
	// control, it will further fail the payment via control tower.
	reportAndFail := func(srcIdx *int,
		msg lnwire.FailureMessage) (*attemptResult, error) {

		// Report outcome to mission control.
		reason, err := p.router.cfg.MissionControl.ReportPaymentFail(
			attemptID, &attempt.Route, srcIdx, msg,
		)
		if err != nil {
			log.Errorf("Error reporting payment result to mc: %v",
				err)

			reason = &internalErrorReason
		}

		// Fail the attempt only if there's no reason.
		if reason == nil {
			// Fail the attempt.
			return p.failAttempt(attemptID, sendErr)
		}

		// Otherwise fail both the payment and the attempt.
		return p.failPaymentAndAttempt(attemptID, reason, sendErr)
	}

	// If this attempt ID is unknown to the Switch, it means it was never
	// checkpointed and forwarded by the switch before a restart. In this
	// case we can safely send a new payment attempt, and wait for its
	// result to be available.
	if errors.Is(sendErr, htlcswitch.ErrPaymentIDNotFound) {
		log.Debugf("Attempt ID %v for payment %v not found in the "+
			"Switch, retrying.", attempt.AttemptID, p.identifier)

		return p.failAttempt(attemptID, sendErr)
	}

	if sendErr == htlcswitch.ErrUnreadableFailureMessage {
		log.Tracef("Unreadable failure when sending htlc")

		return reportAndFail(nil, nil)
	}

	// If the error is a ClearTextError, we have received a valid wire
	// failure message, either from our own outgoing link or from a node
	// down the route. If the error is not related to the propagation of
	// our payment, we can stop trying because an internal error has
	// occurred.
	rtErr, ok := sendErr.(htlcswitch.ClearTextError)
	if !ok {
		return p.failPaymentAndAttempt(
			attemptID, &internalErrorReason, sendErr,
		)
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

	// Extract the wire failure and apply channel update if it contains
	// one.  If we received an unknown failure message from a node along
	// the route, the failure message will be nil.
	failureMessage := rtErr.WireMessage()
	err := p.handleFailureMessage(
		&attempt.Route, failureSourceIdx, failureMessage,
	)
	if err != nil {
		return p.failPaymentAndAttempt(
			attemptID, &internalErrorReason, sendErr,
		)
	}

	log.Tracef("Node=%v reported failure when sending htlc",
		failureSourceIdx)

	return reportAndFail(&failureSourceIdx, failureMessage)
}

// handleFailureMessage tries to apply a channel update present in the failure
// message if any.
func (p *paymentLifecycle) handleFailureMessage(rt *route.Route,
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
func (p *paymentLifecycle) failAttempt(attemptID uint64,
	sendError error) (*attemptResult, error) {

	log.Warnf("Attempt %v for payment %v failed: %v", attemptID,
		p.identifier, sendError)

	failInfo := marshallError(
		sendError,
		p.router.cfg.Clock.Now(),
	)

	// Now that we are failing this payment attempt, cancel the shard with
	// the ShardTracker such that it can derive the correct hash for the
	// next attempt.
	if err := p.shardTracker.CancelShard(attemptID); err != nil {
		return nil, err
	}

	attempt, err := p.router.cfg.Control.FailAttempt(
		p.identifier, attemptID, failInfo,
	)
	if err != nil {
		return nil, err
	}

	return &attemptResult{
		attempt: attempt,
		err:     sendError,
	}, nil
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
