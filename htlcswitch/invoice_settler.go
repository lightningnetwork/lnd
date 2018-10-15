package htlcswitch

import (
	"github.com/lightningnetwork/lnd/contractcourt"
)

// InvoiceSettler handles settling and failing of invoices.
type InvoiceSettler struct {
	// Registry is a sub-system which responsible for managing the invoices
	// in thread-safe manner.
	Registry InvoiceDatabase

	// ResolutionMsgs is the channel that the switch is listening to for
	// invoice resolutions. InvoiceSettler isn't calling switch directly
	// because this would create a circular dependency.
	ResolutionMsgs chan resolutionMsg
}

// NewInvoiceSettler returns a new invoice settler instance.
func NewInvoiceSettler(Registry InvoiceDatabase) *InvoiceSettler {
	return &InvoiceSettler{
		Registry:       Registry,
		ResolutionMsgs: make(chan resolutionMsg),
	}
}

// Settle settles the (hold) invoice corresponding to the given preimage.
// TO BE IMPLEMENTED.
func (i *InvoiceSettler) Settle(preimage [32]byte) error {

	return nil
}

// Fail fails the (hold) invoice corresponding to the given hash.
// TO BE IMPLEMENTED.
func (i *InvoiceSettler) Fail(hash []byte) error {
	return nil
}

// handleIncoming is called from switch when a htlc comes in for which we are
// the exit hop.
func (i *InvoiceSettler) handleIncoming(pkt *htlcPacket) error {
	// We're the designated payment destination.  Therefore
	// we attempt to see if we have an invoice locally
	// which'll allow us to settle this htlc.
	invoiceHash := pkt.circuit.PaymentHash
	invoice, _, err := i.Registry.LookupInvoice(
		invoiceHash,
	)

	// TODO: Return errors below synchronously or use async flow for
	// all cases?

	/*if err != nil {
		log.Errorf("unable to query invoice registry: "+
			" %v", err)
		failure := lnwire.FailUnknownPaymentHash{}

		i.ResolutionMsgs <- contractcourt.ResolutionMsg{
			SourceChan: exitHop,
			Failure:    failure,
		}
		l.sendHTLCError(
			pd.HtlcIndex, failure, obfuscator, pd.SourceRef,
		)

		needUpdate = true
		return
	}*/

	// If the invoice is already settled, we choose to
	// accept the payment to simplify failure recovery.
	//
	// NOTE: Though our recovery and forwarding logic is
	// predominately batched, settling invoices happens
	// iteratively. We may reject one of two payments
	// for the same rhash at first, but then restart and
	// reject both after seeing that the invoice has been
	// settled. Without any record of which one settles
	// first, it is ambiguous as to which one actually
	// settled the invoice. Thus, by accepting all
	// payments, we eliminate the race condition that can
	// lead to this inconsistency.
	//
	// TODO(conner): track ownership of settlements to
	// properly recover from failures? or add batch invoice
	// settlement

	/*if invoice.Terms.Settled {
		log.Warnf("Accepting duplicate payment for "+
			"hash=%x", invoiceHash)
	}*/

	// If we're not currently in debug mode, and the
	// extended htlc doesn't meet the value requested, then
	// we'll fail the htlc.  Otherwise, we settle this htlc
	// within our local state update log, then send the
	// update entry to the remote party.
	//
	// NOTE: We make an exception when the value requested
	// by the invoice is zero. This means the invoice
	// allows the payee to specify the amount of satoshis
	// they wish to send.  So since we expect the htlc to
	// have a different amount, we should not fail.
	/*if !l.cfg.DebugHTLC && invoice.Terms.Value > 0 &&
		pd.Amount < invoice.Terms.Value {

		log.Errorf("rejecting htlc due to incorrect "+
			"amount: expected %v, received %v",
			invoice.Terms.Value, pd.Amount)

		failure := lnwire.FailIncorrectPaymentAmount{}
		l.sendHTLCError(
			pd.HtlcIndex, failure, obfuscator, pd.SourceRef,
		)

		needUpdate = true
		return
	}*/

	// As we're the exit hop, we'll double check the
	// hop-payload included in the HTLC to ensure that it
	// was crafted correctly by the sender and matches the
	// HTLC we were extended.
	//
	// NOTE: We make an exception when the value requested
	// by the invoice is zero. This means the invoice
	// allows the payee to specify the amount of satoshis
	// they wish to send.  So since we expect the htlc to
	// have a different amount, we should not fail.
	/*if !l.cfg.DebugHTLC && invoice.Terms.Value > 0 &&
		fwdInfo.AmountToForward < invoice.Terms.Value {

		log.Errorf("Onion payload of incoming htlc(%x) "+
			"has incorrect value: expected %v, "+
			"got %v", pd.RHash, invoice.Terms.Value,
			fwdInfo.AmountToForward)

		failure := lnwire.FailIncorrectPaymentAmount{}
		l.sendHTLCError(
			pd.HtlcIndex, failure, obfuscator, pd.SourceRef,
		)

		needUpdate = true
		return
	}*/

	// We'll also ensure that our time-lock value has been
	// computed correctly.

	/*expectedHeight := heightNow + minCltvDelta
	switch {

	case !l.cfg.DebugHTLC && fwdInfo.OutgoingCTLV < expectedHeight:
		log.Errorf("Onion payload of incoming "+
			"htlc(%x) has incorrect time-lock: "+
			"expected %v, got %v",
			pd.RHash[:], expectedHeight,
			fwdInfo.OutgoingCTLV)

		failure := lnwire.NewFinalIncorrectCltvExpiry(
			fwdInfo.OutgoingCTLV,
		)
		l.sendHTLCError(
			pd.HtlcIndex, failure, obfuscator, pd.SourceRef,
		)

		needUpdate = true
		return

	case !l.cfg.DebugHTLC && pd.Timeout != fwdInfo.OutgoingCTLV:
		log.Errorf("HTLC(%x) has incorrect "+
			"time-lock: expected %v, got %v",
			pd.RHash[:], pd.Timeout,
			fwdInfo.OutgoingCTLV)

		failure := lnwire.NewFinalIncorrectCltvExpiry(
			fwdInfo.OutgoingCTLV,
		)
		l.sendHTLCError(
			pd.HtlcIndex, failure, obfuscator, pd.SourceRef,
		)

		needUpdate = true
		return
	}
	*/
	preimage := invoice.Terms.PaymentPreimage

	// TODO: Mark the invoice as accepted here

	// Execute sending resolution message in a go routine to prevent
	// deadlock. Eventually InvoiceSettler may need its own main loop to
	// receive events from the switch and rpcserver.
	//
	// Resolution is only possible when the preimage is known. Otherwise do
	// nothing yet and wait for InvoiceSettler.Settle to be called with the
	// preimage.
	go func() {
		done := make(chan struct{})

		// TODO: This does not work, because the switch cannot look up
		// the incoming channel. Outgoing HtlcIndex hasn't been
		// committed to the circuit map.
		i.ResolutionMsgs <- resolutionMsg{
			ResolutionMsg: contractcourt.ResolutionMsg{
				SourceChan: exitHop,
				HtlcIndex:  invoice.AddIndex,
				PreImage:   &preimage,
			},
			doneChan: done,
		}

		<-done

		// Notify the invoiceRegistry of the invoices we just
		// settled (with the amount accepted at settle time)
		// with this latest commitment update.
		err = i.Registry.SettleInvoice(
			invoiceHash, pkt.incomingAmount,
		)
		if err != nil {
			log.Errorf("unable to settle invoice: %v", err)
		}

		log.Infof("settling %x as exit hop", invoiceHash)

		// If the link is in hodl.BogusSettle mode, replace the
		// preimage with a fake one before sending it to the
		// peer.
		//
		// TODO: This isn't the place anymore?

		/*if l.cfg.DebugHTLC &&
			l.cfg.HodlMask.Active(hodl.BogusSettle) {
			l.warnf(hodl.BogusSettle.Warning())
			preimage = [32]byte{}
			copy(preimage[:], bytes.Repeat([]byte{2}, 32))
		}*/
	}()

	return nil
}
