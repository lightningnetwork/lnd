//go:build invoicesrpc
// +build invoicesrpc

package invoicesrpc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/macaroons"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"gopkg.in/macaroon-bakery.v2/bakery"
)

const (
	// subServerName is the name of the sub rpc server. We'll use this name
	// to register ourselves, and we also require that the main
	// SubServerConfigDispatcher instance recognize it as the name of our
	// RPC service.
	subServerName = "InvoicesRPC"
)

var (
	// ErrServerShuttingDown is returned when the server is shutting down.
	ErrServerShuttingDown = errors.New("server shutting down")

	// macaroonOps are the set of capabilities that our minted macaroon (if
	// it doesn't already exist) will have.
	macaroonOps = []bakery.Op{
		{
			Entity: "invoices",
			Action: "write",
		},
		{
			Entity: "invoices",
			Action: "read",
		},
	}

	// macPermissions maps RPC calls to the permissions they require.
	macPermissions = map[string][]bakery.Op{
		"/invoicesrpc.Invoices/SubscribeSingleInvoice": {{
			Entity: "invoices",
			Action: "read",
		}},
		"/invoicesrpc.Invoices/SettleInvoice": {{
			Entity: "invoices",
			Action: "write",
		}},
		"/invoicesrpc.Invoices/CancelInvoice": {{
			Entity: "invoices",
			Action: "write",
		}},
		"/invoicesrpc.Invoices/AddHoldInvoice": {{
			Entity: "invoices",
			Action: "write",
		}},
		"/invoicesrpc.Invoices/LookupInvoiceV2": {{
			Entity: "invoices",
			Action: "write",
		}},
		"/invoicesrpc.Invoices/HtlcModifier": {{
			Entity: "invoices",
			Action: "write",
		}},
	}

	// DefaultInvoicesMacFilename is the default name of the invoices
	// macaroon that we expect to find via a file handle within the main
	// configuration file in this package.
	DefaultInvoicesMacFilename = "invoices.macaroon"
)

// ServerShell is a shell struct holding a reference to the actual sub-server.
// It is used to register the gRPC sub-server with the root server before we
// have the necessary dependencies to populate the actual sub-server.
type ServerShell struct {
	InvoicesServer
}

// Server is a sub-server of the main RPC server: the invoices RPC. This sub
// RPC server allows external callers to access the status of the invoices
// currently active within lnd, as well as configuring it at runtime.
type Server struct {
	// Required by the grpc-gateway/v2 library for forward compatibility.
	UnimplementedInvoicesServer

	quit chan struct{}

	cfg *Config
}

// A compile time check to ensure that Server fully implements the
// InvoicesServer gRPC service.
var _ InvoicesServer = (*Server)(nil)

// New returns a new instance of the invoicesrpc Invoices sub-server. We also
// return the set of permissions for the macaroons that we may create within
// this method. If the macaroons we need aren't found in the filepath, then
// we'll create them on start up. If we're unable to locate, or create the
// macaroons we need, then we'll return with an error.
func New(cfg *Config) (*Server, lnrpc.MacaroonPerms, error) {
	// If the path of the invoices macaroon wasn't specified, then we'll
	// assume that it's found at the default network directory.
	macFilePath := filepath.Join(
		cfg.NetworkDir, DefaultInvoicesMacFilename,
	)

	// Now that we know the full path of the invoices macaroon, we can
	// check to see if we need to create it or not. If stateless_init is set
	// then we don't write the macaroons.
	if cfg.MacService != nil && !cfg.MacService.StatelessInit &&
		!lnrpc.FileExists(macFilePath) {

		log.Infof("Baking macaroons for invoices RPC Server at: %v",
			macFilePath)

		// At this point, we know that the invoices macaroon doesn't
		// yet, exist, so we need to create it with the help of the
		// main macaroon service.
		invoicesMac, err := cfg.MacService.NewMacaroon(
			context.Background(), macaroons.DefaultRootKeyID,
			macaroonOps...,
		)
		if err != nil {
			return nil, nil, err
		}
		invoicesMacBytes, err := invoicesMac.M().MarshalBinary()
		if err != nil {
			return nil, nil, err
		}
		err = os.WriteFile(macFilePath, invoicesMacBytes, 0644)
		if err != nil {
			_ = os.Remove(macFilePath)
			return nil, nil, err
		}
	}

	server := &Server{
		cfg:  cfg,
		quit: make(chan struct{}, 1),
	}

	return server, macPermissions, nil
}

// Start launches any helper goroutines required for the Server to function.
//
// NOTE: This is part of the lnrpc.SubServer interface.
func (s *Server) Start() error {
	return nil
}

// Stop signals any active goroutines for a graceful closure.
//
// NOTE: This is part of the lnrpc.SubServer interface.
func (s *Server) Stop() error {
	close(s.quit)

	return nil
}

// Name returns a unique string representation of the sub-server. This can be
// used to identify the sub-server and also de-duplicate them.
//
// NOTE: This is part of the lnrpc.SubServer interface.
func (s *Server) Name() string {
	return subServerName
}

// RegisterWithRootServer will be called by the root gRPC server to direct a sub
// RPC server to register itself with the main gRPC root server. Until this is
// called, each sub-server won't be able to have requests routed towards it.
//
// NOTE: This is part of the lnrpc.GrpcHandler interface.
func (r *ServerShell) RegisterWithRootServer(grpcServer *grpc.Server) error {
	// We make sure that we register it with the main gRPC server to ensure
	// all our methods are routed properly.
	RegisterInvoicesServer(grpcServer, r)

	log.Debugf("Invoices RPC server successfully registered with root " +
		"gRPC server")

	return nil
}

// RegisterWithRestServer will be called by the root REST mux to direct a sub
// RPC server to register itself with the main REST mux server. Until this is
// called, each sub-server won't be able to have requests routed towards it.
//
// NOTE: This is part of the lnrpc.GrpcHandler interface.
func (r *ServerShell) RegisterWithRestServer(ctx context.Context,
	mux *runtime.ServeMux, dest string, opts []grpc.DialOption) error {

	// We make sure that we register it with the main REST server to ensure
	// all our methods are routed properly.
	err := RegisterInvoicesHandlerFromEndpoint(ctx, mux, dest, opts)
	if err != nil {
		log.Errorf("Could not register Invoices REST server "+
			"with root REST server: %v", err)
		return err
	}

	log.Debugf("Invoices REST server successfully registered with " +
		"root REST server")
	return nil
}

// CreateSubServer populates the subserver's dependencies using the passed
// SubServerConfigDispatcher. This method should fully initialize the
// sub-server instance, making it ready for action. It returns the macaroon
// permissions that the sub-server wishes to pass on to the root server for all
// methods routed towards it.
//
// NOTE: This is part of the lnrpc.GrpcHandler interface.
func (r *ServerShell) CreateSubServer(
	configRegistry lnrpc.SubServerConfigDispatcher) (lnrpc.SubServer,
	lnrpc.MacaroonPerms, error) {

	subServer, macPermissions, err := createNewSubServer(configRegistry)
	if err != nil {
		return nil, nil, err
	}

	r.InvoicesServer = subServer
	return subServer, macPermissions, nil
}

// SubscribeSingleInvoice returns a uni-directional stream (server -> client)
// for notifying the client of state changes for a specified invoice.
func (s *Server) SubscribeSingleInvoice(req *SubscribeSingleInvoiceRequest,
	updateStream Invoices_SubscribeSingleInvoiceServer) error {

	hash, err := lntypes.MakeHash(req.RHash)
	if err != nil {
		return err
	}

	invoiceClient, err := s.cfg.InvoiceRegistry.SubscribeSingleInvoice(
		updateStream.Context(), hash,
	)
	if err != nil {
		return err
	}
	defer invoiceClient.Cancel()

	log.Debugf("Created new single invoice(pay_hash=%v) subscription", hash)

	for {
		select {
		case newInvoice := <-invoiceClient.Updates:
			rpcInvoice, err := CreateRPCInvoice(
				newInvoice, s.cfg.ChainParams,
			)
			if err != nil {
				return err
			}

			// Give the aux data parser a chance to format the
			// custom data in the invoice HTLCs.
			err = s.cfg.ParseAuxData(rpcInvoice)
			if err != nil {
				return fmt.Errorf("error parsing custom data: "+
					"%w", err)
			}

			if err := updateStream.Send(rpcInvoice); err != nil {
				return err
			}

			// If we have reached a terminal state, close the
			// stream with no error.
			if newInvoice.State.IsFinal() {
				return nil
			}

		case <-updateStream.Context().Done():
			return fmt.Errorf("subscription for "+
				"invoice(pay_hash=%v): %w", hash,
				updateStream.Context().Err())

		case <-s.quit:
			return nil
		}
	}
}

// SettleInvoice settles an accepted invoice. If the invoice is already settled,
// this call will succeed.
func (s *Server) SettleInvoice(ctx context.Context,
	in *SettleInvoiceMsg) (*SettleInvoiceResp, error) {

	preimage, err := lntypes.MakePreimage(in.Preimage)
	if err != nil {
		return nil, err
	}

	err = s.cfg.InvoiceRegistry.SettleHodlInvoice(ctx, preimage)
	if err != nil && !errors.Is(err, invoices.ErrInvoiceAlreadySettled) {
		return nil, err
	}

	return &SettleInvoiceResp{}, nil
}

// CancelInvoice cancels a currently open invoice. If the invoice is already
// canceled, this call will succeed. If the invoice is already settled, it will
// fail.
func (s *Server) CancelInvoice(ctx context.Context,
	in *CancelInvoiceMsg) (*CancelInvoiceResp, error) {

	paymentHash, err := lntypes.MakeHash(in.PaymentHash)
	if err != nil {
		return nil, err
	}

	err = s.cfg.InvoiceRegistry.CancelInvoice(ctx, paymentHash)
	if err != nil {
		return nil, err
	}

	log.Infof("Canceled invoice %v", paymentHash)

	return &CancelInvoiceResp{}, nil
}

// AddHoldInvoice attempts to add a new hold invoice to the invoice database.
// Any duplicated invoices are rejected, therefore all invoices *must* have a
// unique payment hash.
func (s *Server) AddHoldInvoice(ctx context.Context,
	invoice *AddHoldInvoiceRequest) (*AddHoldInvoiceResp, error) {

	addInvoiceCfg := &AddInvoiceConfig{
		AddInvoice:            s.cfg.InvoiceRegistry.AddInvoice,
		IsChannelActive:       s.cfg.IsChannelActive,
		ChainParams:           s.cfg.ChainParams,
		NodeSigner:            s.cfg.NodeSigner,
		DefaultCLTVExpiry:     s.cfg.DefaultCLTVExpiry,
		ChanDB:                s.cfg.ChanStateDB,
		Graph:                 s.cfg.Graph,
		GenInvoiceFeatures:    s.cfg.GenInvoiceFeatures,
		GenAmpInvoiceFeatures: s.cfg.GenAmpInvoiceFeatures,
		GetAlias:              s.cfg.GetAlias,
	}

	hash, err := lntypes.MakeHash(invoice.Hash)
	if err != nil {
		return nil, err
	}

	value, err := lnrpc.UnmarshallAmt(invoice.Value, invoice.ValueMsat)
	if err != nil {
		return nil, err
	}

	// Convert the passed routing hints to the required format.
	routeHints, err := CreateZpay32HopHints(invoice.RouteHints)
	if err != nil {
		return nil, err
	}
	addInvoiceData := &AddInvoiceData{
		Memo:            invoice.Memo,
		Hash:            &hash,
		Value:           value,
		DescriptionHash: invoice.DescriptionHash,
		Expiry:          invoice.Expiry,
		FallbackAddr:    invoice.FallbackAddr,
		CltvExpiry:      invoice.CltvExpiry,
		Private:         invoice.Private,
		HodlInvoice:     true,
		Preimage:        nil,
		RouteHints:      routeHints,
	}

	_, dbInvoice, err := AddInvoice(ctx, addInvoiceCfg, addInvoiceData)
	if err != nil {
		return nil, err
	}

	return &AddHoldInvoiceResp{
		AddIndex:       dbInvoice.AddIndex,
		PaymentRequest: string(dbInvoice.PaymentRequest),
		PaymentAddr:    dbInvoice.Terms.PaymentAddr[:],
	}, nil
}

// LookupInvoiceV2 attempts to look up at invoice. An invoice can be referenced
// using either its payment hash, payment address, or set ID.
func (s *Server) LookupInvoiceV2(ctx context.Context,
	req *LookupInvoiceMsg) (*lnrpc.Invoice, error) {

	var invoiceRef invoices.InvoiceRef

	// First, we'll attempt to parse out the invoice ref from the proto
	// oneof.  If none of the three currently supported types was
	// specified, then we'll exit with an error.
	switch {
	case req.GetPaymentHash() != nil:
		payHash, err := lntypes.MakeHash(req.GetPaymentHash())
		if err != nil {
			return nil, status.Error(
				codes.InvalidArgument,
				fmt.Sprintf("unable to parse pay hash: %v", err),
			)
		}

		invoiceRef = invoices.InvoiceRefByHash(payHash)

	case req.GetPaymentAddr() != nil &&
		req.LookupModifier == LookupModifier_HTLC_SET_BLANK:

		var payAddr [32]byte
		copy(payAddr[:], req.GetPaymentAddr())

		invoiceRef = invoices.InvoiceRefByAddrBlankHtlc(payAddr)

	case req.GetPaymentAddr() != nil:
		var payAddr [32]byte
		copy(payAddr[:], req.GetPaymentAddr())

		invoiceRef = invoices.InvoiceRefByAddr(payAddr)

	case req.GetSetId() != nil &&
		req.LookupModifier == LookupModifier_HTLC_SET_ONLY:

		var setID [32]byte
		copy(setID[:], req.GetSetId())

		invoiceRef = invoices.InvoiceRefBySetIDFiltered(setID)

	case req.GetSetId() != nil:
		var setID [32]byte
		copy(setID[:], req.GetSetId())

		invoiceRef = invoices.InvoiceRefBySetID(setID)

	default:
		return nil, status.Error(codes.InvalidArgument,
			"invoice ref must be set")
	}

	// Attempt to locate the invoice, returning a nice "not found" error if
	// we can't find it in the database.
	invoice, err := s.cfg.InvoiceRegistry.LookupInvoiceByRef(
		ctx, invoiceRef,
	)
	switch {
	case errors.Is(err, invoices.ErrInvoiceNotFound):
		return nil, status.Error(codes.NotFound, err.Error())
	case err != nil:
		return nil, err
	}

	rpcInvoice, err := CreateRPCInvoice(&invoice, s.cfg.ChainParams)
	if err != nil {
		return nil, err
	}

	// Give the aux data parser a chance to format the custom data in the
	// invoice HTLCs.
	err = s.cfg.ParseAuxData(rpcInvoice)
	if err != nil {
		return nil, fmt.Errorf("error parsing custom data: %w", err)
	}

	return rpcInvoice, nil
}

// HtlcModifier is a bidirectional streaming RPC that allows a client to
// intercept and modify the HTLCs that attempt to settle the given invoice. The
// server will send HTLCs of invoices to the client and the client can modify
// some aspects of the HTLC in order to pass the invoice acceptance tests.
func (s *Server) HtlcModifier(
	modifierServer Invoices_HtlcModifierServer) error {

	modifier := newHtlcModifier(s.cfg.ChainParams, modifierServer)
	reset, modifierQuit, err := s.cfg.HtlcModifier.RegisterInterceptor(
		modifier.onIntercept,
	)
	if err != nil {
		return fmt.Errorf("cannot register interceptor: %w", err)
	}

	defer reset()

	log.Debugf("Invoice HTLC modifier client connected")

	for {
		select {
		case <-modifierServer.Context().Done():
			return modifierServer.Context().Err()

		case <-modifierQuit:
			return ErrServerShuttingDown

		case <-s.quit:
			return ErrServerShuttingDown
		}
	}
}
