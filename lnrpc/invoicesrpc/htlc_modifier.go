package invoicesrpc

import (
	"fmt"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lnwire"
)

// htlcModifierConfig contains the configuration for an RPC invoice HTLC
// modifier server.
type htlcModifierConfig struct {
	// chainParams is required to properly marshall an invoice for RPC.
	chainParams *chaincfg.Params

	// serverStream is a bidirectional RPC server stream to send invoices to
	// a client and receive accept responses from the client.
	serverStream Invoices_HtlcModifierServer

	// modificationInterceptor is the HTLC modification interceptor that
	// will be used to intercept and resolve invoice HTLCs.
	modificationInterceptor invoices.HtlcModifier
}

// htlcModifier is a helper struct that handles the lifecycle of an RPC invoice
// HTLC modifier server instance.
//
// This struct handles passing send and receive RPC messages between the client
// and the invoice service.
type htlcModifier struct {
	// cfg contains the configuration for the invoice HTLC modifier.
	cfg htlcModifierConfig
}

// newHtlcModifier creates a new RPC invoice HTLC modifier handler.
func newHtlcModifier(cfg htlcModifierConfig) *htlcModifier {
	return &htlcModifier{
		cfg: cfg,
	}
}

// run sends the intercepted invoice HTLCs to the client and receives the
// corresponding responses.
func (r *htlcModifier) run() error {
	// Register our invoice modifier.
	r.cfg.modificationInterceptor.SetClientCallback(r.onIntercept)
	defer r.cfg.modificationInterceptor.SetClientCallback(nil)

	// Listen for a response from the client in a loop.
	for {
		resp, err := r.cfg.serverStream.Recv()
		if err != nil {
			return err
		}

		log.Tracef("Received invoice HTLC modifier response %v", resp)

		if err := r.resolveFromClient(resp); err != nil {
			return err
		}
	}
}

// onIntercept is called when an invoice HTLC is intercepted by the invoice HTLC
// modifier. This method sends the invoice and the current HTLC to the client.
func (r *htlcModifier) onIntercept(req invoices.HtlcModifyRequest) error {
	// Convert the circuit key to an RPC circuit key.
	rpcCircuitKey := &CircuitKey{
		ChanId: req.ExitHtlcCircuitKey.ChanID.ToUint64(),
		HtlcId: req.ExitHtlcCircuitKey.HtlcID,
	}

	// Convert the invoice to an RPC invoice.
	rpcInvoice, err := CreateRPCInvoice(&req.Invoice, r.cfg.chainParams)
	if err != nil {
		return err
	}

	return r.cfg.serverStream.Send(&HtlcModifyRequest{
		Invoice:                   rpcInvoice,
		ExitHtlcCircuitKey:        rpcCircuitKey,
		ExitHtlcAmt:               uint64(req.ExitHtlcAmt),
		ExitHtlcExpiry:            req.ExitHtlcExpiry,
		CurrentHeight:             req.CurrentHeight,
		ExitHtlcWireCustomRecords: req.WireCustomRecords,
	})
}

// resolveFromClient handles an invoice HTLC modification received from the
// client.
func (r *htlcModifier) resolveFromClient(
	in *HtlcModifyResponse) error {

	log.Tracef("Resolving invoice HTLC modifier response %v", in)

	if in.CircuitKey == nil {
		return fmt.Errorf("missing circuit key")
	}

	circuitKey := invoices.CircuitKey{
		ChanID: lnwire.NewShortChanIDFromInt(in.CircuitKey.ChanId),
		HtlcID: in.CircuitKey.HtlcId,
	}

	// Pass the resolution to the modifier.
	return r.cfg.modificationInterceptor.Modify(
		circuitKey, lnwire.MilliSatoshi(in.AmtPaid),
	)
}
