package routing

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnutils"
	"github.com/lightningnetwork/lnd/lnwire"
	paymentsdb "github.com/lightningnetwork/lnd/payments/db"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/routing/shards"
	"github.com/lightningnetwork/lnd/tlv"
)

// ErrPaymentLifecycleExiting is used when waiting for htlc attempt result, but
// the payment lifecycle is exiting .
var ErrPaymentLifecycleExiting = errors.New("payment lifecycle exiting")

// switchResult is the result sent back from the switch after processing the
// HTLC.
type switchResult struct {
	// attempt is the HTLC sent to the switch.
	attempt *paymentsdb.HTLCAttempt

	// result is sent from the switch which contains either a preimage if
	// ths HTLC is settled or an error if it's failed.
	result *htlcswitch.PaymentResult
}

// paymentLifecycle holds all information about the current state of a payment
// needed to resume if from any point.
type paymentLifecycle struct {
	router                *ChannelRouter
	feeLimit              lnwire.MilliSatoshi
	identifier            lntypes.Hash
	paySession            PaymentSession
	shardTracker          shards.ShardTracker
	currentHeight         int32
	firstHopCustomRecords lnwire.CustomRecords

	// quit is closed to signal the sub goroutines of the payment lifecycle
	// to stop.
	quit chan struct{}

	// resultCollected is used to send the result returned from the switch
	// for a given HTLC attempt.
	resultCollected chan *switchResult

	// resultCollector is a function that is used to collect the result of
	// an HTLC attempt, which is always mounted to `p.collectResultAsync`
	// except in unit test, where we use a much simpler resultCollector to
	// decouple the test flow for the payment lifecycle.
	resultCollector func(attempt *paymentsdb.HTLCAttempt)
}

// newPaymentLifecycle initiates a new payment lifecycle and returns it.
func newPaymentLifecycle(r *ChannelRouter, feeLimit lnwire.MilliSatoshi,
	identifier lntypes.Hash, paySession PaymentSession,
	shardTracker shards.ShardTracker, currentHeight int32,
	firstHopCustomRecords lnwire.CustomRecords) *paymentLifecycle {

	p := &paymentLifecycle{
		router:                r,
		feeLimit:              feeLimit,
		identifier:            identifier,
		paySession:            paySession,
		shardTracker:          shardTracker,
		currentHeight:         currentHeight,
		quit:                  make(chan struct{}),
		resultCollected:       make(chan *switchResult, 1),
		firstHopCustomRecords: firstHopCustomRecords,
	}

	// Mount the result collector.
	p.resultCollector = p.collectResultAsync

	return p
}

// calcFeeBudget returns the available fee to be used for sending HTLC
// attempts.
func (p *paymentLifecycle) calcFeeBudget(
	feesPaid lnwire.MilliSatoshi) lnwire.MilliSatoshi {

	budget := p.feeLimit

	// We'll subtract the used fee from our fee budget. In case of
	// overflow, we need to check whether feesPaid exceeds our budget
	// already.
	if feesPaid <= budget {
		budget -= feesPaid
	} else {
		budget = 0
	}

	return budget
}

// stateStep defines an action to be taken in our payment lifecycle. We either
// quit, continue, or exit the lifecycle, see details below.
type stateStep uint8

const (
	// stepSkip is used when we need to skip the current lifecycle and jump
	// to the next one.
	stepSkip stateStep = iota

	// stepProceed is used when we can proceed the current lifecycle.
	stepProceed

	// stepExit is used when we need to quit the current lifecycle.
	stepExit
)

// decideNextStep is used to determine the next step in the payment lifecycle.
// It first checks whether the current state of the payment allows more HTLC
// attempts to be made. If allowed, it will return so the lifecycle can continue
// making new attempts. Otherwise, it checks whether we need to wait for the
// results of already sent attempts. If needed, it will block until one of the
// results is sent back. then process its result here. When there's no need to
// wait for results, the method will exit with `stepExit` such that the payment
// lifecycle loop will terminate.
func (p *paymentLifecycle) decideNextStep(ctx context.Context,
	payment paymentsdb.DBMPPayment) (stateStep, error) {

	// Check whether we could make new HTLC attempts.
	allow, err := payment.AllowMoreAttempts()
	if err != nil {
		return stepExit, err
	}

	// Exit early we need to make more attempts.
	if allow {
		return stepProceed, nil
	}

	// We cannot make more attempts, we now check whether we need to wait
	// for results.
	wait, err := payment.NeedWaitAttempts()
	if err != nil {
		return stepExit, err
	}

	// If we are not allowed to make new HTLC attempts and there's no need
	// to wait, the lifecycle is done and we can exit.
	if !wait {
		return stepExit, nil
	}

	log.Tracef("Waiting for attempt results for payment %v", p.identifier)

	// Otherwise we wait for the result for one HTLC attempt then continue
	// the lifecycle.
	select {
	case r := <-p.resultCollected:
		log.Tracef("Received attempt result for payment %v",
			p.identifier)

		// Handle the result here. If there's no error, we will return
		// stepSkip and move to the next lifecycle iteration, which will
		// refresh the payment and wait for the next attempt result, if
		// any.
		_, err := p.handleAttemptResult(ctx, r.attempt, r.result)

		// We would only get a DB-related error here, which will cause
		// us to abort the payment flow.
		if err != nil {
			return stepExit, err
		}

	case <-p.quit:
		return stepExit, ErrPaymentLifecycleExiting

	case <-p.router.quit:
		return stepExit, ErrRouterShuttingDown
	}

	return stepSkip, nil
}

// resumePayment resumes the paymentLifecycle from the current state.
func (p *paymentLifecycle) resumePayment(ctx context.Context) ([32]byte,
	*route.Route, error) {

	// We need to make sure we can still do db operations after the context
	// is cancelled.
	//
	// TODO(ziggie): This is a workaround to avoid a greater refactor of the
	// payment lifecycle. We can currently not rely on the parent context
	// because this method is also collecting the results of inflight HTLCs
	// after the context is cancelled. So we need to make sure we only use
	// the current context to stop creating new attempts but use this
	// cleanupCtx to do all the db operations.
	cleanupCtx := context.WithoutCancel(ctx)

	// When the payment lifecycle loop exits, we make sure to signal any
	// sub goroutine of the HTLC attempt to exit, then wait for them to
	// return.
	defer p.stop()

	// If we had any existing attempts outstanding, we'll start by spinning
	// up goroutines that'll collect their results and deliver them to the
	// lifecycle loop below.
	payment, err := p.reloadInflightAttempts(ctx)
	if err != nil {
		return [32]byte{}, nil, err
	}

	// Get the payment status.
	status := payment.GetStatus()

	// exitWithErr is a helper closure that logs and returns an error.
	exitWithErr := func(err error) ([32]byte, *route.Route, error) {
		// Log an error with the latest payment status.
		//
		// NOTE: this `status` variable is reassigned in the loop
		// below. We could also call `payment.GetStatus` here, but in a
		// rare case when the critical log is triggered when using
		// postgres as db backend, the `payment` could be nil, causing
		// the payment fetching to return an error.
		log.Errorf("Payment %v with status=%v failed: %v", p.identifier,
			status, err)

		return [32]byte{}, nil, err
	}

	// We'll continue until either our payment succeeds, or we encounter a
	// critical error during path finding.
lifecycle:
	for {
		// Before we attempt any new shard, we'll check to see if we've
		// gone past the payment attempt timeout or if the context was
		// canceled. If the context is done, the payment is marked as
		// failed and we reload the latest payment state to reflect
		// this.
		//
		// NOTE: This can be called several times if there are more
		// attempts to be resolved after the timeout or context is
		// cancelled.
		if err := p.checkContext(ctx); err != nil {
			return exitWithErr(err)
		}

		// We update the payment state on every iteration.
		currentPayment, ps, err := p.reloadPayment(cleanupCtx)
		if err != nil {
			return exitWithErr(err)
		}

		// Reassign status so it can be read in `exitWithErr`.
		status = currentPayment.GetStatus()

		// Reassign payment such that when the lifecycle exits, the
		// latest payment can be read when we access its terminal info.
		payment = currentPayment

		// We now proceed our lifecycle with the following tasks in
		// order,
		//   1. request route.
		//   2. create HTLC attempt.
		//   3. send HTLC attempt.
		//   4. collect HTLC attempt result.
		//

		// Now decide the next step of the current lifecycle.
		step, err := p.decideNextStep(cleanupCtx, payment)
		if err != nil {
			return exitWithErr(err)
		}

		switch step {
		// Exit the for loop and return below.
		case stepExit:
			break lifecycle

		// Continue the for loop and skip the rest.
		case stepSkip:
			continue lifecycle

		// Continue the for loop and proceed the rest.
		case stepProceed:

		// Unknown step received, exit with an error.
		default:
			err = fmt.Errorf("unknown step: %v", step)
			return exitWithErr(err)
		}

		// Now request a route to be used to create our HTLC attempt.
		rt, err := p.requestRoute(cleanupCtx, ps)
		if err != nil {
			return exitWithErr(err)
		}

		// We may not be able to find a route for current attempt. In
		// that case, we continue the loop and move straight to the
		// next iteration in case there are results for inflight HTLCs
		// that still need to be collected.
		if rt == nil {
			log.Errorf("No route found for payment %v",
				p.identifier)

			continue lifecycle
		}

		log.Tracef("Found route: %s", lnutils.SpewLogClosure(rt.Hops))

		// We found a route to try, create a new HTLC attempt to try.
		attempt, err := p.registerAttempt(
			cleanupCtx, rt, ps.RemainingAmt,
		)
		if err != nil {
			return exitWithErr(err)
		}

		// Once the attempt is created, send it to the htlcswitch.
		result, err := p.sendAttempt(cleanupCtx, attempt)
		if err != nil {
			return exitWithErr(err)
		}

		// Now that the shard was successfully sent, launch a go
		// routine that will handle its result when its back.
		if result.err == nil {
			p.resultCollector(attempt)
		}
	}

	// Once we are out the lifecycle loop, it means we've reached a
	// terminal condition. We either return the settled preimage or the
	// payment's failure reason.
	//
	// Optionally delete the failed attempts from the database. If we are
	// configured to keep failed payment attempts, we skip deletion.
	if !p.router.cfg.KeepFailedPaymentAttempts {
		err = p.router.cfg.Control.DeleteFailedAttempts(
			cleanupCtx, p.identifier,
		)
		if err != nil {
			log.Errorf("Error deleting failed htlc attempts "+
				"for payment %v: %v", p.identifier, err)
		}
	}

	htlc, failure := payment.TerminalInfo()
	if htlc != nil {
		return htlc.Settle.Preimage, &htlc.Route, nil
	}

	// Otherwise return the payment failure reason.
	return [32]byte{}, nil, *failure
}

// checkContext checks whether the payment context has been canceled.
// Cancellation occurs manually or if the context times out.
func (p *paymentLifecycle) checkContext(ctx context.Context) error {
	select {
	case <-ctx.Done():
		// If the context was canceled, we'll mark the payment as
		// failed. There are two cases to distinguish here: Either a
		// user-provided timeout was reached, or the context was
		// canceled, either to a manual cancellation or due to an
		// unknown error.
		var reason paymentsdb.FailureReason
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			reason = paymentsdb.FailureReasonTimeout
			log.Warnf("Payment attempt not completed before "+
				"context timeout, id=%s", p.identifier.String())
		} else {
			reason = paymentsdb.FailureReasonCanceled
			log.Warnf("Payment attempt context canceled, id=%s",
				p.identifier.String())
		}

		// The context is already cancelled at this point, so we create
		// a new context so the payment can successfully be marked as
		// failed.
		cleanupCtx := context.WithoutCancel(ctx)

		// By marking the payment failed, depending on whether it has
		// inflight HTLCs or not, its status will now either be
		// `StatusInflight` or `StatusFailed`. In either case, no more
		// HTLCs will be attempted.
		err := p.router.cfg.Control.FailPayment(
			cleanupCtx, p.identifier, reason,
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
func (p *paymentLifecycle) requestRoute(ctx context.Context,
	ps *paymentsdb.MPPaymentState) (*route.Route, error) {

	remainingFees := p.calcFeeBudget(ps.FeesPaid)

	// Query our payment session to construct a route.
	rt, err := p.paySession.RequestRoute(
		ps.RemainingAmt, remainingFees,
		uint32(ps.NumAttemptsInFlight), uint32(p.currentHeight),
		p.firstHopCustomRecords,
	)

	// Exit early if there's no error.
	if err == nil {
		// Allow the traffic shaper to add custom records to the
		// outgoing HTLC and also adjust the amount if needed.
		err = p.amendFirstHopData(rt)
		if err != nil {
			return nil, err
		}

		return rt, nil
	}

	// Otherwise we need to handle the error.
	log.Warnf("Failed to find route for payment %v: %v", p.identifier, err)

	// If the error belongs to `noRouteError` set, it means a non-critical
	// error has happened during path finding, and we will mark the payment
	// failed with this reason. Otherwise, we'll return the critical error
	// found to abort the lifecycle.
	var routeErr noRouteError
	if !errors.As(err, &routeErr) {
		return nil, fmt.Errorf("requestRoute got: %w", err)
	}

	// It's the `paymentSession`'s responsibility to find a route for us
	// with the best effort. When it cannot find a path, we need to treat it
	// as a terminal condition and fail the payment no matter it has
	// inflight HTLCs or not.
	failureCode := routeErr.FailureReason()
	log.Warnf("Marking payment %v permanently failed with no route: %v",
		p.identifier, failureCode)

	err = p.router.cfg.Control.FailPayment(
		ctx, p.identifier, failureCode,
	)
	if err != nil {
		return nil, fmt.Errorf("FailPayment got: %w", err)
	}

	// NOTE: we decide to not return the non-critical noRouteError here to
	// avoid terminating the payment lifecycle as there might be other
	// inflight HTLCs which we must wait for their results.
	return nil, nil
}

// stop signals any active shard goroutine to exit.
func (p *paymentLifecycle) stop() {
	close(p.quit)
}

// attemptResult holds the HTLC attempt and a possible error returned from
// sending it.
type attemptResult struct {
	// err is non-nil if a non-critical error was encountered when trying
	// to send the attempt, and we successfully updated the control tower
	// to reflect this error. This can be errors like not enough local
	// balance for the given route etc.
	err error

	// attempt is the attempt structure as recorded in the database.
	attempt *paymentsdb.HTLCAttempt
}

// collectResultAsync launches a goroutine that will wait for the result of the
// given HTLC attempt to be available then save its result in a map. Once
// received, it will send the result returned from the switch to channel
// `resultCollected`.
func (p *paymentLifecycle) collectResultAsync(attempt *paymentsdb.HTLCAttempt) {
	log.Debugf("Collecting result for attempt %v in payment %v",
		attempt.AttemptID, p.identifier)

	go func() {
		result, err := p.collectResult(attempt)
		if err != nil {
			log.Errorf("Error collecting result for attempt %v in "+
				"payment %v: %v", attempt.AttemptID,
				p.identifier, err)

			return
		}

		log.Debugf("Result collected for attempt %v in payment %v",
			attempt.AttemptID, p.identifier)

		// Create a switch result and send it to the resultCollected
		// chan, which gets processed when the lifecycle is waiting for
		// a result to be received in decideNextStep.
		r := &switchResult{
			attempt: attempt,
			result:  result,
		}

		// Signal that a result has been collected.
		select {
		// Send the result so decideNextStep can proceed.
		case p.resultCollected <- r:

		case <-p.quit:
			log.Debugf("Lifecycle exiting while collecting "+
				"result for payment %v", p.identifier)

		case <-p.router.quit:
		}
	}()
}

// collectResult waits for the result of the given HTLC attempt to be sent by
// the switch and returns it.
func (p *paymentLifecycle) collectResult(
	attempt *paymentsdb.HTLCAttempt) (*htlcswitch.PaymentResult, error) {

	log.Tracef("Collecting result for attempt %v",
		lnutils.SpewLogClosure(attempt))

	result := &htlcswitch.PaymentResult{}

	// Regenerate the circuit for this attempt.
	circuit, err := attempt.Circuit()

	// TODO(yy): We generate this circuit to create the error decryptor,
	// which is then used in htlcswitch as the deobfuscator to decode the
	// error from `UpdateFailHTLC`. However, suppose it's an
	// `UpdateFulfillHTLC` message yet for some reason the sphinx packet is
	// failed to be generated, we'd miss settling it. This means we should
	// give it a second chance to try the settlement path in case
	// `GetAttemptResult` gives us back the preimage. And move the circuit
	// creation into htlcswitch so it's only constructed when there's a
	// failure message we need to decode.
	if err != nil {
		log.Debugf("Unable to generate circuit for attempt %v: %v",
			attempt.AttemptID, err)
		return nil, err
	}

	// Using the created circuit, initialize the error decrypter, so we can
	// parse+decode any failures incurred by this payment within the
	// switch.
	errorDecryptor := &htlcswitch.SphinxErrorDecrypter{
		OnionErrorDecrypter: sphinx.NewOnionErrorDecrypter(circuit),
	}

	// Now ask the switch to return the result of the payment when
	// available.
	//
	// TODO(yy): consider using htlcswitch to create the `errorDecryptor`
	// since the htlc is already in db. This will also make the interface
	// `PaymentAttemptDispatcher` deeper and easier to use. Moreover, we'd
	// only create the decryptor when received a failure, further saving us
	// a few CPU cycles.
	resultChan, err := p.router.cfg.Payer.GetAttemptResult(
		attempt.AttemptID, p.identifier, errorDecryptor,
	)
	// Handle the switch error.
	if err != nil {
		log.Errorf("Failed getting result for attemptID %d "+
			"from switch: %v", attempt.AttemptID, err)

		result.Error = err

		return result, nil
	}

	// The switch knows about this payment, we'll wait for a result to be
	// available.
	select {
	case r, ok := <-resultChan:
		if !ok {
			return nil, htlcswitch.ErrSwitchExiting
		}

		result = r

	case <-p.quit:
		return nil, ErrPaymentLifecycleExiting

	case <-p.router.quit:
		return nil, ErrRouterShuttingDown
	}

	return result, nil
}

// registerAttempt is responsible for creating and saving an HTLC attempt in db
// by using the route info provided. The `remainingAmt` is used to decide
// whether this is the last attempt.
func (p *paymentLifecycle) registerAttempt(ctx context.Context, rt *route.Route,
	remainingAmt lnwire.MilliSatoshi) (*paymentsdb.HTLCAttempt, error) {

	// If this route will consume the last remaining amount to send
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
		ctx, p.identifier, &attempt.HTLCAttemptInfo,
	)

	return attempt, err
}

// createNewPaymentAttempt creates a new payment attempt from the given route.
func (p *paymentLifecycle) createNewPaymentAttempt(rt *route.Route,
	lastShard bool) (*paymentsdb.HTLCAttempt, error) {

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

	// If this shard carries MPP or AMP options, add them to the last hop
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
	return paymentsdb.NewHtlcAttempt(
		attemptID, sessionKey, *rt, p.router.cfg.Clock.Now(), &hash,
	)
}

// sendAttempt attempts to send the current attempt to the switch to complete
// the payment. If this attempt fails, then we'll continue on to the next
// available route.
func (p *paymentLifecycle) sendAttempt(ctx context.Context,
	attempt *paymentsdb.HTLCAttempt) (*attemptResult, error) {

	log.Debugf("Sending HTLC attempt(id=%v, total_amt=%v, first_hop_amt=%d"+
		") for payment %v", attempt.AttemptID,
		attempt.Route.TotalAmount, attempt.Route.FirstHopAmount.Val,
		p.identifier)

	rt := attempt.Route

	// Construct the first hop.
	firstHop := lnwire.NewShortChanIDFromInt(rt.Hops[0].ChannelID)

	// Craft an HTLC packet to send to the htlcswitch. The metadata within
	// this packet will be used to route the payment through the network,
	// starting with the first-hop.
	htlcAdd := &lnwire.UpdateAddHTLC{
		Amount:        rt.FirstHopAmount.Val.Int(),
		Expiry:        rt.TotalTimeLock,
		PaymentHash:   *attempt.Hash,
		CustomRecords: rt.FirstHopWireCustomRecords,
	}

	// Generate the raw encoded sphinx packet to be included along
	// with the htlcAdd message that we send directly to the
	// switch.
	onionBlob, err := attempt.OnionBlob()
	if err != nil {
		log.Errorf("Failed to create onion blob: attempt=%d in "+
			"payment=%v, err:%v", attempt.AttemptID,
			p.identifier, err)

		return p.failAttempt(ctx, attempt.AttemptID, err)
	}

	htlcAdd.OnionBlob = onionBlob

	// Send it to the Switch. When this method returns we assume
	// the Switch successfully has persisted the payment attempt,
	// such that we can resume waiting for the result after a
	// restart.
	err = p.router.cfg.Payer.SendHTLC(firstHop, attempt.AttemptID, htlcAdd)
	if err != nil {
		log.Errorf("Failed sending attempt %d for payment %v to "+
			"switch: %v", attempt.AttemptID, p.identifier, err)

		return p.handleSwitchErr(ctx, attempt, err)
	}

	log.Debugf("Attempt %v for payment %v successfully sent to switch, "+
		"route: %v", attempt.AttemptID, p.identifier, &attempt.Route)

	return &attemptResult{
		attempt: attempt,
	}, nil
}

// amendFirstHopData is a function that calls the traffic shaper to allow it to
// add custom records to the outgoing HTLC and also adjust the amount if
// needed.
func (p *paymentLifecycle) amendFirstHopData(rt *route.Route) error {
	// The first hop amount on the route is the full route amount if not
	// overwritten by the traffic shaper. So we set the initial value now
	// and potentially overwrite it later.
	rt.FirstHopAmount = tlv.NewRecordT[tlv.TlvType0](
		tlv.NewBigSizeT(rt.TotalAmount),
	)

	// By default, we set the first hop custom records to the initial
	// value requested by the RPC. The traffic shaper may overwrite this
	// value.
	rt.FirstHopWireCustomRecords = p.firstHopCustomRecords

	if len(rt.Hops) == 0 {
		return fmt.Errorf("cannot amend first hop data, route length " +
			"is zero")
	}

	firstHopPK := rt.Hops[0].PubKeyBytes

	// extraDataRequest is a helper struct to pass the custom records and
	// amount back from the traffic shaper.
	type extraDataRequest struct {
		customRecords fn.Option[lnwire.CustomRecords]

		amount fn.Option[lnwire.MilliSatoshi]
	}

	// If a hook exists that may affect our outgoing message, we call it now
	// and apply its side effects to the UpdateAddHTLC message.
	result, err := fn.MapOptionZ(
		p.router.cfg.TrafficShaper,
		//nolint:ll
		func(ts htlcswitch.AuxTrafficShaper) fn.Result[extraDataRequest] {
			newAmt, newRecords, err := ts.ProduceHtlcExtraData(
				rt.TotalAmount, p.firstHopCustomRecords,
				firstHopPK,
			)
			if err != nil {
				return fn.Err[extraDataRequest](err)
			}

			// Make sure we only received valid records.
			if err := newRecords.Validate(); err != nil {
				return fn.Err[extraDataRequest](err)
			}

			log.Debugf("Aux traffic shaper returned custom "+
				"records %v and amount %d msat for HTLC",
				lnutils.SpewLogClosure(newRecords), newAmt)

			return fn.Ok(extraDataRequest{
				customRecords: fn.Some(newRecords),
				amount:        fn.Some(newAmt),
			})
		},
	).Unpack()
	if err != nil {
		return fmt.Errorf("traffic shaper failed to produce extra "+
			"data: %w", err)
	}

	// Apply the side effects to the UpdateAddHTLC message.
	result.customRecords.WhenSome(func(records lnwire.CustomRecords) {
		rt.FirstHopWireCustomRecords = records
	})
	result.amount.WhenSome(func(amount lnwire.MilliSatoshi) {
		rt.FirstHopAmount = tlv.NewRecordT[tlv.TlvType0](
			tlv.NewBigSizeT(amount),
		)
	})

	return nil
}

// failAttemptAndPayment fails both the payment and its attempt via the
// router's control tower, which marks the payment as failed in db.
func (p *paymentLifecycle) failPaymentAndAttempt(ctx context.Context,
	attemptID uint64, reason *paymentsdb.FailureReason,
	sendErr error) (*attemptResult, error) {

	log.Errorf("Payment %v failed: final_outcome=%v, raw_err=%v",
		p.identifier, *reason, sendErr)

	// Fail the payment via control tower.
	//
	// NOTE: we must fail the payment first before failing the attempt.
	// Otherwise, once the attempt is marked as failed, another goroutine
	// might make another attempt while we are failing the payment.
	err := p.router.cfg.Control.FailPayment(
		ctx, p.identifier, *reason,
	)
	if err != nil {
		log.Errorf("Unable to fail payment: %v", err)
		return nil, err
	}

	// Fail the attempt.
	return p.failAttempt(ctx, attemptID, sendErr)
}

// handleSwitchErr inspects the given error from the Switch and determines
// whether we should make another payment attempt, or if it should be
// considered a terminal error. Terminal errors will be recorded with the
// control tower. It analyzes the sendErr for the payment attempt received from
// the switch and updates mission control and/or channel policies. Depending on
// the error type, the error is either the final outcome of the payment or we
// need to continue with an alternative route. A final outcome is indicated by
// a non-nil reason value.
func (p *paymentLifecycle) handleSwitchErr(ctx context.Context,
	attempt *paymentsdb.HTLCAttempt,
	sendErr error) (*attemptResult, error) {

	internalErrorReason := paymentsdb.FailureReasonError
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
			return p.failAttempt(ctx, attemptID, sendErr)
		}

		// Otherwise fail both the payment and the attempt.
		return p.failPaymentAndAttempt(ctx, attemptID, reason, sendErr)
	}

	// If this attempt ID is unknown to the Switch, it means it was never
	// checkpointed and forwarded by the switch before a restart. In this
	// case we can safely send a new payment attempt, and wait for its
	// result to be available.
	if errors.Is(sendErr, htlcswitch.ErrPaymentIDNotFound) {
		log.Warnf("Failing attempt=%v for payment=%v as it's not "+
			"found in the Switch", attempt.AttemptID, p.identifier)

		return p.failAttempt(ctx, attemptID, sendErr)
	}

	if errors.Is(sendErr, htlcswitch.ErrUnreadableFailureMessage) {
		log.Warn("Unreadable failure when sending htlc: id=%v, hash=%v",
			attempt.AttemptID, attempt.Hash)

		// Since this error message cannot be decrypted, we will send a
		// nil error message to our mission controller and fail the
		// payment.
		return reportAndFail(nil, nil)
	}

	// If the error is a ClearTextError, we have received a valid wire
	// failure message, either from our own outgoing link or from a node
	// down the route. If the error is not related to the propagation of
	// our payment, we can stop trying because an internal error has
	// occurred.
	var rtErr htlcswitch.ClearTextError
	ok := errors.As(sendErr, &rtErr)
	if !ok {
		return p.failPaymentAndAttempt(
			ctx, attemptID, &internalErrorReason, sendErr,
		)
	}

	// failureSourceIdx is the index of the node that the failure occurred
	// at. If the ClearTextError received is not a ForwardingError the
	// payment error occurred at our node, so we leave this value as 0
	// to indicate that the failure occurred locally. If the error is a
	// ForwardingError, it did not originate at our node, so we set
	// failureSourceIdx to the index of the node where the failure occurred.
	failureSourceIdx := 0
	var source *htlcswitch.ForwardingError
	ok = errors.As(rtErr, &source)
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
		return p.failPaymentAndAttempt(
			ctx, attemptID, &internalErrorReason, sendErr,
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
	// implementation. Therefore, return an error.
	errVertex := rt.Hops[errorSourceIdx-1].PubKeyBytes
	errSource, err := btcec.ParsePubKey(errVertex[:])
	if err != nil {
		log.Errorf("Cannot parse pubkey: idx=%v, pubkey=%v",
			errorSourceIdx, errVertex)

		return err
	}

	var (
		isAdditionalEdge bool
		policy           *models.CachedEdgePolicy
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
	if !p.router.cfg.ApplyChannelUpdate(update) {
		log.Debugf("Invalid channel update received: node=%v",
			errVertex)
	}
	return nil
}

// failAttempt calls control tower to fail the current payment attempt.
func (p *paymentLifecycle) failAttempt(ctx context.Context, attemptID uint64,
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
		ctx, p.identifier, attemptID, failInfo,
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
func marshallError(sendError error, time time.Time) *paymentsdb.HTLCFailInfo {
	response := &paymentsdb.HTLCFailInfo{
		FailTime: time,
	}

	switch {
	case errors.Is(sendError, htlcswitch.ErrPaymentIDNotFound):
		response.Reason = paymentsdb.HTLCFailInternal
		return response

	case errors.Is(sendError, htlcswitch.ErrUnreadableFailureMessage):
		response.Reason = paymentsdb.HTLCFailUnreadable
		return response
	}

	var rtErr htlcswitch.ClearTextError
	ok := errors.As(sendError, &rtErr)
	if !ok {
		response.Reason = paymentsdb.HTLCFailInternal
		return response
	}

	message := rtErr.WireMessage()
	if message != nil {
		response.Reason = paymentsdb.HTLCFailMessage
		response.Message = message
	} else {
		response.Reason = paymentsdb.HTLCFailUnknown
	}

	// If the ClearTextError received is a ForwardingError, the error
	// originated from a node along the route, not locally on our outgoing
	// link. We set failureSourceIdx to the index of the node where the
	// failure occurred. If the error is not a ForwardingError, the failure
	// occurred at our node, so we leave the index as 0 to indicate that
	// we failed locally.
	var fErr *htlcswitch.ForwardingError
	ok = errors.As(rtErr, &fErr)
	if ok {
		response.FailureSourceIndex = uint32(fErr.FailureSourceIdx)
	}

	return response
}

// patchLegacyPaymentHash will make a copy of the passed attempt and sets its
// Hash field to be the payment hash if it's nil.
//
// NOTE: For legacy payments, which were created before the AMP feature was
// enabled, the `Hash` field in their HTLC attempts is nil. In that case, we use
// the payment hash as the `attempt.Hash` as they are identical.
func (p *paymentLifecycle) patchLegacyPaymentHash(
	a paymentsdb.HTLCAttempt) paymentsdb.HTLCAttempt {

	// Exit early if this is not a legacy attempt.
	if a.Hash != nil {
		return a
	}

	// Log a warning if the user is still using legacy payments, which has
	// weaker support.
	log.Warnf("Found legacy htlc attempt %v in payment %v", a.AttemptID,
		p.identifier)

	// Set the attempt's hash to be the payment hash, which is the payment's
	// `PaymentHash`` in the `PaymentCreationInfo`. For legacy payments
	// before AMP feature, the `Hash` field was not set so we use the
	// payment hash instead.
	//
	// NOTE: During the router's startup, we have a similar logic in
	// `resumePayments`, in which we will use the payment hash instead if
	// the attempt's hash is nil.
	a.Hash = &p.identifier

	return a
}

// reloadInflightAttempts is called when the payment lifecycle is resumed after
// a restart. It reloads all inflight attempts from the control tower and
// collects the results of the attempts that have been sent before.
func (p *paymentLifecycle) reloadInflightAttempts(
	ctx context.Context) (paymentsdb.DBMPPayment, error) {

	payment, err := p.router.cfg.Control.FetchPayment(ctx, p.identifier)
	if err != nil {
		return nil, err
	}

	for _, a := range payment.InFlightHTLCs() {
		a := a

		log.Infof("Resuming HTLC attempt %v for payment %v",
			a.AttemptID, p.identifier)

		// Potentially attach the payment hash to the `Hash` field if
		// it's a legacy payment.
		a = p.patchLegacyPaymentHash(a)

		p.resultCollector(&a)
	}

	return payment, nil
}

// reloadPayment returns the latest payment found in the db (control tower).
func (p *paymentLifecycle) reloadPayment(
	ctx context.Context) (paymentsdb.DBMPPayment,
	*paymentsdb.MPPaymentState, error) {

	// Read the db to get the latest state of the payment.
	payment, err := p.router.cfg.Control.FetchPayment(ctx, p.identifier)
	if err != nil {
		return nil, nil, err
	}

	ps := payment.GetState()
	remainingFees := p.calcFeeBudget(ps.FeesPaid)

	log.Debugf("Payment %v: status=%v, active_shards=%v, rem_value=%v, "+
		"fee_limit=%v", p.identifier, payment.GetStatus(),
		ps.NumAttemptsInFlight, ps.RemainingAmt, remainingFees)

	return payment, ps, nil
}

// handleAttemptResult processes the result of an HTLC attempt returned from
// the htlcswitch.
func (p *paymentLifecycle) handleAttemptResult(ctx context.Context,
	attempt *paymentsdb.HTLCAttempt,
	result *htlcswitch.PaymentResult) (*attemptResult, error) {

	// If the result has an error, we need to further process it by failing
	// the attempt and maybe fail the payment.
	if result.Error != nil {
		return p.handleSwitchErr(ctx, attempt, result.Error)
	}

	// We got an attempt settled result back from the switch.
	log.Debugf("Payment(%v): attempt(%v) succeeded", p.identifier,
		attempt.AttemptID)

	// Report success to mission control.
	err := p.router.cfg.MissionControl.ReportPaymentSuccess(
		attempt.AttemptID, &attempt.Route,
	)
	if err != nil {
		log.Errorf("Error reporting payment success to mc: %v", err)
	}

	// In case of success we atomically store settle result to the DB and
	// move the shard to the settled state.
	htlcAttempt, err := p.router.cfg.Control.SettleAttempt(
		ctx, p.identifier, attempt.AttemptID,
		&paymentsdb.HTLCSettleInfo{
			Preimage:   result.Preimage,
			SettleTime: p.router.cfg.Clock.Now(),
		},
	)
	if err != nil {
		log.Errorf("Error settling attempt %v for payment %v with "+
			"preimage %v: %v", attempt.AttemptID, p.identifier,
			result.Preimage, err)

		// We won't mark the attempt as failed since we already have
		// the preimage.
		return nil, err
	}

	return &attemptResult{
		attempt: htlcAttempt,
	}, nil
}

// collectAndHandleResult waits for the result for the given attempt to be
// available from the Switch, then records the attempt outcome with the control
// tower. An attemptResult is returned, indicating the final outcome of this
// HTLC attempt.
func (p *paymentLifecycle) collectAndHandleResult(ctx context.Context,
	attempt *paymentsdb.HTLCAttempt) (*attemptResult, error) {

	result, err := p.collectResult(attempt)
	if err != nil {
		return nil, err
	}

	return p.handleAttemptResult(ctx, attempt, result)
}
