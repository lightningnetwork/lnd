package invoicesrpc

import (
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/tlv"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// invoiceAcceptorConfig contains the configuration for an RPC invoice acceptor
// server.
type invoiceAcceptorConfig struct {
	// chainParams is required to properly marshall an invoice for RPC.
	chainParams *chaincfg.Params

	// rpcServer is a bidirectional RPC server to send invoices to a client
	// and receive accept responses from the client.
	rpcServer Invoices_InvoiceAcceptorServer

	// interceptor is the invoice interceptor that will be used to intercept
	// and resolve invoices.
	interceptor invoices.SettlementInterceptorInterface
}

// invoiceAcceptor is a helper struct that handles the lifecycle of an RPC
// invoice acceptor server instance.
//
// This struct handles passing send and receive RPC messages between the client
// and the invoice service.
type invoiceAcceptor struct {
	// cfg contains the configuration for the invoice acceptor.
	cfg invoiceAcceptorConfig
}

// newInvoiceAcceptor creates a new RPC invoice acceptor handler.
func newInvoiceAcceptor(cfg invoiceAcceptorConfig) *invoiceAcceptor {
	return &invoiceAcceptor{
		cfg: cfg,
	}
}

// run sends the intercepted invoices to the client and receives the
// corresponding responses.
func (r *invoiceAcceptor) run() error {
	// Register our invoice interceptor.
	r.cfg.interceptor.SetClientCallback(r.onIntercept)
	defer r.cfg.interceptor.SetClientCallback(nil)

	// Listen for a response from the client in a loop.
	for {
		resp, err := r.cfg.rpcServer.Recv()
		if err != nil {
			return err
		}

		if err := r.resolveFromClient(resp); err != nil {
			return err
		}
	}
}

// onIntercept is called when an invoice is intercepted by the invoice
// interceptor. This method sends the invoice to the client.
func (r *invoiceAcceptor) onIntercept(
	req invoices.InterceptClientRequest) error {

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

	// Unpack the message custom records from the option.
	var msgCustomRecords tlv.Blob
	req.MsgCustomRecords.WhenSome(func(cr tlv.Blob) {
		msgCustomRecords = cr
	})

	return r.cfg.rpcServer.Send(&InvoiceAcceptorRequest{
		Invoice:                  rpcInvoice,
		ExitHtlcCircuitKey:       rpcCircuitKey,
		ExitHtlcAmt:              uint64(req.ExitHtlcAmt),
		ExitHtlcExpiry:           req.ExitHtlcExpiry,
		CurrentHeight:            req.CurrentHeight,
		ExitHtlcMsgCustomRecords: msgCustomRecords,
	})
}

// resolveFromClient handles an invoice resolution received from the client.
func (r *invoiceAcceptor) resolveFromClient(
	in *InvoiceAcceptorResponse) error {

	log.Tracef("Resolving invoice acceptor response %v", in)

	// Parse the invoice preimage from the response.
	if len(in.Preimage) != lntypes.HashSize {
		return status.Errorf(codes.InvalidArgument,
			"Preimage has invalid length: %d", len(in.Preimage))
	}
	preimage, err := lntypes.MakePreimage(in.Preimage)
	if err != nil {
		return status.Errorf(codes.InvalidArgument,
			"Preimage is invalid: %v", err)
	}

	// Derive the payment hash from the preimage.
	paymentHash := preimage.Hash()

	// Pass the resolution to the interceptor.
	return r.cfg.interceptor.Resolve(paymentHash, in.SkipAmountCheck)
}
