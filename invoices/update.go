package invoices

import (
	"bytes"
	"encoding/hex"
	"errors"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/amp"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
)

// invoiceUpdateCtx is an object that describes the context for the invoice
// update to be carried out.
type invoiceUpdateCtx struct {
	hash                 lntypes.Hash
	circuitKey           CircuitKey
	amtPaid              lnwire.MilliSatoshi
	expiry               uint32
	currentHeight        int32
	finalCltvRejectDelta int32

	// wireCustomRecords are the custom records that were included with the
	// HTLC wire message.
	wireCustomRecords lnwire.CustomRecords

	// customRecords is a map of custom records that were included with the
	// HTLC onion payload.
	customRecords record.CustomSet

	mpp          *record.MPP
	amp          *record.AMP
	metadata     []byte
	pathID       *chainhash.Hash
	totalAmtMsat lnwire.MilliSatoshi
}

// invoiceRef returns an identifier that can be used to lookup or update the
// invoice this HTLC is targeting.
func (i *invoiceUpdateCtx) invoiceRef() InvoiceRef {
	switch {
	case i.pathID != nil:
		return InvoiceRefByHashAndAddr(i.hash, *i.pathID)

	case i.amp != nil && i.mpp != nil:
		payAddr := i.mpp.PaymentAddr()
		return InvoiceRefByAddr(payAddr)

	case i.mpp != nil:
		payAddr := i.mpp.PaymentAddr()
		return InvoiceRefByHashAndAddr(i.hash, payAddr)

	default:
		return InvoiceRefByHash(i.hash)
	}
}

// setID returns an identifier that identifies other possible HTLCs that this
// particular one is related to. If nil is returned this means the HTLC is an
// MPP or legacy payment, otherwise the HTLC belongs AMP payment.
func (i invoiceUpdateCtx) setID() *[32]byte {
	if i.amp != nil {
		setID := i.amp.SetID()
		return &setID
	}
	return nil
}

// log logs a message specific to this update context.
func (i *invoiceUpdateCtx) log(s string) {
	// Don't use %x in the log statement below, because it doesn't
	// distinguish between nil and empty metadata.
	metadata := "<nil>"
	if i.metadata != nil {
		metadata = hex.EncodeToString(i.metadata)
	}

	log.Debugf("Invoice%v: %v, amt=%v, expiry=%v, circuit=%v, mpp=%v, "+
		"amp=%v, metadata=%v", i.invoiceRef(), s, i.amtPaid, i.expiry,
		i.circuitKey, i.mpp, i.amp, metadata)
}

// failRes is a helper function which creates a failure resolution with
// the information contained in the invoiceUpdateCtx and the fail resolution
// result provided.
func (i invoiceUpdateCtx) failRes(outcome FailResolutionResult) *HtlcFailResolution {
	return NewFailResolution(i.circuitKey, i.currentHeight, outcome)
}

// settleRes is a helper function which creates a settle resolution with
// the information contained in the invoiceUpdateCtx and the preimage and
// the settle resolution result provided.
func (i invoiceUpdateCtx) settleRes(preimage lntypes.Preimage,
	outcome SettleResolutionResult) *HtlcSettleResolution {

	return NewSettleResolution(
		preimage, i.circuitKey, i.currentHeight, outcome,
	)
}

// acceptRes is a helper function which creates an accept resolution with
// the information contained in the invoiceUpdateCtx and the accept resolution
// result provided.
func (i invoiceUpdateCtx) acceptRes(
	outcome acceptResolutionResult) *htlcAcceptResolution {

	return newAcceptResolution(i.circuitKey, outcome)
}

// resolveReplayedHtlc returns the HTLC resolution for a replayed HTLC. The
// returned boolean indicates whether the HTLC was replayed or not.
func resolveReplayedHtlc(ctx *invoiceUpdateCtx, inv *Invoice) (bool,
	HtlcResolution, error) {

	// Don't update the invoice when this is a replayed htlc.
	htlc, replayedHTLC := inv.Htlcs[ctx.circuitKey]
	if !replayedHTLC {
		return false, nil, nil
	}

	switch htlc.State {
	case HtlcStateCanceled:
		return true, ctx.failRes(ResultReplayToCanceled), nil

	case HtlcStateAccepted:
		return true, ctx.acceptRes(resultReplayToAccepted), nil

	case HtlcStateSettled:
		pre := inv.Terms.PaymentPreimage

		// Terms.PaymentPreimage will be nil for AMP invoices.
		// Set it to the HTLCs AMP Preimage instead.
		if pre == nil {
			pre = htlc.AMP.Preimage
		}

		return true, ctx.settleRes(
			*pre,
			ResultReplayToSettled,
		), nil

	default:
		return true, nil, errors.New("unknown htlc state")
	}
}

// updateInvoice is a callback for DB.UpdateInvoice that contains the invoice
// settlement logic. It returns a HTLC resolution that indicates what the
// outcome of the update was.
//
// NOTE: Make sure replayed HTLCs are always considered before calling this
// function.
func updateInvoice(ctx *invoiceUpdateCtx, inv *Invoice) (
	*InvoiceUpdateDesc, HtlcResolution, error) {

	// If no MPP payload was provided, then we expect this to be a keysend,
	// or a payment to an invoice created before we started to require the
	// MPP payload.
	if ctx.mpp == nil && ctx.pathID == nil {
		return updateLegacy(ctx, inv)
	}

	return updateMpp(ctx, inv)
}

// updateMpp is a callback for DB.UpdateInvoice that contains the invoice
// settlement logic for mpp payments.
func updateMpp(ctx *invoiceUpdateCtx, inv *Invoice) (*InvoiceUpdateDesc,
	HtlcResolution, error) {

	// Reject HTLCs to AMP invoices if they are missing an AMP payload, and
	// HTLCs to MPP invoices if they have an AMP payload.
	switch {
	case inv.Terms.Features.RequiresFeature(lnwire.AMPRequired) &&
		ctx.amp == nil:

		return nil, ctx.failRes(ResultHtlcInvoiceTypeMismatch), nil

	case !inv.Terms.Features.RequiresFeature(lnwire.AMPRequired) &&
		ctx.amp != nil:

		return nil, ctx.failRes(ResultHtlcInvoiceTypeMismatch), nil
	}

	setID := ctx.setID()

	var (
		totalAmt    = ctx.totalAmtMsat
		paymentAddr []byte
	)
	// If an MPP record is present, then the payment address and total
	// payment amount is extracted from it. Otherwise, the pathID is used
	// to extract the payment address.
	if ctx.mpp != nil {
		totalAmt = ctx.mpp.TotalMsat()
		payAddr := ctx.mpp.PaymentAddr()
		paymentAddr = payAddr[:]
	} else {
		paymentAddr = ctx.pathID[:]
	}

	// For storage, we don't really care where the custom records came from.
	// So we merge them together and store them in the same field.
	customRecords := lnwire.CustomRecords(
		ctx.customRecords,
	).MergedCopy(ctx.wireCustomRecords)

	// Start building the accept descriptor.
	acceptDesc := &HtlcAcceptDesc{
		Amt:           ctx.amtPaid,
		Expiry:        ctx.expiry,
		AcceptHeight:  ctx.currentHeight,
		MppTotalAmt:   totalAmt,
		CustomRecords: record.CustomSet(customRecords),
	}

	if ctx.amp != nil {
		acceptDesc.AMP = &InvoiceHtlcAMPData{
			Record:   *ctx.amp,
			Hash:     ctx.hash,
			Preimage: nil,
		}
	}

	// Only accept payments to open invoices. This behaviour differs from
	// non-mpp payments that are accepted even after the invoice is settled.
	// Because non-mpp payments don't have a payment address, this is needed
	// to thwart probing.
	if inv.State != ContractOpen {
		return nil, ctx.failRes(ResultInvoiceNotOpen), nil
	}

	// Check the payment address that authorizes the payment.
	if !bytes.Equal(paymentAddr, inv.Terms.PaymentAddr[:]) {
		return nil, ctx.failRes(ResultAddressMismatch), nil
	}

	// Don't accept zero-valued sets.
	if totalAmt == 0 {
		return nil, ctx.failRes(ResultHtlcSetTotalTooLow), nil
	}

	// Check that the total amt of the htlc set is high enough. In case this
	// is a zero-valued invoice, it will always be enough.
	if totalAmt < inv.Terms.Value {
		return nil, ctx.failRes(ResultHtlcSetTotalTooLow), nil
	}

	htlcSet := inv.HTLCSet(setID, HtlcStateAccepted)

	// Check whether total amt matches other HTLCs in the set.
	var newSetTotal lnwire.MilliSatoshi
	for _, htlc := range htlcSet {
		if totalAmt != htlc.MppTotalAmt {
			return nil, ctx.failRes(ResultHtlcSetTotalMismatch), nil
		}

		newSetTotal += htlc.Amt
	}

	// Add amount of new htlc.
	newSetTotal += ctx.amtPaid

	// The invoice is still open. Check the expiry.
	if ctx.expiry < uint32(ctx.currentHeight+ctx.finalCltvRejectDelta) {
		return nil, ctx.failRes(ResultExpiryTooSoon), nil
	}

	if ctx.expiry < uint32(ctx.currentHeight+inv.Terms.FinalCltvDelta) {
		return nil, ctx.failRes(ResultExpiryTooSoon), nil
	}

	if setID != nil && *setID == BlankPayAddr {
		return nil, ctx.failRes(ResultAmpError), nil
	}

	// Record HTLC in the invoice database.
	newHtlcs := map[CircuitKey]*HtlcAcceptDesc{
		ctx.circuitKey: acceptDesc,
	}

	update := InvoiceUpdateDesc{
		UpdateType: AddHTLCsUpdate,
		AddHtlcs:   newHtlcs,
	}

	// If the invoice cannot be settled yet, only record the htlc.
	setComplete := newSetTotal >= totalAmt
	if !setComplete {
		return &update, ctx.acceptRes(resultPartialAccepted), nil
	}

	// Check to see if we can settle or this is a hold invoice, and
	// we need to wait for the preimage.
	if inv.HodlInvoice {
		update.State = &InvoiceStateUpdateDesc{
			NewState: ContractAccepted,
		}
		return &update, ctx.acceptRes(resultAccepted), nil
	}

	var (
		htlcPreimages map[CircuitKey]lntypes.Preimage
		htlcPreimage  lntypes.Preimage
	)
	if ctx.amp != nil {
		var failRes *HtlcFailResolution
		htlcPreimages, failRes = reconstructAMPPreimages(ctx, htlcSet)
		if failRes != nil {
			update.UpdateType = CancelInvoiceUpdate
			update.State = &InvoiceStateUpdateDesc{
				NewState: ContractCanceled,
				SetID:    setID,
			}
			return &update, failRes, nil
		}

		// The preimage for _this_ HTLC will be the one with context's
		// circuit key.
		htlcPreimage = htlcPreimages[ctx.circuitKey]
	} else {
		htlcPreimage = *inv.Terms.PaymentPreimage
	}

	update.State = &InvoiceStateUpdateDesc{
		NewState:      ContractSettled,
		Preimage:      inv.Terms.PaymentPreimage,
		HTLCPreimages: htlcPreimages,
		SetID:         setID,
	}

	return &update, ctx.settleRes(htlcPreimage, ResultSettled), nil
}

// HTLCSet is a map of CircuitKey to InvoiceHTLC.
type HTLCSet = map[CircuitKey]*InvoiceHTLC

// HTLCPreimages is a map of CircuitKey to preimage.
type HTLCPreimages = map[CircuitKey]lntypes.Preimage

// reconstructAMPPreimages reconstructs the root seed for an AMP HTLC set and
// verifies that all derived child hashes match the payment hashes of the HTLCs
// in the set. This method is meant to be called after receiving the full amount
// committed to via mpp_total_msat. This method will return a fail resolution if
// any of the child hashes fail to match their corresponding HTLCs.
func reconstructAMPPreimages(ctx *invoiceUpdateCtx,
	htlcSet HTLCSet) (HTLCPreimages, *HtlcFailResolution) {

	// Create a slice containing all the child descriptors to be used for
	// reconstruction. This should include all HTLCs currently in the HTLC
	// set, plus the incoming HTLC.
	childDescs := make([]amp.ChildDesc, 0, 1+len(htlcSet))

	// Add the new HTLC's child descriptor at index 0.
	childDescs = append(childDescs, amp.ChildDesc{
		Share: ctx.amp.RootShare(),
		Index: ctx.amp.ChildIndex(),
	})

	// Next, construct an index mapping the position in childDescs to a
	// circuit key for all preexisting HTLCs.
	indexToCircuitKey := make(map[int]CircuitKey)

	// Add the child descriptor for each HTLC in the HTLC set, recording
	// it's position within the slice.
	var htlcSetIndex int
	for circuitKey, htlc := range htlcSet {
		childDescs = append(childDescs, amp.ChildDesc{
			Share: htlc.AMP.Record.RootShare(),
			Index: htlc.AMP.Record.ChildIndex(),
		})
		indexToCircuitKey[htlcSetIndex] = circuitKey
		htlcSetIndex++
	}

	// Using the child descriptors, reconstruct the root seed and derive the
	// child hash/preimage pairs for each of the HTLCs.
	children := amp.ReconstructChildren(childDescs...)

	// Validate that the derived child preimages match the hash of each
	// HTLC's respective hash.
	if ctx.hash != children[0].Hash {
		return nil, ctx.failRes(ResultAmpReconstruction)
	}
	for idx, child := range children[1:] {
		circuitKey := indexToCircuitKey[idx]
		htlc := htlcSet[circuitKey]
		if htlc.AMP.Hash != child.Hash {
			return nil, ctx.failRes(ResultAmpReconstruction)
		}
	}

	// Finally, construct the map of learned preimages indexed by circuit
	// key, so that they can be persisted along with each HTLC when updating
	// the invoice.
	htlcPreimages := make(map[CircuitKey]lntypes.Preimage)
	htlcPreimages[ctx.circuitKey] = children[0].Preimage
	for idx, child := range children[1:] {
		circuitKey := indexToCircuitKey[idx]
		htlcPreimages[circuitKey] = child.Preimage
	}

	return htlcPreimages, nil
}

// updateLegacy is a callback for DB.UpdateInvoice that contains the invoice
// settlement logic for legacy payments.
//
// NOTE: This function is only kept in place in order to be able to handle key
// send payments and any invoices we created in the past that are valid and
// still had the optional mpp bit set.
func updateLegacy(ctx *invoiceUpdateCtx,
	inv *Invoice) (*InvoiceUpdateDesc, HtlcResolution, error) {

	// If the invoice is already canceled, there is no further
	// checking to do.
	if inv.State == ContractCanceled {
		return nil, ctx.failRes(ResultInvoiceAlreadyCanceled), nil
	}

	// If an invoice amount is specified, check that enough is paid. Also
	// check this for duplicate payments if the invoice is already settled
	// or accepted. In case this is a zero-valued invoice, it will always be
	// enough.
	if ctx.amtPaid < inv.Terms.Value {
		return nil, ctx.failRes(ResultAmountTooLow), nil
	}

	// If the invoice had the required feature bit set at this point, then
	// if we're in this method it means that the remote party didn't supply
	// the expected payload. However if this is a keysend payment, then
	// we'll permit it to pass.
	_, isKeySend := ctx.customRecords[record.KeySendType]
	invoiceFeatures := inv.Terms.Features
	paymentAddrRequired := invoiceFeatures.RequiresFeature(
		lnwire.PaymentAddrRequired,
	)
	if !isKeySend && paymentAddrRequired {
		log.Warnf("Payment to pay_hash=%v doesn't include MPP "+
			"payload, rejecting", ctx.hash)
		return nil, ctx.failRes(ResultAddressMismatch), nil
	}

	// Don't allow settling the invoice with an old style
	// htlc if we are already in the process of gathering an
	// mpp set.
	for _, htlc := range inv.HTLCSet(nil, HtlcStateAccepted) {
		if htlc.MppTotalAmt > 0 {
			return nil, ctx.failRes(ResultMppInProgress), nil
		}
	}

	// The invoice is still open. Check the expiry.
	if ctx.expiry < uint32(ctx.currentHeight+ctx.finalCltvRejectDelta) {
		return nil, ctx.failRes(ResultExpiryTooSoon), nil
	}

	if ctx.expiry < uint32(ctx.currentHeight+inv.Terms.FinalCltvDelta) {
		return nil, ctx.failRes(ResultExpiryTooSoon), nil
	}

	// For storage, we don't really care where the custom records came from.
	// So we merge them together and store them in the same field.
	customRecords := lnwire.CustomRecords(
		ctx.customRecords,
	).MergedCopy(ctx.wireCustomRecords)

	// Record HTLC in the invoice database.
	newHtlcs := map[CircuitKey]*HtlcAcceptDesc{
		ctx.circuitKey: {
			Amt:           ctx.amtPaid,
			Expiry:        ctx.expiry,
			AcceptHeight:  ctx.currentHeight,
			CustomRecords: record.CustomSet(customRecords),
		},
	}

	update := InvoiceUpdateDesc{
		AddHtlcs:   newHtlcs,
		UpdateType: AddHTLCsUpdate,
	}

	// Don't update invoice state if we are accepting a duplicate payment.
	// We do accept or settle the HTLC.
	switch inv.State {
	case ContractAccepted:
		return &update, ctx.acceptRes(resultDuplicateToAccepted), nil

	case ContractSettled:
		return &update, ctx.settleRes(
			*inv.Terms.PaymentPreimage, ResultDuplicateToSettled,
		), nil
	}

	// Check to see if we can settle or this is an hold invoice and we need
	// to wait for the preimage.
	if inv.HodlInvoice {
		update.State = &InvoiceStateUpdateDesc{
			NewState: ContractAccepted,
		}

		return &update, ctx.acceptRes(resultAccepted), nil
	}

	update.State = &InvoiceStateUpdateDesc{
		NewState: ContractSettled,
		Preimage: inv.Terms.PaymentPreimage,
	}

	return &update, ctx.settleRes(
		*inv.Terms.PaymentPreimage, ResultSettled,
	), nil
}
