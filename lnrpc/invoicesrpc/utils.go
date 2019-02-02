package invoicesrpc

import (
	"encoding/hex"
	"fmt"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/lightningnetwork/lnd/zpay32"
)

// CreateRPCInvoice creates an *lnrpc.Invoice from the *channeldb.Invoice.
func CreateRPCInvoice(invoice *channeldb.Invoice,
	activeNetParams *chaincfg.Params) (*lnrpc.Invoice, error) {

	paymentRequest := string(invoice.PaymentRequest)
	decoded, err := zpay32.Decode(paymentRequest, activeNetParams)
	if err != nil {
		return nil, fmt.Errorf("unable to decode payment request: %v",
			err)
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

	// Expiry time will default to 3600 seconds if not specified
	// explicitly.
	expiry := int64(decoded.Expiry().Seconds())

	// The expiry will default to 9 blocks if not specified explicitly.
	cltvExpiry := decoded.MinFinalCLTVExpiry()

	// Convert between the `lnrpc` and `routing` types.
	routeHints := CreateRPCRouteHints(decoded.RouteHints)

	preimage := invoice.Terms.PaymentPreimage
	satAmt := invoice.Terms.Value.ToSatoshis()
	satAmtPaid := invoice.AmtPaid.ToSatoshis()

	isSettled := invoice.Terms.State == channeldb.ContractSettled

	var state lnrpc.Invoice_InvoiceState
	switch invoice.Terms.State {
	case channeldb.ContractOpen:
		state = lnrpc.Invoice_OPEN
	case channeldb.ContractSettled:
		state = lnrpc.Invoice_SETTLED
	default:
		return nil, fmt.Errorf("unknown invoice state")
	}

	return &lnrpc.Invoice{
		Memo:            string(invoice.Memo[:]),
		Receipt:         invoice.Receipt[:],
		RHash:           decoded.PaymentHash[:],
		RPreimage:       preimage[:],
		Value:           int64(satAmt),
		CreationDate:    invoice.CreationDate.Unix(),
		SettleDate:      settleDate,
		Settled:         isSettled,
		PaymentRequest:  paymentRequest,
		DescriptionHash: descHash,
		Expiry:          expiry,
		CltvExpiry:      cltvExpiry,
		FallbackAddr:    fallbackAddr,
		RouteHints:      routeHints,
		AddIndex:        invoice.AddIndex,
		Private:         len(routeHints) > 0,
		SettleIndex:     invoice.SettleIndex,
		AmtPaidSat:      int64(satAmtPaid),
		AmtPaidMsat:     int64(invoice.AmtPaid),
		AmtPaid:         int64(invoice.AmtPaid),
		State:           state,
	}, nil
}

// CreateRPCRouteHints takes in the decoded form of an invoice's route hints
// and converts them into the lnrpc type.
func CreateRPCRouteHints(routeHints [][]routing.HopHint) []*lnrpc.RouteHint {
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
