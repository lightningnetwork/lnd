package routing

import (
	"fmt"
	"time"

	"github.com/davecgh/go-spew/spew"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

// paymentLifecycle holds all information about the current state of a payment
// needed to resume if from any point.
type paymentLifecycle struct {
	router         *ChannelRouter
	payment        *LightningPayment
	paySession     *paymentSession
	timeoutChan    <-chan time.Time
	currentHeight  int32
	finalCLTVDelta uint16
	circuit        *sphinx.Circuit
	lastError      error
}

// resumePayment resumes the paymentLifecycle from the current state.
func (p *paymentLifecycle) resumePayment() ([32]byte, *route.Route, error) {
	// We'll continue until either our payment succeeds, or we encounter a
	// critical error during path finding.
	for {
		// Before we attempt this next payment, we'll check to see if
		// either we've gone past the payment attempt timeout, or the
		// router is exiting. In either case, we'll stop this payment
		// attempt short.
		select {
		case <-p.timeoutChan:
			errStr := fmt.Sprintf("payment attempt not completed " +
				"before timeout")

			return [32]byte{}, nil, newErr(
				ErrPaymentAttemptTimeout, errStr,
			)

		case <-p.router.quit:
			// The payment will be resumed from the current state
			// after restart.
			return [32]byte{}, nil, ErrRouterShuttingDown

		default:
			// Fall through if we haven't hit our time limit, or
			// are expiring.
		}

		// Create a new payment attempt from the given payment session.
		route, err := p.paySession.RequestRoute(
			p.payment, uint32(p.currentHeight), p.finalCLTVDelta,
		)
		if err != nil {
			// If there was an error already recorded for this
			// payment, we'll return that.
			if p.lastError != nil {
				return [32]byte{}, nil, fmt.Errorf("unable to "+
					"route payment to destination: %v",
					p.lastError)
			}

			// Terminal state, return.
			return [32]byte{}, nil, err
		}

		// Generate a new key to be used for this attempt.
		sessionKey, err := generateNewSessionKey()
		if err != nil {
			return [32]byte{}, nil, err
		}

		// Generate the raw encoded sphinx packet to be included along
		// with the htlcAdd message that we send directly to the
		// switch.
		onionBlob, c, err := generateSphinxPacket(
			route, p.payment.PaymentHash[:], sessionKey,
		)
		if err != nil {
			return [32]byte{}, nil, err
		}

		// Update our cached circuit with the newly generated
		// one.
		p.circuit = c

		// Craft an HTLC packet to send to the layer 2 switch. The
		// metadata within this packet will be used to route the
		// payment through the network, starting with the first-hop.
		htlcAdd := &lnwire.UpdateAddHTLC{
			Amount:      route.TotalAmount,
			Expiry:      route.TotalTimeLock,
			PaymentHash: p.payment.PaymentHash,
		}
		copy(htlcAdd.OnionBlob[:], onionBlob)

		// Attempt to send this payment through the network to complete
		// the payment. If this attempt fails, then we'll continue on
		// to the next available route.
		firstHop := lnwire.NewShortChanIDFromInt(
			route.Hops[0].ChannelID,
		)

		// We generate a new, unique payment ID that we will use for
		// this HTLC.
		paymentID, err := p.router.cfg.NextPaymentID()
		if err != nil {
			return [32]byte{}, nil, err
		}

		log.Tracef("Attempting to send payment %x (pid=%v), "+
			"using route: %v", p.payment.PaymentHash, paymentID,
			newLogClosure(func() string {
				return spew.Sdump(route)
			}),
		)

		// Send it to the Switch. When this method returns we assume
		// the Switch successfully has persisted the payment attempt,
		// such that we can resume waiting for the result after a
		// restart.
		err = p.router.cfg.Payer.SendHTLC(
			firstHop, paymentID, htlcAdd,
		)
		if err != nil {
			log.Errorf("Failed sending attempt %d for payment "+
				"%x to switch: %v", paymentID, p.payment.PaymentHash, err)

			// We must inspect the error to know whether it was
			// critical or not, to decide whether we should
			// continue trying.
			finalOutcome := p.router.processSendError(
				p.paySession, route, err,
			)
			if finalOutcome {
				return [32]byte{}, nil, err
			}

			// We make another payment attempt.
			p.lastError = err
			continue
		}

		log.Debugf("Payment %x (pid=%v) successfully sent to switch",
			p.payment.PaymentHash, paymentID)

		// Using the created circuit, initialize the error decrypter so we can
		// parse+decode any failures incurred by this payment within the
		// switch.
		errorDecryptor := &htlcswitch.SphinxErrorDecrypter{
			OnionErrorDecrypter: sphinx.NewOnionErrorDecrypter(p.circuit),
		}

		// Now ask the switch to return the result of the payment when
		// available.
		resultChan, err := p.router.cfg.Payer.GetPaymentResult(
			paymentID, errorDecryptor,
		)
		switch {

		// If this payment ID is unknown to the Switch, it means it was
		// never checkpointed and forwarded by the switch before a
		// restart. In this case we can safely send a new payment
		// attempt, and wait for its result to be available.
		case err == htlcswitch.ErrPaymentIDNotFound:
			log.Debugf("Payment ID %v for hash %x not found in "+
				"the Switch, retrying.", paymentID,
				p.payment.PaymentHash)

			// Make a new attempt.
			continue

		// A critical, unexpected error was encountered.
		case err != nil:
			log.Errorf("Failed getting result for paymentID %d "+
				"from switch: %v", paymentID, err)

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

			finalOutcome := p.router.processSendError(
				p.paySession, route, result.Error,
			)

			if finalOutcome {
				log.Errorf("Payment %x failed with "+
					"final outcome: %v",
					p.payment.PaymentHash, result.Error)

				// Terminal state, return the error we
				// encountered.
				return [32]byte{}, nil, result.Error
			}

			// We make another payment attempt.
			p.lastError = result.Error
			continue
		}

		// We successfully got a payment result back from the switch.
		log.Debugf("Payment %x succeeded with pid=%v",
			p.payment.PaymentHash, paymentID)

		// Terminal state, return the preimage and the route
		// taken.
		return result.Preimage, route, nil
	}

}
