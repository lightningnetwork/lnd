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

// paymentLifecycle holds all information about the current state of a payment
// needed to resume if from any point.
type paymentLifecycle struct {
	router         *ChannelRouter
	payment        *LightningPayment
	paySession     PaymentSession
	timeoutChan    <-chan time.Time
	currentHeight  int32
	finalCLTVDelta uint16
	attempt        *channeldb.HTLCAttemptInfo
	circuit        *sphinx.Circuit
	lastError      error
}

// resumePayment resumes the paymentLifecycle from the current state.
func (p *paymentLifecycle) resumePayment() ([32]byte, *route.Route, error) {
	// We'll continue until either our payment succeeds, or we encounter a
	// critical error during path finding.
	for {

		// If this payment had no existing payment attempt, we create
		// and send one now.
		if p.attempt == nil {
			firstHop, htlcAdd, err := p.createNewPaymentAttempt()
			if err != nil {
				return [32]byte{}, nil, err
			}

			// Now that the attempt is created and checkpointed to
			// the DB, we send it.
			sendErr := p.sendPaymentAttempt(firstHop, htlcAdd)
			if sendErr != nil {
				// We must inspect the error to know whether it
				// was critical or not, to decide whether we
				// should continue trying.
				err := p.handleSendError(sendErr)
				if err != nil {
					return [32]byte{}, nil, err
				}

				// Error was handled successfully, reset the
				// attempt to indicate we want to make a new
				// attempt.
				p.attempt = nil
				continue
			}
		} else {
			// If this was a resumed attempt, we must regenerate the
			// circuit. We don't need to check for errors resulting
			// from an invalid route, because the sphinx packet has
			// been successfully generated before.
			_, c, err := generateSphinxPacket(
				&p.attempt.Route, p.payment.PaymentHash[:],
				p.attempt.SessionKey,
			)
			if err != nil {
				return [32]byte{}, nil, err
			}
			p.circuit = c
		}

		// Using the created circuit, initialize the error decrypter so we can
		// parse+decode any failures incurred by this payment within the
		// switch.
		errorDecryptor := &htlcswitch.SphinxErrorDecrypter{
			OnionErrorDecrypter: sphinx.NewOnionErrorDecrypter(p.circuit),
		}

		// Now ask the switch to return the result of the payment when
		// available.
		resultChan, err := p.router.cfg.Payer.GetPaymentResult(
			p.attempt.AttemptID, p.payment.PaymentHash, errorDecryptor,
		)
		switch {

		// If this attempt ID is unknown to the Switch, it means it was
		// never checkpointed and forwarded by the switch before a
		// restart. In this case we can safely send a new payment
		// attempt, and wait for its result to be available.
		case err == htlcswitch.ErrPaymentIDNotFound:
			log.Debugf("Payment ID %v for hash %x not found in "+
				"the Switch, retrying.", p.attempt.AttemptID,
				p.payment.PaymentHash)

			// Reset the attempt to indicate we want to make a new
			// attempt.
			p.attempt = nil
			continue

		// A critical, unexpected error was encountered.
		case err != nil:
			log.Errorf("Failed getting result for attemptID %d "+
				"from switch: %v", p.attempt.AttemptID, err)

			return [32]byte{}, nil, err
		}

		// The switch knows about this payment, we'll wait for a result
		// to be available.
		var (
			result *htlcswitch.PaymentResult
			ok     bool
		)

		select {
		case result, ok = <-resultChan:
			if !ok {
				return [32]byte{}, nil, htlcswitch.ErrSwitchExiting
			}

		case <-p.router.quit:
			return [32]byte{}, nil, ErrRouterShuttingDown
		}

		// In case of a payment failure, we use the error to decide
		// whether we should retry.
		if result.Error != nil {
			log.Errorf("Attempt to send payment %x failed: %v",
				p.payment.PaymentHash, result.Error)

			// We must inspect the error to know whether it was
			// critical or not, to decide whether we should
			// continue trying.
			if err := p.handleSendError(result.Error); err != nil {
				return [32]byte{}, nil, err
			}

			// Error was handled successfully, reset the attempt to
			// indicate we want to make a new attempt.
			p.attempt = nil
			continue
		}

		// We successfully got a payment result back from the switch.
		log.Debugf("Payment %x succeeded with pid=%v",
			p.payment.PaymentHash, p.attempt.AttemptID)

		// Report success to mission control.
		err = p.router.cfg.MissionControl.ReportPaymentSuccess(
			p.attempt.AttemptID, &p.attempt.Route,
		)
		if err != nil {
			log.Errorf("Error reporting payment success to mc: %v",
				err)
		}

		// In case of success we atomically store the db payment and
		// move the payment to the success state.
		err = p.router.cfg.Control.Success(p.payment.PaymentHash, result.Preimage)
		if err != nil {
			log.Errorf("Unable to succeed payment "+
				"attempt: %v", err)
			return [32]byte{}, nil, err
		}

		// Terminal state, return the preimage and the route
		// taken.
		return result.Preimage, &p.attempt.Route, nil
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

// createNewPaymentAttempt creates and stores a new payment attempt to the
// database.
func (p *paymentLifecycle) createNewPaymentAttempt() (lnwire.ShortChannelID,
	*lnwire.UpdateAddHTLC, error) {

	// Before we attempt this next payment, we'll check to see if either
	// we've gone past the payment attempt timeout, or the router is
	// exiting. In either case, we'll stop this payment attempt short. If a
	// timeout is not applicable, timeoutChan will be nil.
	select {
	case <-p.timeoutChan:
		// Mark the payment as failed because of the
		// timeout.
		err := p.router.cfg.Control.Fail(
			p.payment.PaymentHash, channeldb.FailureReasonTimeout,
		)
		if err != nil {
			return lnwire.ShortChannelID{}, nil, err
		}

		errStr := fmt.Sprintf("payment attempt not completed " +
			"before timeout")

		return lnwire.ShortChannelID{}, nil,
			newErr(ErrPaymentAttemptTimeout, errStr)

	case <-p.router.quit:
		// The payment will be resumed from the current state
		// after restart.
		return lnwire.ShortChannelID{}, nil, ErrRouterShuttingDown

	default:
		// Fall through if we haven't hit our time limit, or
		// are expiring.
	}

	// Create a new payment attempt from the given payment session.
	rt, err := p.paySession.RequestRoute(
		p.payment, uint32(p.currentHeight), p.finalCLTVDelta,
	)
	if err != nil {
		log.Warnf("Failed to find route for payment %x: %v",
			p.payment.PaymentHash, err)

		// Convert error to payment-level failure.
		failure := errorToPaymentFailure(err)

		// If we're unable to successfully make a payment using
		// any of the routes we've found, then mark the payment
		// as permanently failed.
		saveErr := p.router.cfg.Control.Fail(
			p.payment.PaymentHash, failure,
		)
		if saveErr != nil {
			return lnwire.ShortChannelID{}, nil, saveErr
		}

		// If there was an error already recorded for this
		// payment, we'll return that.
		if p.lastError != nil {
			return lnwire.ShortChannelID{}, nil,
				errNoRoute{lastError: p.lastError}
		}
		// Terminal state, return.
		return lnwire.ShortChannelID{}, nil, err
	}

	// Generate a new key to be used for this attempt.
	sessionKey, err := generateNewSessionKey()
	if err != nil {
		return lnwire.ShortChannelID{}, nil, err
	}

	// Generate the raw encoded sphinx packet to be included along
	// with the htlcAdd message that we send directly to the
	// switch.
	onionBlob, c, err := generateSphinxPacket(
		rt, p.payment.PaymentHash[:], sessionKey,
	)

	// With SendToRoute, it can happen that the route exceeds protocol
	// constraints. Mark the payment as failed with an internal error.
	if err == route.ErrMaxRouteHopsExceeded ||
		err == sphinx.ErrMaxRoutingInfoSizeExceeded {

		log.Debugf("Invalid route provided for payment %x: %v",
			p.payment.PaymentHash, err)

		controlErr := p.router.cfg.Control.Fail(
			p.payment.PaymentHash, channeldb.FailureReasonError,
		)
		if controlErr != nil {
			return lnwire.ShortChannelID{}, nil, controlErr
		}
	}

	// In any case, don't continue if there is an error.
	if err != nil {
		return lnwire.ShortChannelID{}, nil, err
	}

	// Update our cached circuit with the newly generated
	// one.
	p.circuit = c

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
	attemptID, err := p.router.cfg.NextPaymentID()
	if err != nil {
		return lnwire.ShortChannelID{}, nil, err
	}

	// We now have all the information needed to populate
	// the current attempt information.
	p.attempt = &channeldb.HTLCAttemptInfo{
		AttemptID:  attemptID,
		SessionKey: sessionKey,
		Route:      *rt,
	}

	// Before sending this HTLC to the switch, we checkpoint the
	// fresh attemptID and route to the DB. This lets us know on
	// startup the ID of the payment that we attempted to send,
	// such that we can query the Switch for its whereabouts. The
	// route is needed to handle the result when it eventually
	// comes back.
	err = p.router.cfg.Control.RegisterAttempt(p.payment.PaymentHash, p.attempt)
	if err != nil {
		return lnwire.ShortChannelID{}, nil, err
	}

	return firstHop, htlcAdd, nil
}

// sendPaymentAttempt attempts to send the current attempt to the switch.
func (p *paymentLifecycle) sendPaymentAttempt(firstHop lnwire.ShortChannelID,
	htlcAdd *lnwire.UpdateAddHTLC) error {

	log.Tracef("Attempting to send payment %x (pid=%v), "+
		"using route: %v", p.payment.PaymentHash, p.attempt.AttemptID,
		newLogClosure(func() string {
			return spew.Sdump(p.attempt.Route)
		}),
	)

	// Send it to the Switch. When this method returns we assume
	// the Switch successfully has persisted the payment attempt,
	// such that we can resume waiting for the result after a
	// restart.
	err := p.router.cfg.Payer.SendHTLC(
		firstHop, p.attempt.AttemptID, htlcAdd,
	)
	if err != nil {
		log.Errorf("Failed sending attempt %d for payment "+
			"%x to switch: %v", p.attempt.AttemptID,
			p.payment.PaymentHash, err)
		return err
	}

	log.Debugf("Payment %x (pid=%v) successfully sent to switch, route: %v",
		p.payment.PaymentHash, p.attempt.AttemptID, &p.attempt.Route)

	return nil
}

// handleSendError inspects the given error from the Switch and determines
// whether we should make another payment attempt.
func (p *paymentLifecycle) handleSendError(sendErr error) error {

	reason := p.router.processSendError(
		p.attempt.AttemptID, &p.attempt.Route, sendErr,
	)
	if reason == nil {
		// Save the forwarding error so it can be returned if
		// this turns out to be the last attempt.
		p.lastError = sendErr

		return nil
	}

	log.Debugf("Payment %x failed: final_outcome=%v, raw_err=%v",
		p.payment.PaymentHash, *reason, sendErr)

	// Mark the payment failed with no route.
	//
	// TODO(halseth): make payment codes for the actual reason we don't
	// continue path finding.
	err := p.router.cfg.Control.Fail(
		p.payment.PaymentHash, *reason,
	)
	if err != nil {
		return err
	}

	// Terminal state, return the error we encountered.
	return sendErr
}
