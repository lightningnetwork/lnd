package invoicesrpc

import (
	"encoding/hex"
	"fmt"

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
		State:           state,
		Htlcs:           rpcHtlcs,
		Features:        CreateRPCFeatures(invoice.Terms.Features),
		IsKeysend:       invoice.IsKeysend(),
		PaymentAddr:     invoice.Terms.PaymentAddr[:],
		IsAmp:           invoice.IsAMP(),
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
