package invoices

import (
	"errors"
	"fmt"
	"time"

	"github.com/lightningnetwork/lnd/channeldb/models"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
)

// UpdateInvoice fetches the invoice, obtains the update descriptor from the
// callback and applies the updates in a single db transaction.
func UpdateInvoice(hash *lntypes.Hash, invoice *Invoice, updateTime time.Time,
	callback InvoiceUpdateCallback, updater InvoiceUpdater) (*Invoice,
	error) {

	// Create deep copy to prevent any accidental modification in the
	// callback.
	invoiceCopy, err := CopyInvoice(invoice)
	if err != nil {
		return nil, err
	}

	// Call the callback and obtain the update descriptor.
	update, err := callback(invoiceCopy)
	if err != nil {
		return invoice, err
	}

	// If there is nothing to update, return early.
	if update == nil {
		return invoice, nil
	}

	switch update.UpdateType {
	case CancelHTLCsUpdate:
		updateCtx, err := cancelHTLCs(invoice, updateTime, update)
		if err != nil {
			return nil, err
		}

		err = updater.StoreCancelHtlcsUpdate(*updateCtx)
		if err != nil {
			return nil, err
		}

		return updateCtx.Invoice, nil

	case AddHTLCsUpdate:
		updateCtx, err := addHTLCs(invoice, hash, updateTime, update)
		if err != nil {
			return nil, err
		}

		err = updater.StoreAddHtlcsUpdate(*updateCtx)
		if err != nil {
			return nil, err
		}

		return updateCtx.Invoice, nil

	case SettleHodlInvoiceUpdate:
		updateCtx, err := settleHodlInvoice(
			invoice, hash, updateTime, update.State,
		)
		if err != nil {
			return nil, err
		}

		err = updater.StoreSettleHodlInvoiceUpdate(*updateCtx)
		if err != nil {
			return nil, err
		}

		return updateCtx.Invoice, nil

	case CancelInvoiceUpdate:
		updateCtx, err := cancelInvoice(
			invoice, hash, updateTime, update.State,
		)
		if err != nil {
			return nil, err
		}

		err = updater.StoreCancelInvoiceUpdate(*updateCtx)
		if err != nil {
			return nil, err
		}

		return updateCtx.Invoice, nil

	default:
		return nil, fmt.Errorf("unknown update type: %s",
			update.UpdateType)
	}
}

// cancelHTLCs tries to cancel the htlcs in the given InvoiceUpdateDesc.
//
// NOTE: cancelHTLCs updates will only use the `CancelHtlcs` field in the
// InvoiceUpdateDesc.
func cancelHTLCs(invoice *Invoice, updateTime time.Time,
	update *InvoiceUpdateDesc) (*InvoiceUpdaterContext, error) {

	// Process add actions from update descriptor.
	htlcsAmpUpdate := make(map[SetID]map[models.CircuitKey]*InvoiceHTLC)

	// Process cancel actions from update descriptor.
	cancelHtlcs := update.CancelHtlcs
	for key, htlc := range invoice.Htlcs {
		htlc := htlc

		// Check whether this htlc needs to be canceled. If it does,
		// update the htlc state to Canceled.
		_, cancel := cancelHtlcs[key]
		if !cancel {
			continue
		}

		err := cancelSingleHtlc(updateTime, htlc, invoice.State)
		if err != nil {
			return nil, err
		}

		// Delete processed cancel action, so that we can check later
		// that there are no actions left.
		delete(cancelHtlcs, key)

		// Tally this into the set of HTLCs that need to be updated on
		// disk, but once again, only if this is an AMP invoice.
		if invoice.IsAMP() {
			cancelHtlcsAmp(invoice, htlcsAmpUpdate, htlc, key)
		}
	}

	// Verify that we didn't get an action for htlcs that are not present on
	// the invoice.
	if len(cancelHtlcs) > 0 {
		return nil, errors.New("cancel action on non-existent htlc(s)")
	}

	return &InvoiceUpdaterContext{
		Invoice:        invoice,
		UpdateTime:     updateTime,
		AMPHTLCsUpdate: htlcsAmpUpdate,
	}, nil
}

// addHTLCs tries to add the htlcs in the given InvoiceUpdateDesc.
func addHTLCs(invoice *Invoice, hash *lntypes.Hash,
	updateTime time.Time, update *InvoiceUpdateDesc) (
	*InvoiceUpdaterContext, error) {

	var setID *[32]byte
	invoiceIsAMP := invoice.IsAMP()
	if invoiceIsAMP && update.State != nil {
		setID = update.State.SetID
	}

	// Process add actions from update descriptor.
	htlcsAmpUpdate := make(map[SetID]map[models.CircuitKey]*InvoiceHTLC) //nolint:lll
	for key, htlcUpdate := range update.AddHtlcs {
		if _, exists := invoice.Htlcs[key]; exists {
			return nil, fmt.Errorf("duplicate add of htlc %v", key)
		}

		// Force caller to supply htlc without custom records in a
		// consistent way.
		if htlcUpdate.CustomRecords == nil {
			return nil, errors.New("nil custom records map")
		}

		htlc := &InvoiceHTLC{
			Amt:           htlcUpdate.Amt,
			MppTotalAmt:   htlcUpdate.MppTotalAmt,
			Expiry:        htlcUpdate.Expiry,
			AcceptHeight:  uint32(htlcUpdate.AcceptHeight),
			AcceptTime:    updateTime,
			State:         HtlcStateAccepted,
			CustomRecords: htlcUpdate.CustomRecords,
			AMP:           htlcUpdate.AMP.Copy(),
		}

		invoice.Htlcs[key] = htlc

		// Collect the set of new HTLCs so we can write them properly
		// below, but only if this is an AMP invoice.
		if invoiceIsAMP {
			if htlcUpdate.AMP == nil {
				return nil, fmt.Errorf("unable to add htlc "+
					"without AMP data to AMP invoice(%v)",
					invoice.AddIndex)
			}

			updateHtlcsAmp(
				invoice, htlcsAmpUpdate, htlc,
				htlcUpdate.AMP.Record.SetID(), key,
			)
		}
	}

	// At this point, the set of accepted HTLCs should be fully
	// populated with added HTLCs or removed of canceled ones. Update
	// invoice state if the update descriptor indicates an invoice state
	// change, which depends on having an accurate view of the accepted
	// HTLCs.
	if update.State != nil {
		newState, err := updateInvoiceState(
			invoice, hash, *update.State,
		)
		if err != nil {
			return nil, err
		}

		// If this isn't an AMP invoice, then we'll go ahead and update
		// the invoice state directly here. For AMP invoices, we instead
		// will keep the top-level invoice open, and update the state of
		// each _htlc set_ instead. However, we'll allow the invoice to
		// transition to the cancelled state regardless.
		if !invoiceIsAMP || *newState == ContractCanceled {
			invoice.State = *newState
		}
	}

	// The set of HTLC pre-images will only be set if we were actually able
	// to reconstruct all the AMP pre-images.
	var settleEligibleAMP bool
	if update.State != nil {
		settleEligibleAMP = len(update.State.HTLCPreimages) != 0
	}

	// With any invoice level state transitions recorded, we'll now
	// finalize the process by updating the state transitions for
	// individual HTLCs
	var (
		settledSetIDs = make(map[SetID]struct{})
		amtPaid       lnwire.MilliSatoshi
	)
	for key, htlc := range invoice.Htlcs {
		// Set the HTLC preimage for any AMP HTLCs.
		if setID != nil && update.State != nil {
			preimage, ok := update.State.HTLCPreimages[key]
			switch {
			// If we don't already have a preimage for this HTLC, we
			// can set it now.
			case ok && htlc.AMP.Preimage == nil:
				htlc.AMP.Preimage = &preimage

			// Otherwise, prevent over-writing an existing
			// preimage.  Ignore the case where the preimage is
			// identical.
			case ok && *htlc.AMP.Preimage != preimage:
				return nil, ErrHTLCPreimageAlreadyExists
			}
		}

		// The invoice state may have changed and this could have
		// implications for the states of the individual htlcs. Align
		// the htlc state with the current invoice state.
		//
		// If we have all the pre-images for an AMP invoice, then we'll
		// act as if we're able to settle the entire invoice. We need
		// to do this since it's possible for us to settle AMP invoices
		// while the contract state (on disk) is still in the accept
		// state.
		htlcContextState := invoice.State
		if settleEligibleAMP {
			htlcContextState = ContractSettled
		}
		htlcSettled, err := updateHtlc(
			updateTime, htlc, htlcContextState, setID,
		)
		if err != nil {
			return nil, err
		}

		// If the HTLC has being settled for the first time, and this
		// is an AMP invoice, then we'll need to update some additional
		// meta data state.
		if htlcSettled && invoiceIsAMP {
			settleHtlcsAmp(
				invoice, settledSetIDs, htlcsAmpUpdate, htlc,
				key,
			)
		}

		accepted := htlc.State == HtlcStateAccepted
		settled := htlc.State == HtlcStateSettled
		invoiceStateReady := accepted || settled

		if !invoiceIsAMP {
			// Update the running amount paid to this invoice. We
			// don't include accepted htlcs when the invoice is
			// still open.
			if invoice.State != ContractOpen &&
				invoiceStateReady {

				amtPaid += htlc.Amt
			}
		} else {
			// For AMP invoices, since we won't always be reading
			// out the total invoice set each time, we'll instead
			// accumulate newly added invoices to the total amount
			// paid.
			if _, ok := update.AddHtlcs[key]; !ok {
				continue
			}

			// Update the running amount paid to this invoice. AMP
			// invoices never go to the settled state, so if it's
			// open, then we tally the HTLC.
			if invoice.State == ContractOpen &&
				invoiceStateReady {

				amtPaid += htlc.Amt
			}
		}
	}

	// For non-AMP invoices we recalculate the amount paid from scratch
	// each time, while for AMP invoices, we'll accumulate only based on
	// newly added HTLCs.
	if !invoiceIsAMP {
		invoice.AmtPaid = amtPaid
	} else {
		invoice.AmtPaid += amtPaid
	}

	return &InvoiceUpdaterContext{
		Invoice:        invoice,
		UpdateTime:     updateTime,
		SettledSetIDs:  settledSetIDs,
		AMPHTLCsUpdate: htlcsAmpUpdate,
	}, nil
}

// settleHodlInvoice marks a hodl invoice as settled.
//
// NOTE: Currently it is not possible to have HODL AMP invoices.
func settleHodlInvoice(invoice *Invoice, hash *lntypes.Hash,
	updateTime time.Time, update *InvoiceStateUpdateDesc) (
	*InvoiceUpdaterContext, error) {

	if !invoice.HodlInvoice {
		return nil, fmt.Errorf("unable to settle hodl invoice: %v is "+
			"not a hodl invoice", invoice.AddIndex)
	}

	// TODO(positiveblue): because NewState can only be ContractSettled we
	// can remove it from the API and set it here directly.
	switch {
	case update == nil:
		fallthrough

	case update.NewState != ContractSettled:
		return nil, fmt.Errorf("unable to settle hodl invoice: "+
			"not valid InvoiceUpdateDesc.State: %v", update)

	case update.Preimage == nil:
		return nil, fmt.Errorf("unable to settle hodl invoice: " +
			"preimage is nil")
	}

	// TODO(positiveblue): create a invoice.CanSettleHodlInvoice func.
	newState, err := updateInvoiceState(invoice, hash, *update)
	if err != nil {
		return nil, err
	}

	if newState == nil || *newState != ContractSettled {
		return nil, fmt.Errorf("unable to settle hodl invoice: "+
			"new computed state is not settled: %s", newState)
	}

	invoice.State = ContractSettled

	// TODO(positiveblue): this logic can be further simplified.
	var amtPaid lnwire.MilliSatoshi
	for _, htlc := range invoice.Htlcs {
		_, err := updateHtlc(
			updateTime, htlc, ContractSettled, nil,
		)
		if err != nil {
			return nil, err
		}

		if htlc.State == HtlcStateSettled {
			amtPaid += htlc.Amt
		}
	}

	invoice.AmtPaid = amtPaid

	return &InvoiceUpdaterContext{
		Invoice:    invoice,
		UpdateTime: updateTime,
	}, nil
}

// cancelInvoice attempts to cancel the given invoice. That includes changing
// the invoice state and the state of any relevant HTLC.
func cancelInvoice(invoice *Invoice, hash *lntypes.Hash,
	updateTime time.Time, update *InvoiceStateUpdateDesc) (
	*InvoiceUpdaterContext, error) {

	switch {
	case update == nil:
		fallthrough

	case update.NewState != ContractCanceled:
		return nil, fmt.Errorf("unable to cancel invoice: "+
			"InvoiceUpdateDesc.State not valid: %v", update)
	}

	var (
		setID        *[32]byte
		invoiceIsAMP bool
	)

	invoiceIsAMP = invoice.IsAMP()
	if invoiceIsAMP {
		setID = update.SetID
	}

	newState, err := updateInvoiceState(invoice, hash, *update)
	if err != nil {
		return nil, err
	}

	if newState == nil || *newState != ContractCanceled {
		return nil, fmt.Errorf("unable to cancel invoice(%v): new "+
			"computed state is not canceled: %s", invoice.AddIndex,
			newState)
	}

	invoice.State = ContractCanceled

	// TODO(positiveblue): this logic can be simplified.
	for _, htlc := range invoice.Htlcs {
		_, err := updateHtlc(
			updateTime, htlc, ContractCanceled, setID,
		)
		if err != nil {
			return nil, err
		}
	}

	return &InvoiceUpdaterContext{
		Invoice:    invoice,
		UpdateTime: updateTime,
	}, nil
}

// updateInvoiceState validates and processes an invoice state update. The new
// state to transition to is returned, so the caller is able to select exactly
// how the invoice state is updated.
func updateInvoiceState(invoice *Invoice, hash *lntypes.Hash,
	update InvoiceStateUpdateDesc) (*ContractState, error) {

	// Returning to open is never allowed from any state.
	if update.NewState == ContractOpen {
		return nil, ErrInvoiceCannotOpen
	}

	switch invoice.State {
	// Once a contract is accepted, we can only transition to settled or
	// canceled. Forbid transitioning back into this state. Otherwise this
	// state is identical to ContractOpen, so we fallthrough to apply the
	// same checks that we apply to open invoices.
	case ContractAccepted:
		if update.NewState == ContractAccepted {
			return nil, ErrInvoiceCannotAccept
		}

		fallthrough

	// If a contract is open, permit a state transition to accepted, settled
	// or canceled. The only restriction is on transitioning to settled
	// where we ensure the preimage is valid.
	case ContractOpen:
		if update.NewState == ContractCanceled {
			return &update.NewState, nil
		}

		// Sanity check that the user isn't trying to settle or accept a
		// non-existent HTLC set.
		set := invoice.HTLCSet(update.SetID, HtlcStateAccepted)
		if len(set) == 0 {
			return nil, ErrEmptyHTLCSet
		}

		// For AMP invoices, there are no invoice-level preimage checks.
		// However, we still sanity check that we aren't trying to
		// settle an AMP invoice with a preimage.
		if update.SetID != nil {
			if update.Preimage != nil {
				return nil, errors.New("AMP set cannot have " +
					"preimage")
			}

			return &update.NewState, nil
		}

		switch {
		// If an invoice-level preimage was supplied, but the InvoiceRef
		// doesn't specify a hash (e.g. AMP invoices) we fail.
		case update.Preimage != nil && hash == nil:
			return nil, ErrUnexpectedInvoicePreimage

		// Validate the supplied preimage for non-AMP invoices.
		case update.Preimage != nil:
			if update.Preimage.Hash() != *hash {
				return nil, ErrInvoicePreimageMismatch
			}
			invoice.Terms.PaymentPreimage = update.Preimage

		// Permit non-AMP invoices to be accepted without knowing the
		// preimage. When trying to settle we'll have to pass through
		// the above check in order to not hit the one below.
		case update.NewState == ContractAccepted:

		// Fail if we still don't have a preimage when transitioning to
		// settle the non-AMP invoice.
		case update.NewState == ContractSettled &&
			invoice.Terms.PaymentPreimage == nil:

			return nil, errors.New("unknown preimage")
		}

		return &update.NewState, nil

	// Once settled, we are in a terminal state.
	case ContractSettled:
		return nil, ErrInvoiceAlreadySettled

	// Once canceled, we are in a terminal state.
	case ContractCanceled:
		return nil, ErrInvoiceAlreadyCanceled

	default:
		return nil, errors.New("unknown state transition")
	}
}

// cancelHtlcsAmp processes a cancellation of an HTLC that belongs to an AMP
// HTLC set. We'll need to update the meta data in the  main invoice, and also
// apply the new update to the update MAP, since all the HTLCs for a given HTLC
// set need to be written in-line with each other.
func cancelHtlcsAmp(invoice *Invoice,
	updateMap map[SetID]map[models.CircuitKey]*InvoiceHTLC,
	htlc *InvoiceHTLC, circuitKey models.CircuitKey) {

	setID := htlc.AMP.Record.SetID()

	// First, we'll update the state of the entire HTLC set to cancelled.
	ampState := invoice.AMPState[setID]
	ampState.State = HtlcStateCanceled

	ampState.InvoiceKeys[circuitKey] = struct{}{}
	ampState.AmtPaid -= htlc.Amt

	// With the state update,d we'll set the new value so the struct
	// changes are propagated.
	invoice.AMPState[setID] = ampState

	if _, ok := updateMap[setID]; !ok {
		// Only HTLCs in the accepted state, can be cancelled, but we
		// also want to merge that with HTLCs that may be canceled as
		// well since it can be cancelled one by one.
		updateMap[setID] = invoice.HTLCSet(
			&setID, HtlcStateAccepted,
		)

		cancelledHtlcs := invoice.HTLCSet(
			&setID, HtlcStateCanceled,
		)
		for htlcKey, htlc := range cancelledHtlcs {
			updateMap[setID][htlcKey] = htlc
		}
	}

	// Finally, include the newly cancelled HTLC in the set of HTLCs we
	// need to cancel.
	updateMap[setID][circuitKey] = htlc

	// We'll only decrement the total amount paid if the invoice was
	// already in the accepted state.
	if invoice.AmtPaid != 0 {
		invoice.AmtPaid -= htlc.Amt
	}
}

// cancelSingleHtlc validates cancellation of a single htlc and update its
// state.
func cancelSingleHtlc(resolveTime time.Time, htlc *InvoiceHTLC,
	invState ContractState) error {

	// It is only possible to cancel individual htlcs on an open invoice.
	if invState != ContractOpen {
		return fmt.Errorf("htlc canceled on invoice in "+
			"state %v", invState)
	}

	// It is only possible if the htlc is still pending.
	if htlc.State != HtlcStateAccepted {
		return fmt.Errorf("htlc canceled in state %v",
			htlc.State)
	}

	htlc.State = HtlcStateCanceled
	htlc.ResolveTime = resolveTime

	return nil
}

// updateHtlcsAmp takes an invoice, and a new HTLC to be added (along with its
// set ID), and update sthe internal AMP state of an invoice, and also tallies
// the set of HTLCs to be updated on disk.
func updateHtlcsAmp(invoice *Invoice,
	updateMap map[SetID]map[models.CircuitKey]*InvoiceHTLC,
	htlc *InvoiceHTLC, setID SetID,
	circuitKey models.CircuitKey) {

	ampState, ok := invoice.AMPState[setID]
	if !ok {
		// If an entry for this set ID doesn't already exist, then
		// we'll need to create it.
		ampState = InvoiceStateAMP{
			State:       HtlcStateAccepted,
			InvoiceKeys: make(map[models.CircuitKey]struct{}),
		}
	}

	ampState.AmtPaid += htlc.Amt
	ampState.InvoiceKeys[circuitKey] = struct{}{}

	// Due to the way maps work, we need to read out the value, update it,
	// then re-assign it into the map.
	invoice.AMPState[setID] = ampState

	// Now that we've updated the invoice state, we'll inform the caller of
	// the _neitre_ HTLC set they need to write for this new set ID.
	if _, ok := updateMap[setID]; !ok {
		// If we're just now creating the HTLCs for this set then we'll
		// also pull in the existing HTLCs are part of this set, so we
		// can write them all to disk together (same value)
		updateMap[setID] = invoice.HTLCSet(
			(*[32]byte)(&setID), HtlcStateAccepted,
		)
	}
	updateMap[setID][circuitKey] = htlc
}

// settleHtlcsAmp processes a new settle operation on an HTLC set for an AMP
// invoice. We'll update some meta data in the main invoice, and also signal
// that this HTLC set needs to be re-written back to disk.
func settleHtlcsAmp(invoice *Invoice,
	settledSetIDs map[SetID]struct{},
	updateMap map[SetID]map[models.CircuitKey]*InvoiceHTLC,
	htlc *InvoiceHTLC, circuitKey models.CircuitKey) {

	// First, add the set ID to the set that was settled in this invoice
	// update. We'll use this later to update the settle index.
	setID := htlc.AMP.Record.SetID()
	settledSetIDs[setID] = struct{}{}

	// Next update the main AMP meta-data to indicate that this HTLC set
	// has been fully settled.
	ampState := invoice.AMPState[setID]
	ampState.State = HtlcStateSettled

	ampState.InvoiceKeys[circuitKey] = struct{}{}

	invoice.AMPState[setID] = ampState

	// Finally, we'll add this to the set of HTLCs that need to be updated.
	if _, ok := updateMap[setID]; !ok {
		mapEntry := make(map[models.CircuitKey]*InvoiceHTLC)
		updateMap[setID] = mapEntry
	}
	updateMap[setID][circuitKey] = htlc
}

// updateHtlc aligns the state of an htlc with the given invoice state. A
// boolean is returned if the HTLC was settled.
func updateHtlc(resolveTime time.Time, htlc *InvoiceHTLC,
	invState ContractState, setID *[32]byte) (bool, error) {

	trySettle := func(persist bool) (bool, error) {
		if htlc.State != HtlcStateAccepted {
			return false, nil
		}

		// Settle the HTLC if it matches the settled set id. If
		// there're other HTLCs with distinct setIDs, then we'll leave
		// them, as they may eventually be settled as we permit
		// multiple settles to a single pay_addr for AMP.
		var htlcState HtlcState
		if htlc.IsInHTLCSet(setID) {
			// Non-AMP HTLCs can be settled immediately since we
			// already know the preimage is valid due to checks at
			// the invoice level. For AMP HTLCs, verify that the
			// per-HTLC preimage-hash pair is valid.
			switch {
			// Non-AMP HTLCs can be settle immediately since we
			// already know the preimage is valid due to checks at
			// the invoice level.
			case setID == nil:

			// At this point, the setID is non-nil, meaning this is
			// an AMP HTLC. We know that htlc.AMP cannot be nil,
			// otherwise IsInHTLCSet would have returned false.
			//
			// Fail if an accepted AMP HTLC has no preimage.
			case htlc.AMP.Preimage == nil:
				return false, ErrHTLCPreimageMissing

			// Fail if the accepted AMP HTLC has an invalid
			// preimage.
			case !htlc.AMP.Preimage.Matches(htlc.AMP.Hash):
				return false, ErrHTLCPreimageMismatch
			}

			htlcState = HtlcStateSettled
		}

		// Only persist the changes if the invoice is moving to the
		// settled state, and we're actually updating the state to
		// settled.
		if persist && htlcState == HtlcStateSettled {
			htlc.State = htlcState
			htlc.ResolveTime = resolveTime
		}

		return persist && htlcState == HtlcStateSettled, nil
	}

	if invState == ContractSettled {
		// Check that we can settle the HTLCs. For legacy and MPP HTLCs
		// this will be a NOP, but for AMP HTLCs this asserts that we
		// have a valid hash/preimage pair. Passing true permits the
		// method to update the HTLC to HtlcStateSettled.
		return trySettle(true)
	}

	// We should never find a settled HTLC on an invoice that isn't in
	// ContractSettled.
	if htlc.State == HtlcStateSettled {
		return false, ErrHTLCAlreadySettled
	}

	switch invState {
	case ContractCanceled:
		if htlc.State == HtlcStateAccepted {
			htlc.State = HtlcStateCanceled
			htlc.ResolveTime = resolveTime
		}

		return false, nil

	// TODO(roasbeef): never fully passed thru now?
	case ContractAccepted:
		// Check that we can settle the HTLCs. For legacy and MPP HTLCs
		// this will be a NOP, but for AMP HTLCs this asserts that we
		// have a valid hash/preimage pair. Passing false prevents the
		// method from putting the HTLC in HtlcStateSettled, leaving it
		// in HtlcStateAccepted.
		return trySettle(false)

	case ContractOpen:
		return false, nil

	default:
		return false, errors.New("unknown state transition")
	}
}
