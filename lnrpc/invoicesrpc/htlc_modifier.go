package invoicesrpc

import (
	"fmt"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lnwire"
)

// htlcModifier is a helper struct that handles the lifecycle of an RPC invoice
// HTLC modifier server instance.
//
// This struct handles passing send and receive RPC messages between the client
// and the invoice service.
type htlcModifier struct {
	// chainParams is required to properly marshall an invoice for RPC.
	chainParams *chaincfg.Params

	// serverStream is a bidirectional RPC server stream to send invoices to
	// a client and receive accept responses from the client.
	serverStream Invoices_HtlcModifierServer
}

// newHtlcModifier creates a new RPC invoice HTLC modifier handler.
func newHtlcModifier(params *chaincfg.Params,
	serverStream Invoices_HtlcModifierServer) *htlcModifier {

	return &htlcModifier{
		chainParams:  params,
		serverStream: serverStream,
	}
}

// onIntercept is called when an invoice HTLC is intercepted by the invoice HTLC
// modifier. This method sends the invoice and the current HTLC to the client.
func (r *htlcModifier) onIntercept(
	req invoices.HtlcModifyRequest) (*invoices.HtlcModifyResponse, error) {

	// Convert the circuit key to an RPC circuit key.
	rpcCircuitKey := &CircuitKey{
		ChanId: req.ExitHtlcCircuitKey.ChanID.ToUint64(),
		HtlcId: req.ExitHtlcCircuitKey.HtlcID,
	}

	// Convert the invoice to an RPC invoice.
	rpcInvoice, err := CreateRPCInvoice(&req.Invoice, r.chainParams)
	if err != nil {
		return nil, err
	}

	// Send the modification request to the client.
	err = r.serverStream.Send(&HtlcModifyRequest{
		Invoice:                   rpcInvoice,
		ExitHtlcCircuitKey:        rpcCircuitKey,
		ExitHtlcAmt:               uint64(req.ExitHtlcAmt),
		ExitHtlcExpiry:            req.ExitHtlcExpiry,
		CurrentHeight:             req.CurrentHeight,
		ExitHtlcWireCustomRecords: req.WireCustomRecords,
	})
	if err != nil {
		return nil, err
	}

	// Then wait for the client to respond.
	resp, err := r.serverStream.Recv()
	if err != nil {
		return nil, err
	}

	if resp.CircuitKey == nil {
		return nil, fmt.Errorf("missing circuit key")
	}

	log.Tracef("Resolving invoice HTLC modifier response %v", resp)

	// Pass the resolution to the modifier.
	var amtPaid lnwire.MilliSatoshi
	if resp.AmtPaid != nil {
		amtPaid = lnwire.MilliSatoshi(*resp.AmtPaid)
	}

	return &invoices.HtlcModifyResponse{
		AmountPaid: amtPaid,
		CancelSet:  resp.CancelSet,
	}, nil
}
