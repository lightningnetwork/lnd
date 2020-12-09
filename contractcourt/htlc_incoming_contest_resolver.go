package contractcourt

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/htlcswitch/hop"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
)

// htlcIncomingContestResolver is a ContractResolver that's able to resolve an
// incoming HTLC that is still contested. An HTLC is still contested, if at the
// time of commitment broadcast, we don't know of the preimage for it yet, and
// it hasn't expired. In this case, we can resolve the HTLC if we learn of the
// preimage, otherwise the remote party will sweep it after it expires.
//
// TODO(roasbeef): just embed the other resolver?
type htlcIncomingContestResolver struct {
	// htlcExpiry is the absolute expiry of this incoming HTLC. We use this
	// value to determine if we can exit early as if the HTLC times out,
	// before we learn of the preimage then we can't claim it on chain
	// successfully.
	htlcExpiry uint32

	// htlcSuccessResolver is the inner resolver that may be utilized if we
	// learn of the preimage.
	*htlcSuccessResolver
}

// newIncomingContestResolver instantiates a new incoming htlc contest resolver.
func newIncomingContestResolver(
	res lnwallet.IncomingHtlcResolution, broadcastHeight uint32,
	htlc channeldb.HTLC, resCfg ResolverConfig) *htlcIncomingContestResolver {

	success := newSuccessResolver(
		res, broadcastHeight, htlc, resCfg,
	)

	return &htlcIncomingContestResolver{
		htlcExpiry:          htlc.RefundTimeout,
		htlcSuccessResolver: success,
	}
}

// Resolve attempts to resolve this contract. As we don't yet know of the
// preimage for the contract, we'll wait for one of two things to happen:
//
//   1. We learn of the preimage! In this case, we can sweep the HTLC incoming
//      and ensure that if this was a multi-hop HTLC we are made whole. In this
//      case, an additional ContractResolver will be returned to finish the
//      job.
//
//   2. The HTLC expires. If this happens, then the contract is fully resolved
//      as we have no remaining actions left at our disposal.
//
// NOTE: Part of the ContractResolver interface.
func (h *htlcIncomingContestResolver) Resolve() (ContractResolver, error) {
	// If we're already full resolved, then we don't have anything further
	// to do.
	if h.resolved {
		return nil, nil
	}

	// First try to parse the payload. If that fails, we can stop resolution
	// now.
	payload, err := h.decodePayload()
	if err != nil {
		log.Debugf("ChannelArbitrator(%v): cannot decode payload of "+
			"htlc %v", h.ChanPoint, h.HtlcPoint())

		// If we've locked in an htlc with an invalid payload on our
		// commitment tx, we don't need to resolve it. The other party
		// will time it out and get their funds back. This situation can
		// present itself when we crash before processRemoteAdds in the
		// link has ran.
		h.resolved = true

		// We write a report to disk that indicates we could not decode
		// the htlc.
		resReport := h.report().resolverReport(
			nil, channeldb.ResolverTypeIncomingHtlc,
			channeldb.ResolverOutcomeAbandoned,
		)
		return nil, h.PutResolverReport(nil, resReport)
	}

	// Register for block epochs. After registration, the current height
	// will be sent on the channel immediately.
	blockEpochs, err := h.Notifier.RegisterBlockEpochNtfn(nil)
	if err != nil {
		return nil, err
	}
	defer blockEpochs.Cancel()

	var currentHeight int32
	select {
	case newBlock, ok := <-blockEpochs.Epochs:
		if !ok {
			return nil, errResolverShuttingDown
		}
		currentHeight = newBlock.Height
	case <-h.quit:
		return nil, errResolverShuttingDown
	}

	// We'll first check if this HTLC has been timed out, if so, we can
	// return now and mark ourselves as resolved. If we're past the point of
	// expiry of the HTLC, then at this point the sender can sweep it, so
	// we'll end our lifetime. Here we deliberately forego the chance that
	// the sender doesn't sweep and we already have or will learn the
	// preimage. Otherwise the resolver could potentially stay active
	// indefinitely and the channel will never close properly.
	if uint32(currentHeight) >= h.htlcExpiry {
		// TODO(roasbeef): should also somehow check if outgoing is
		// resolved or not
		//  * may need to hook into the circuit map
		//  * can't timeout before the outgoing has been

		log.Infof("%T(%v): HTLC has timed out (expiry=%v, height=%v), "+
			"abandoning", h, h.htlcResolution.ClaimOutpoint,
			h.htlcExpiry, currentHeight)
		h.resolved = true

		// Finally, get our report and checkpoint our resolver with a
		// timeout outcome report.
		report := h.report().resolverReport(
			nil, channeldb.ResolverTypeIncomingHtlc,
			channeldb.ResolverOutcomeTimeout,
		)
		return nil, h.Checkpoint(h, report)
	}

	// applyPreimage is a helper function that will populate our internal
	// resolver with the preimage we learn of. This should be called once
	// the preimage is revealed so the inner resolver can properly complete
	// its duties. The error return value indicates whether the preimage
	// was properly applied.
	applyPreimage := func(preimage lntypes.Preimage) error {
		// Sanity check to see if this preimage matches our htlc. At
		// this point it should never happen that it does not match.
		if !preimage.Matches(h.htlc.RHash) {
			return errors.New("preimage does not match hash")
		}

		// Update htlcResolution with the matching preimage.
		h.htlcResolution.Preimage = preimage

		log.Infof("%T(%v): extracted preimage=%v from beacon!", h,
			h.htlcResolution.ClaimOutpoint, preimage)

		// If this is our commitment transaction, then we'll need to
		// populate the witness for the second-level HTLC transaction.
		if h.htlcResolution.SignedSuccessTx != nil {
			// Within the witness for the success transaction, the
			// preimage is the 4th element as it looks like:
			//
			//  * <sender sig> <recvr sig> <preimage> <witness script>
			//
			// We'll populate it within the witness, as since this
			// was a "contest" resolver, we didn't yet know of the
			// preimage.
			h.htlcResolution.SignedSuccessTx.TxIn[0].Witness[3] = preimage[:]
		}

		return nil
	}

	// Define a closure to process htlc resolutions either directly or
	// triggered by future notifications.
	processHtlcResolution := func(e invoices.HtlcResolution) (
		ContractResolver, error) {

		// Take action based on the type of resolution we have
		// received.
		switch resolution := e.(type) {

		// If the htlc resolution was a settle, apply the
		// preimage and return a success resolver.
		case *invoices.HtlcSettleResolution:
			err := applyPreimage(resolution.Preimage)
			if err != nil {
				return nil, err
			}

			return h.htlcSuccessResolver, nil

		// If the htlc was failed, mark the htlc as
		// resolved.
		case *invoices.HtlcFailResolution:
			log.Infof("%T(%v): Exit hop HTLC canceled "+
				"(expiry=%v, height=%v), abandoning", h,
				h.htlcResolution.ClaimOutpoint,
				h.htlcExpiry, currentHeight)

			h.resolved = true

			// Checkpoint our resolver with an abandoned outcome
			// because we take no further action on this htlc.
			report := h.report().resolverReport(
				nil, channeldb.ResolverTypeIncomingHtlc,
				channeldb.ResolverOutcomeAbandoned,
			)
			return nil, h.Checkpoint(h, report)

		// Error if the resolution type is unknown, we are only
		// expecting settles and fails.
		default:
			return nil, fmt.Errorf("unknown resolution"+
				" type: %v", e)
		}
	}

	var (
		hodlChan       chan interface{}
		witnessUpdates <-chan lntypes.Preimage
	)
	if payload.FwdInfo.NextHop == hop.Exit {
		// Create a buffered hodl chan to prevent deadlock.
		hodlChan = make(chan interface{}, 1)

		// Notify registry that we are potentially resolving as an exit
		// hop on-chain. If this HTLC indeed pays to an existing
		// invoice, the invoice registry will tell us what to do with
		// the HTLC. This is identical to HTLC resolution in the link.
		circuitKey := channeldb.CircuitKey{
			ChanID: h.ShortChanID,
			HtlcID: h.htlc.HtlcIndex,
		}

		resolution, err := h.Registry.NotifyExitHopHtlc(
			h.htlc.RHash, h.htlc.Amt, h.htlcExpiry, currentHeight,
			circuitKey, hodlChan, payload,
		)
		if err != nil {
			return nil, err
		}

		defer h.Registry.HodlUnsubscribeAll(hodlChan)

		// Take action based on the resolution we received. If the htlc
		// was settled, or a htlc for a known invoice failed we can
		// resolve it directly. If the resolution is nil, the htlc was
		// neither accepted nor failed, so we cannot take action yet.
		switch res := resolution.(type) {
		case *invoices.HtlcFailResolution:
			// In the case where the htlc failed, but the invoice
			// was known to the registry, we can directly resolve
			// the htlc.
			if res.Outcome != invoices.ResultInvoiceNotFound {
				return processHtlcResolution(resolution)
			}

		// If we settled the htlc, we can resolve it.
		case *invoices.HtlcSettleResolution:
			return processHtlcResolution(resolution)

		// If the resolution is nil, the htlc was neither settled nor
		// failed so we cannot take action at present.
		case nil:

		default:
			return nil, fmt.Errorf("unknown htlc resolution type: %T",
				resolution)
		}
	} else {
		// If the HTLC hasn't expired yet, then we may still be able to
		// claim it if we learn of the pre-image, so we'll subscribe to
		// the preimage database to see if it turns up, or the HTLC
		// times out.
		//
		// NOTE: This is done BEFORE opportunistically querying the db,
		// to ensure the preimage can't be delivered between querying
		// and registering for the preimage subscription.
		preimageSubscription := h.PreimageDB.SubscribeUpdates()
		defer preimageSubscription.CancelSubscription()

		// With the epochs and preimage subscriptions initialized, we'll
		// query to see if we already know the preimage.
		preimage, ok := h.PreimageDB.LookupPreimage(h.htlc.RHash)
		if ok {
			// If we do, then this means we can claim the HTLC!
			// However, we don't know how to ourselves, so we'll
			// return our inner resolver which has the knowledge to
			// do so.
			if err := applyPreimage(preimage); err != nil {
				return nil, err
			}

			return h.htlcSuccessResolver, nil
		}

		witnessUpdates = preimageSubscription.WitnessUpdates
	}

	for {
		select {
		case preimage := <-witnessUpdates:
			// We received a new preimage, but we need to ignore
			// all except the preimage we are waiting for.
			if !preimage.Matches(h.htlc.RHash) {
				continue
			}

			if err := applyPreimage(preimage); err != nil {
				return nil, err
			}

			// We've learned of the preimage and this information
			// has been added to our inner resolver. We return it so
			// it can continue contract resolution.
			return h.htlcSuccessResolver, nil

		case hodlItem := <-hodlChan:
			htlcResolution := hodlItem.(invoices.HtlcResolution)
			return processHtlcResolution(htlcResolution)

		case newBlock, ok := <-blockEpochs.Epochs:
			if !ok {
				return nil, errResolverShuttingDown
			}

			// If this new height expires the HTLC, then this means
			// we never found out the preimage, so we can mark
			// resolved and exit.
			newHeight := uint32(newBlock.Height)
			if newHeight >= h.htlcExpiry {
				log.Infof("%T(%v): HTLC has timed out "+
					"(expiry=%v, height=%v), abandoning", h,
					h.htlcResolution.ClaimOutpoint,
					h.htlcExpiry, currentHeight)
				h.resolved = true

				report := h.report().resolverReport(
					nil,
					channeldb.ResolverTypeIncomingHtlc,
					channeldb.ResolverOutcomeTimeout,
				)
				return nil, h.Checkpoint(h, report)
			}

		case <-h.quit:
			return nil, errResolverShuttingDown
		}
	}
}

// report returns a report on the resolution state of the contract.
func (h *htlcIncomingContestResolver) report() *ContractReport {
	// No locking needed as these values are read-only.

	finalAmt := h.htlc.Amt.ToSatoshis()
	if h.htlcResolution.SignedSuccessTx != nil {
		finalAmt = btcutil.Amount(
			h.htlcResolution.SignedSuccessTx.TxOut[0].Value,
		)
	}

	return &ContractReport{
		Outpoint:       h.htlcResolution.ClaimOutpoint,
		Type:           ReportOutputIncomingHtlc,
		Amount:         finalAmt,
		MaturityHeight: h.htlcExpiry,
		LimboBalance:   finalAmt,
		Stage:          1,
	}
}

// Stop signals the resolver to cancel any current resolution processes, and
// suspend.
//
// NOTE: Part of the ContractResolver interface.
func (h *htlcIncomingContestResolver) Stop() {
	close(h.quit)
}

// IsResolved returns true if the stored state in the resolve is fully
// resolved. In this case the target output can be forgotten.
//
// NOTE: Part of the ContractResolver interface.
func (h *htlcIncomingContestResolver) IsResolved() bool {
	return h.resolved
}

// Encode writes an encoded version of the ContractResolver into the passed
// Writer.
//
// NOTE: Part of the ContractResolver interface.
func (h *htlcIncomingContestResolver) Encode(w io.Writer) error {
	// We'll first write out the one field unique to this resolver.
	if err := binary.Write(w, endian, h.htlcExpiry); err != nil {
		return err
	}

	// Then we'll write out our internal resolver.
	return h.htlcSuccessResolver.Encode(w)
}

// newIncomingContestResolverFromReader attempts to decode an encoded ContractResolver
// from the passed Reader instance, returning an active ContractResolver
// instance.
func newIncomingContestResolverFromReader(r io.Reader, resCfg ResolverConfig) (
	*htlcIncomingContestResolver, error) {

	h := &htlcIncomingContestResolver{}

	// We'll first read the one field unique to this resolver.
	if err := binary.Read(r, endian, &h.htlcExpiry); err != nil {
		return nil, err
	}

	// Then we'll decode our internal resolver.
	successResolver, err := newSuccessResolverFromReader(r, resCfg)
	if err != nil {
		return nil, err
	}
	h.htlcSuccessResolver = successResolver

	return h, nil
}

// Supplement adds additional information to the resolver that is required
// before Resolve() is called.
//
// NOTE: Part of the htlcContractResolver interface.
func (h *htlcIncomingContestResolver) Supplement(htlc channeldb.HTLC) {
	h.htlc = htlc
}

// decodePayload (re)decodes the hop payload of a received htlc.
func (h *htlcIncomingContestResolver) decodePayload() (*hop.Payload, error) {

	onionReader := bytes.NewReader(h.htlc.OnionBlob)
	iterator, err := h.OnionProcessor.ReconstructHopIterator(
		onionReader, h.htlc.RHash[:],
	)
	if err != nil {
		return nil, err
	}

	return iterator.HopPayload()
}

// A compile time assertion to ensure htlcIncomingContestResolver meets the
// ContractResolver interface.
var _ htlcContractResolver = (*htlcIncomingContestResolver)(nil)
