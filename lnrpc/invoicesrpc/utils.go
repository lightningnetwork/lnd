package invoicesrpc

import (
	"cmp"
	"encoding/hex"
	"fmt"
	"slices"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/zpay32"
)

// decodePayReq decodes the invoice payment request if present. This is needed,
// because not all information is stored in dedicated invoice fields. If there
// is no payment request present, a dummy request will be returned. This can
// happen with just-in-time inserted keysend invoices.
func decodePayReq(invoice *invoices.Invoice,
	activeNetParams *chaincfg.Params) (*zpay32.Invoice, error) {

	paymentRequest := string(invoice.PaymentRequest)
	if paymentRequest == "" {
		preimage := invoice.Terms.PaymentPreimage
		if preimage == nil {
			return &zpay32.Invoice{}, nil
		}
		hash := [32]byte(preimage.Hash())
		return &zpay32.Invoice{
			PaymentHash: &hash,
		}, nil
	}

	var err error
	decoded, err := zpay32.Decode(paymentRequest, activeNetParams)
	if err != nil {
		return nil, fmt.Errorf("unable to decode payment "+
			"request: %v", err)
	}
	return decoded, nil
}

// CreateRPCInvoice creates an *lnrpc.Invoice from the *invoices.Invoice.
func CreateRPCInvoice(invoice *invoices.Invoice,
	activeNetParams *chaincfg.Params) (*lnrpc.Invoice, error) {

	decoded, err := decodePayReq(invoice, activeNetParams)
	if err != nil {
		return nil, err
	}

	var rHash []byte
	if decoded.PaymentHash != nil {
		rHash = decoded.PaymentHash[:]
	}

	var descHash []byte
	if decoded.DescriptionHash != nil {
		descHash = decoded.DescriptionHash[:]
	}

	fallbackAddr := ""
	if decoded.FallbackAddr != nil {
		fallbackAddr = decoded.FallbackAddr.String()
	}

	settleDate := int64(0)
	if !invoice.SettleDate.IsZero() {
		settleDate = invoice.SettleDate.Unix()
	}

	// Convert between the `lnrpc` and `routing` types.
	routeHints := CreateRPCRouteHints(decoded.RouteHints)

	preimage := invoice.Terms.PaymentPreimage
	satAmt := invoice.Terms.Value.ToSatoshis()
	satAmtPaid := invoice.AmtPaid.ToSatoshis()

	isSettled := invoice.State == invoices.ContractSettled

	var state lnrpc.Invoice_InvoiceState
	switch invoice.State {
	case invoices.ContractOpen:
		state = lnrpc.Invoice_OPEN

	case invoices.ContractSettled:
		state = lnrpc.Invoice_SETTLED

	case invoices.ContractCanceled:
		state = lnrpc.Invoice_CANCELED

	case invoices.ContractAccepted:
		state = lnrpc.Invoice_ACCEPTED

	default:
		return nil, fmt.Errorf("unknown invoice state %v",
			invoice.State)
	}

	rpcHtlcs := make([]*lnrpc.InvoiceHTLC, 0, len(invoice.Htlcs))
	for key, htlc := range invoice.Htlcs {
		var state lnrpc.InvoiceHTLCState
		switch htlc.State {
		case invoices.HtlcStateAccepted:
			state = lnrpc.InvoiceHTLCState_ACCEPTED
		case invoices.HtlcStateSettled:
			state = lnrpc.InvoiceHTLCState_SETTLED
		case invoices.HtlcStateCanceled:
			state = lnrpc.InvoiceHTLCState_CANCELED
		default:
			return nil, fmt.Errorf("unknown state %v", htlc.State)
		}

		rpcHtlc := lnrpc.InvoiceHTLC{
			ChanId:          key.ChanID.ToUint64(),
			HtlcIndex:       key.HtlcID,
			AcceptHeight:    int32(htlc.AcceptHeight),
			AcceptTime:      htlc.AcceptTime.Unix(),
			ExpiryHeight:    int32(htlc.Expiry),
			AmtMsat:         uint64(htlc.Amt),
			State:           state,
			CustomRecords:   htlc.CustomRecords,
			MppTotalAmtMsat: uint64(htlc.MppTotalAmt),
		}

		// The custom channel data is currently just the raw bytes of
		// the encoded custom records.
		customData, err := lnwire.CustomRecords(
			htlc.CustomRecords,
		).Serialize()
		if err != nil {
			return nil, err
		}
		rpcHtlc.CustomChannelData = customData

		// Populate any fields relevant to AMP payments.
		if htlc.AMP != nil {
			rootShare := htlc.AMP.Record.RootShare()
			setID := htlc.AMP.Record.SetID()

			var preimage []byte
			if htlc.AMP.Preimage != nil {
				preimage = htlc.AMP.Preimage[:]
			}

			rpcHtlc.Amp = &lnrpc.AMP{
				RootShare:  rootShare[:],
				SetId:      setID[:],
				ChildIndex: htlc.AMP.Record.ChildIndex(),
				Hash:       htlc.AMP.Hash[:],
				Preimage:   preimage,
			}
		}

		// Only report resolved times if htlc is resolved.
		if htlc.State != invoices.HtlcStateAccepted {
			rpcHtlc.ResolveTime = htlc.ResolveTime.Unix()
		}

		rpcHtlcs = append(rpcHtlcs, &rpcHtlc)
	}

	// Perform an inplace sort of the HTLCs to ensure they are ordered.
	slices.SortFunc(rpcHtlcs, func(i, j *lnrpc.InvoiceHTLC) int {
		return cmp.Compare(i.HtlcIndex, j.HtlcIndex)
	})

	rpcInvoice := &lnrpc.Invoice{
		Memo:            string(invoice.Memo),
		RHash:           rHash,
		Value:           int64(satAmt),
		ValueMsat:       int64(invoice.Terms.Value),
		CreationDate:    invoice.CreationDate.Unix(),
		SettleDate:      settleDate,
		Settled:         isSettled,
		PaymentRequest:  string(invoice.PaymentRequest),
		DescriptionHash: descHash,
		Expiry:          int64(invoice.Terms.Expiry.Seconds()),
		CltvExpiry:      uint64(invoice.Terms.FinalCltvDelta),
		FallbackAddr:    fallbackAddr,
		RouteHints:      routeHints,
		AddIndex:        invoice.AddIndex,
		Private:         len(routeHints) > 0,
		SettleIndex:     invoice.SettleIndex,
		AmtPaidSat:      int64(satAmtPaid),
		AmtPaidMsat:     int64(invoice.AmtPaid),
		AmtPaid:         int64(invoice.AmtPaid),
		// This will be set to SETTLED if at least one of the AMP Sets
		// is settled (see below).
		State:       state,
		Htlcs:       rpcHtlcs,
		Features:    CreateRPCFeatures(invoice.Terms.Features),
		IsKeysend:   invoice.IsKeysend(),
		PaymentAddr: invoice.Terms.PaymentAddr[:],
		IsAmp:       invoice.IsAMP(),
		IsBlinded:   invoice.IsBlinded(),
	}

	rpcInvoice.AmpInvoiceState = make(map[string]*lnrpc.AMPInvoiceState)
	for setID, ampState := range invoice.AMPState {
		setIDStr := hex.EncodeToString(setID[:])

		var state lnrpc.InvoiceHTLCState
		switch ampState.State {
		case invoices.HtlcStateAccepted:
			state = lnrpc.InvoiceHTLCState_ACCEPTED
		case invoices.HtlcStateSettled:
			state = lnrpc.InvoiceHTLCState_SETTLED
		case invoices.HtlcStateCanceled:
			state = lnrpc.InvoiceHTLCState_CANCELED
		default:
			return nil, fmt.Errorf("unknown state %v", ampState.State)
		}

		rpcInvoice.AmpInvoiceState[setIDStr] = &lnrpc.AMPInvoiceState{
			State:       state,
			SettleIndex: ampState.SettleIndex,
			SettleTime:  ampState.SettleDate.Unix(),
			AmtPaidMsat: int64(ampState.AmtPaid),
		}

		// If at least one of the present HTLC sets show up as being
		// settled, then we'll mark the invoice itself as being
		// settled.
		if ampState.State == invoices.HtlcStateSettled {
			rpcInvoice.Settled = true // nolint:staticcheck
			rpcInvoice.State = lnrpc.Invoice_SETTLED
		}
	}

	if preimage != nil {
		rpcInvoice.RPreimage = preimage[:]
	}

	return rpcInvoice, nil
}

// CreateRPCFeatures maps a feature vector into a list of lnrpc.Features.
func CreateRPCFeatures(fv *lnwire.FeatureVector) map[uint32]*lnrpc.Feature {
	if fv == nil {
		return nil
	}

	features := fv.Features()
	rpcFeatures := make(map[uint32]*lnrpc.Feature, len(features))
	for bit := range features {
		rpcFeatures[uint32(bit)] = &lnrpc.Feature{
			Name:       fv.Name(bit),
			IsRequired: bit.IsRequired(),
			IsKnown:    fv.IsKnown(bit),
		}
	}

	return rpcFeatures
}

// CreateRPCRouteHints takes in the decoded form of an invoice's route hints
// and converts them into the lnrpc type.
func CreateRPCRouteHints(routeHints [][]zpay32.HopHint) []*lnrpc.RouteHint {
	var res []*lnrpc.RouteHint

	for _, route := range routeHints {
		hopHints := make([]*lnrpc.HopHint, 0, len(route))
		for _, hop := range route {
			pubKey := hex.EncodeToString(
				hop.NodeID.SerializeCompressed(),
			)

			hint := &lnrpc.HopHint{
				NodeId:                    pubKey,
				ChanId:                    hop.ChannelID,
				FeeBaseMsat:               hop.FeeBaseMSat,
				FeeProportionalMillionths: hop.FeeProportionalMillionths,
				CltvExpiryDelta:           uint32(hop.CLTVExpiryDelta),
			}

			hopHints = append(hopHints, hint)
		}

		routeHint := &lnrpc.RouteHint{HopHints: hopHints}
		res = append(res, routeHint)
	}

	return res
}

// CreateRPCBlindedPayments takes a set of zpay32.BlindedPaymentPath and
// converts them into a set of lnrpc.BlindedPaymentPaths.
func CreateRPCBlindedPayments(blindedPaths []*zpay32.BlindedPaymentPath) (
	[]*lnrpc.BlindedPaymentPath, error) {

	var res []*lnrpc.BlindedPaymentPath
	for _, path := range blindedPaths {
		features := path.Features.Features()
		var featuresSlice []lnrpc.FeatureBit
		for feature := range features {
			featuresSlice = append(
				featuresSlice, lnrpc.FeatureBit(feature),
			)
		}

		if len(path.Hops) == 0 {
			return nil, fmt.Errorf("each blinded path must " +
				"contain at least one hop")
		}

		var hops []*lnrpc.BlindedHop
		for _, hop := range path.Hops {
			blindedNodeID := hop.BlindedNodePub.
				SerializeCompressed()
			hops = append(hops, &lnrpc.BlindedHop{
				BlindedNode:   blindedNodeID,
				EncryptedData: hop.CipherText,
			})
		}

		introNode := path.Hops[0].BlindedNodePub
		firstBlindingPoint := path.FirstEphemeralBlindingPoint

		blindedPath := &lnrpc.BlindedPath{
			IntroductionNode: introNode.SerializeCompressed(),
			BlindingPoint: firstBlindingPoint.
				SerializeCompressed(),
			BlindedHops: hops,
		}

		res = append(res, &lnrpc.BlindedPaymentPath{
			BlindedPath:         blindedPath,
			BaseFeeMsat:         uint64(path.FeeBaseMsat),
			ProportionalFeeRate: path.FeeRate,
			TotalCltvDelta:      uint32(path.CltvExpiryDelta),
			HtlcMinMsat:         path.HTLCMinMsat,
			HtlcMaxMsat:         path.HTLCMaxMsat,
			Features:            featuresSlice,
		})
	}

	return res, nil
}

// CreateZpay32HopHints takes in the lnrpc form of route hints and converts them
// into an invoice decoded form.
func CreateZpay32HopHints(routeHints []*lnrpc.RouteHint) ([][]zpay32.HopHint, error) {
	var res [][]zpay32.HopHint
	for _, route := range routeHints {
		hopHints := make([]zpay32.HopHint, 0, len(route.HopHints))
		for _, hop := range route.HopHints {
			pubKeyBytes, err := hex.DecodeString(hop.NodeId)
			if err != nil {
				return nil, err
			}
			p, err := btcec.ParsePubKey(pubKeyBytes)
			if err != nil {
				return nil, err
			}
			hopHints = append(hopHints, zpay32.HopHint{
				NodeID:                    p,
				ChannelID:                 hop.ChanId,
				FeeBaseMSat:               hop.FeeBaseMsat,
				FeeProportionalMillionths: hop.FeeProportionalMillionths,
				CLTVExpiryDelta:           uint16(hop.CltvExpiryDelta),
			})
		}
		res = append(res, hopHints)
	}
	return res, nil
}
