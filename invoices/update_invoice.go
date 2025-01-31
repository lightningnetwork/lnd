package invoices

import (
	"errors"
	"fmt"
	"time"

	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
)

// updateHtlcsAmp takes an invoice, and a new HTLC to be added (along with its
// set ID), and updates the internal AMP state of an invoice, and also tallies
// the set of HTLCs to be updated on disk.
func acceptHtlcsAmp(invoice *Invoice, setID SetID,
	circuitKey models.CircuitKey, htlc *InvoiceHTLC,
	updater InvoiceUpdater) error {

	newAmpState, err := getUpdatedInvoiceAmpState(
		invoice, setID, circuitKey, HtlcStateAccepted, htlc.Amt,
	)
	if err != nil {
		return err
	}

	invoice.AMPState[setID] = newAmpState

	// Mark the updates as needing to be written to disk.
	return updater.UpdateAmpState(setID, newAmpState, circuitKey)
}

// cancelHtlcsAmp processes a cancellation of an HTLC that belongs to an AMP
// HTLC set. We'll need to update the meta data in the main invoice, and also
// apply the new update to the update MAP, since all the HTLCs for a given HTLC
// set need to be written in-line with each other.
func cancelHtlcsAmp(invoice *Invoice, circuitKey models.CircuitKey,
	htlc *InvoiceHTLC, updater InvoiceUpdater) error {

	setID := htlc.AMP.Record.SetID()

	// First, we'll update the state of the entire HTLC set
	// to cancelled.
	newAmpState, err := getUpdatedInvoiceAmpState(
		invoice, setID, circuitKey, HtlcStateCanceled,
		htlc.Amt,
	)
	if err != nil {
		return err
	}

	invoice.AMPState[setID] = newAmpState

	// Mark the updates as needing to be written to disk.
	err = updater.UpdateAmpState(setID, newAmpState, circuitKey)
	if err != nil {
		return err
	}

	// We'll only decrement the total amount paid if the invoice was
	// already in the accepted state.
	if invoice.AmtPaid != 0 {
		return updateInvoiceAmtPaid(
			invoice, invoice.AmtPaid-htlc.Amt, updater,
		)
	}

	return nil
}

// settleHtlcsAmp processes a new settle operation on an HTLC set for an AMP
// invoice. We'll update some meta data in the main invoice, and also signal
// that this HTLC set needs to be re-written back to disk.
func settleHtlcsAmp(invoice *Invoice, circuitKey models.CircuitKey,
	htlc *InvoiceHTLC, updater InvoiceUpdater) error {

	setID := htlc.AMP.Record.SetID()

	// Next update the main AMP meta-data to indicate that this HTLC set
	// has been fully settled.
	newAmpState, err := getUpdatedInvoiceAmpState(
		invoice, setID, circuitKey, HtlcStateSettled, 0,
	)
	if err != nil {
		return err
	}

	invoice.AMPState[setID] = newAmpState

	// Mark the updates as needing to be written to disk.
	return updater.UpdateAmpState(setID, newAmpState, circuitKey)
}

// UpdateInvoice fetches the invoice, obtains the update descriptor from the
// callback and applies the updates in a single db transaction.
func UpdateInvoice(hash *lntypes.Hash, invoice *Invoice,
	updateTime time.Time, callback InvoiceUpdateCallback,
	updater InvoiceUpdater) (*Invoice, error) {

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
		err := cancelHTLCs(invoice, updateTime, update, updater)
		if err != nil {
			return nil, err
		}

	case AddHTLCsUpdate:
		err := addHTLCs(invoice, hash, updateTime, update, updater)
		if err != nil {
			return nil, err
		}

	case SettleHodlInvoiceUpdate:
		err := settleHodlInvoice(
			invoice, hash, updateTime, update.State, updater,
		)
		if err != nil {
			return nil, err
		}

	case CancelInvoiceUpdate:
		err := cancelInvoice(
			invoice, hash, updateTime, update.State, updater,
		)
		if err != nil {
			return nil, err
		}

	default:
		return nil, fmt.Errorf("unknown update type: %s",
			update.UpdateType)
	}

	if err := updater.Finalize(update.UpdateType); err != nil {
		return nil, err
	}

	return invoice, nil
}

// cancelHTLCs tries to cancel the htlcs in the given InvoiceUpdateDesc.
//
// NOTE: cancelHTLCs updates will only use the `CancelHtlcs` field in the
// InvoiceUpdateDesc.
func cancelHTLCs(invoice *Invoice, updateTime time.Time,
	update *InvoiceUpdateDesc, updater InvoiceUpdater) error {

	for key := range update.CancelHtlcs {
		htlc, exists := invoice.Htlcs[key]

		// Verify that we don't get an action for htlcs that are not
		// present on the invoice.
		if !exists {
			return fmt.Errorf("cancel of non-existent htlc")
		}

		err := canCancelSingleHtlc(htlc, invoice.State)
		if err != nil {
			return err
		}

		err = resolveHtlc(
			key, htlc, HtlcStateCanceled, updateTime,
			updater,
		)
		if err != nil {
			return err
		}

		// Tally this into the set of HTLCs that need to be updated on
		// disk, but once again, only if this is an AMP invoice.
		if invoice.IsAMP() {
			err := cancelHtlcsAmp(invoice, key, htlc, updater)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

// addHTLCs tries to add the htlcs in the given InvoiceUpdateDesc.
//
//nolint:funlen
func addHTLCs(invoice *Invoice, hash *lntypes.Hash, updateTime time.Time,
	update *InvoiceUpdateDesc, updater InvoiceUpdater) error {

	var setID *[32]byte
	invoiceIsAMP := invoice.IsAMP()
	if invoiceIsAMP && update.State != nil {
		setID = update.State.SetID
	}

	for key, htlcUpdate := range update.AddHtlcs {
		if _, exists := invoice.Htlcs[key]; exists {
			return fmt.Errorf("duplicate add of htlc %v", key)
		}

		// Force caller to supply htlc without custom records in a
		// consistent way.
		if htlcUpdate.CustomRecords == nil {
			return errors.New("nil custom records map")
		}

		htlc := &InvoiceHTLC{
			Amt:           htlcUpdate.Amt,
			MppTotalAmt:   htlcUpdate.MppTotalAmt,
			Expiry:        htlcUpdate.Expiry,
			AcceptHeight:  uint32(htlcUpdate.AcceptHeight),
			AcceptTime:    updateTime,
			State:         HtlcStateAccepted,
			CustomRecords: htlcUpdate.CustomRecords,
		}

		if invoiceIsAMP {
			if htlcUpdate.AMP == nil {
				return fmt.Errorf("unable to add htlc "+
					"without AMP data to AMP invoice(%v)",
					invoice.AddIndex)
			}

			htlc.AMP = htlcUpdate.AMP.Copy()
		}

		if err := updater.AddHtlc(key, htlc); err != nil {
			return err
		}

		invoice.Htlcs[key] = htlc

		// Collect the set of new HTLCs so we can write them properly
		// below, but only if this is an AMP invoice.
		if invoiceIsAMP {
			err := acceptHtlcsAmp(
				invoice, htlcUpdate.AMP.Record.SetID(), key,
				htlc, updater,
			)
			if err != nil {
				return err
			}
		}
	}

	// At this point, the set of accepted HTLCs should be fully
	// populated with added HTLCs or removed of canceled ones. Update
	// invoice state if the update descriptor indicates an invoice state
	// change, which depends on having an accurate view of the accepted
	// HTLCs.
	if update.State != nil {
		newState, err := getUpdatedInvoiceState(
			invoice, hash, *update.State,
		)
		if err != nil {
			return err
		}

		// If this isn't an AMP invoice, then we'll go ahead and update
		// the invoice state directly here. For AMP invoices, we instead
		// will keep the top-level invoice open, and update the state of
		// each _htlc set_ instead. However, we'll allow the invoice to
		// transition to the cancelled state regardless.
		if !invoiceIsAMP || *newState == ContractCanceled {
			err := updater.UpdateInvoiceState(*newState, nil)
			if err != nil {
				return err
			}
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
	var amtPaid lnwire.MilliSatoshi

	for key, htlc := range invoice.Htlcs {
		// Set the HTLC preimage for any AMP HTLCs.
		if setID != nil && update.State != nil {
			preimage, ok := update.State.HTLCPreimages[key]
			switch {
			// If we don't already have a preimage for this HTLC, we
			// can set it now.
			case ok && htlc.AMP.Preimage == nil:
				err := updater.AddAmpHtlcPreimage(
					htlc.AMP.Record.SetID(), key, preimage,
				)
				if err != nil {
					return err
				}
				htlc.AMP.Preimage = &preimage

			// Otherwise, prevent over-writing an existing
			// preimage.  Ignore the case where the preimage is
			// identical.
			case ok && *htlc.AMP.Preimage != preimage:
				return ErrHTLCPreimageAlreadyExists
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
		htlcStateChanged, htlcState, err := getUpdatedHtlcState(
			htlc, htlcContextState, setID,
		)
		if err != nil {
			return err
		}

		if htlcStateChanged {
			err = resolveHtlc(
				key, htlc, htlcState, updateTime, updater,
			)
			if err != nil {
				return err
			}
		}

		htlcSettled := htlcStateChanged &&
			htlcState == HtlcStateSettled

		// If the HTLC has being settled for the first time, and this
		// is an AMP invoice, then we'll need to update some additional
		// meta data state.
		if htlcSettled && invoiceIsAMP {
			err = settleHtlcsAmp(invoice, key, htlc, updater)
			if err != nil {
				return err
			}
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
	if invoiceIsAMP {
		amtPaid += invoice.AmtPaid
	}

	return updateInvoiceAmtPaid(invoice, amtPaid, updater)
}

func resolveHtlc(circuitKey models.CircuitKey, htlc *InvoiceHTLC,
	state HtlcState, resolveTime time.Time,
	updater InvoiceUpdater) error {

	err := updater.ResolveHtlc(circuitKey, state, resolveTime)
	if err != nil {
		return err
	}
	htlc.State = state
	htlc.ResolveTime = resolveTime

	return nil
}

func updateInvoiceAmtPaid(invoice *Invoice, amt lnwire.MilliSatoshi,
	updater InvoiceUpdater) error {

	err := updater.UpdateInvoiceAmtPaid(amt)
	if err != nil {
		return err
	}
	invoice.AmtPaid = amt

	return nil
}

// settleHodlInvoice marks a hodl invoice as settled.
//
// NOTE: Currently it is not possible to have HODL AMP invoices.
func settleHodlInvoice(invoice *Invoice, hash *lntypes.Hash,
	updateTime time.Time, update *InvoiceStateUpdateDesc,
	updater InvoiceUpdater) error {

	if !invoice.HodlInvoice {
		return fmt.Errorf("unable to settle hodl invoice: %v is "+
			"not a hodl invoice", invoice.AddIndex)
	}

	// TODO(positiveblue): because NewState can only be ContractSettled we
	// can remove it from the API and set it here directly.
	switch {
	case update == nil:
		fallthrough

	case update.NewState != ContractSettled:
		return fmt.Errorf("unable to settle hodl invoice: "+
			"not valid InvoiceUpdateDesc.State: %v", update)

	case update.Preimage == nil:
		return fmt.Errorf("unable to settle hodl invoice: " +
			"preimage is nil")
	}

	newState, err := getUpdatedInvoiceState(
		invoice, hash, *update,
	)
	if err != nil {
		return err
	}

	if newState == nil || *newState != ContractSettled {
		return fmt.Errorf("unable to settle hodl invoice: "+
			"new computed state is not settled: %s", newState)
	}

	err = updater.UpdateInvoiceState(
		ContractSettled, update.Preimage,
	)
	if err != nil {
		return err
	}

	invoice.State = ContractSettled
	invoice.Terms.PaymentPreimage = update.Preimage

	// TODO(positiveblue): this logic can be further simplified.
	var amtPaid lnwire.MilliSatoshi
	for key, htlc := range invoice.Htlcs {
		settled, _, err := getUpdatedHtlcState(
			htlc, ContractSettled, nil,
		)
		if err != nil {
			return err
		}

		if settled {
			err = resolveHtlc(
				key, htlc, HtlcStateSettled, updateTime,
				updater,
			)
			if err != nil {
				return err
			}

			amtPaid += htlc.Amt
		}
	}

	return updateInvoiceAmtPaid(invoice, amtPaid, updater)
}

// cancelInvoice attempts to cancel the given invoice. That includes changing
// the invoice state and the state of any relevant HTLC.
func cancelInvoice(invoice *Invoice, hash *lntypes.Hash,
	updateTime time.Time, update *InvoiceStateUpdateDesc,
	updater InvoiceUpdater) error {

	switch {
	case update == nil:
		fallthrough

	case update.NewState != ContractCanceled:
		return fmt.Errorf("unable to cancel invoice: "+
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

	newState, err := getUpdatedInvoiceState(invoice, hash, *update)
	if err != nil {
		return err
	}

	if newState == nil || *newState != ContractCanceled {
		return fmt.Errorf("unable to cancel invoice(%v): new "+
			"computed state is not canceled: %s", invoice.AddIndex,
			newState)
	}

	err = updater.UpdateInvoiceState(ContractCanceled, nil)
	if err != nil {
		return err
	}
	invoice.State = ContractCanceled

	for key, htlc := range invoice.Htlcs {
		// We might not have a setID here in case we are cancelling
		// an AMP invoice however the setID is only important when
		// settling an AMP HTLC.
		canceled, _, err := getUpdatedHtlcState(
			htlc, ContractCanceled, setID,
		)
		if err != nil {
			return err
		}

		if canceled {
			err = resolveHtlc(
				key, htlc, HtlcStateCanceled, updateTime,
				updater,
			)
			if err != nil {
				return err
			}

			// If its an AMP HTLC we need to make sure we persist
			// this new state otherwise AMP HTLCs are not updated
			// on disk because HTLCs for AMP invoices are stored
			// separately.
			if htlc.AMP != nil {
				err := cancelHtlcsAmp(
					invoice, key, htlc, updater,
				)
				if err != nil {
					return err
				}
			}
		}
	}

	return nil
}

// getUpdatedInvoiceState validates and processes an invoice state update. The
// new state to transition to is returned, so the caller is able to select
// exactly how the invoice state is updated. Note that for AMP invoices this
// function is only used to validate the state transition if we're cancelling
// the invoice.
func getUpdatedInvoiceState(invoice *Invoice, hash *lntypes.Hash,
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

// getUpdatedInvoiceAmpState returns the AMP state of an invoice (without
// applying it), given the new state, and the amount of the HTLC that is
// being updated.
func getUpdatedInvoiceAmpState(invoice *Invoice, setID SetID,
	circuitKey models.CircuitKey, state HtlcState,
	amt lnwire.MilliSatoshi) (InvoiceStateAMP, error) {

	// Retrieve the AMP state for this set ID.
	ampState, ok := invoice.AMPState[setID]

	// If the state is accepted then we may need to create a new entry for
	// this set ID, otherwise we expect that the entry already exists and
	// we can update it.
	if !ok && state != HtlcStateAccepted {
		return InvoiceStateAMP{},
			fmt.Errorf("unable to update AMP state for setID=%x ",
				setID)
	}

	switch state {
	case HtlcStateAccepted:
		if !ok {
			// If an entry for this set ID doesn't already exist,
			// then we'll need to create it.
			ampState = InvoiceStateAMP{
				State: HtlcStateAccepted,
				InvoiceKeys: make(
					map[models.CircuitKey]struct{},
				),
			}
		}

		ampState.AmtPaid += amt

	case HtlcStateCanceled:
		ampState.State = HtlcStateCanceled
		ampState.AmtPaid -= amt

	case HtlcStateSettled:
		ampState.State = HtlcStateSettled
	}

	ampState.InvoiceKeys[circuitKey] = struct{}{}

	return ampState, nil
}

// canCancelSingleHtlc validates cancellation of a single HTLC. If nil is
// returned, then the HTLC can be cancelled.
func canCancelSingleHtlc(htlc *InvoiceHTLC,
	invoiceState ContractState) error {

	// It is only possible to cancel individual htlcs on an open invoice.
	if invoiceState != ContractOpen {
		return fmt.Errorf("htlc canceled on invoice in state %v",
			invoiceState)
	}

	// It is only possible if the htlc is still pending.
	if htlc.State != HtlcStateAccepted {
		return fmt.Errorf("htlc canceled in state %v", htlc.State)
	}

	return nil
}

// getUpdatedHtlcState aligns the state of an htlc with the given invoice state.
// A boolean indicating whether the HTLCs state need to be updated, along with
// the new state (or old state if no change is needed) is returned.
func getUpdatedHtlcState(htlc *InvoiceHTLC,
	invoiceState ContractState, setID *[32]byte) (
	bool, HtlcState, error) {

	trySettle := func(persist bool) (bool, HtlcState, error) {
		if htlc.State != HtlcStateAccepted {
			return false, htlc.State, nil
		}

		// Settle the HTLC if it matches the settled set id. If
		// there're other HTLCs with distinct setIDs, then we'll leave
		// them, as they may eventually be settled as we permit
		// multiple settles to a single pay_addr for AMP.
		settled := false
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
				return false, htlc.State,
					ErrHTLCPreimageMissing

			// Fail if the accepted AMP HTLC has an invalid
			// preimage.
			case !htlc.AMP.Preimage.Matches(htlc.AMP.Hash):
				return false, htlc.State,
					ErrHTLCPreimageMismatch
			}

			settled = true
		}

		// Only persist the changes if the invoice is moving to the
		// settled state, and we're actually updating the state to
		// settled.
		newState := htlc.State
		if settled {
			newState = HtlcStateSettled
		}

		return persist && settled, newState, nil
	}

	if invoiceState == ContractSettled {
		// Check that we can settle the HTLCs. For legacy and MPP HTLCs
		// this will be a NOP, but for AMP HTLCs this asserts that we
		// have a valid hash/preimage pair. Passing true permits the
		// method to update the HTLC to HtlcStateSettled.
		return trySettle(true)
	}

	// We should never find a settled HTLC on an invoice that isn't in
	// ContractSettled.
	if htlc.State == HtlcStateSettled {
		return false, htlc.State, ErrHTLCAlreadySettled
	}

	switch invoiceState {
	case ContractCanceled:
		htlcAlreadyCanceled := htlc.State == HtlcStateCanceled
		return !htlcAlreadyCanceled, HtlcStateCanceled, nil

	// TODO(roasbeef): never fully passed thru now?
	case ContractAccepted:
		// Check that we can settle the HTLCs. For legacy and MPP HTLCs
		// this will be a NOP, but for AMP HTLCs this asserts that we
		// have a valid hash/preimage pair. Passing false prevents the
		// method from putting the HTLC in HtlcStateSettled, leaving it
		// in HtlcStateAccepted.
		return trySettle(false)

	case ContractOpen:
		return false, htlc.State, nil

	default:
		return false, htlc.State, errors.New("unknown state transition")
	}
}
