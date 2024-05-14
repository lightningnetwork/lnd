package rpcwallet

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/macaroons"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"gopkg.in/macaroon.v2"
)

type (
	StreamClient = walletrpc.WalletKit_SignCoordinatorStreamsClient
	StreamServer = walletrpc.WalletKit_SignCoordinatorStreamsServer
)

// RemoteSigner is an interface that abstracts the communication with a remote
// signer. It extends the RemoteSignerRequests, and adds some additional methods
// to manage the connection and verify the health of the remote signer.
type RemoteSigner interface {
	// RemoteSignerRequests is an interface that defines the requests that
	// can be sent to a remote signer.
	RemoteSignerRequests

	// Timeout returns the set connection timeout for the remote signer.
	Timeout() time.Duration

	// Ready blocks and returns nil when the remote signer is ready to
	// accept requests.
	Ready() error

	// Ping verifies that the remote signer is still responsive.
	Ping(timeout time.Duration) error

	// Run feeds lnd with the incoming stream set up by an outbound remote
	// signer and then blocks until the stream is closed. Lnd can then send
	// any requests to the remote signer through the stream.
	Run(stream StreamServer) error
}

// RemoteSignerRequests is an interface that defines the requests that can be
// sent to a remote signer. It's a subset of the signrpc.SignerClient and
// walletrpc.WalletKitClient interfaces.
type RemoteSignerRequests interface {
	// DeriveSharedKey sends a SharedKeyRequest to the remote signer and
	// waits for the corresponding response.
	DeriveSharedKey(ctx context.Context,
		in *signrpc.SharedKeyRequest,
		opts ...grpc.CallOption) (*signrpc.SharedKeyResponse, error)

	// DeriveSharedKey sends a MuSig2CleanupRequest to the remote signer and
	// waits for the corresponding response.
	MuSig2Cleanup(ctx context.Context,
		in *signrpc.MuSig2CleanupRequest,
		opts ...grpc.CallOption) (*signrpc.MuSig2CleanupResponse, error)

	// MuSig2CombineSig sends a MuSig2CombineSigRequest to the remote signer
	// and waits for the corresponding response.
	MuSig2CombineSig(ctx context.Context,
		in *signrpc.MuSig2CombineSigRequest,
		opts ...grpc.CallOption) (*signrpc.MuSig2CombineSigResponse,
		error)

	// MuSig2CreateSession sends a MuSig2SessionRequest to the remote signer
	// and waits for the corresponding response.
	MuSig2CreateSession(ctx context.Context,
		in *signrpc.MuSig2SessionRequest,
		opts ...grpc.CallOption) (*signrpc.MuSig2SessionResponse, error)

	// MuSig2RegisterNonces sends a MuSig2RegisterNoncesRequest to the
	// remote signer and waits for the corresponding response.
	MuSig2RegisterNonces(ctx context.Context,
		in *signrpc.MuSig2RegisterNoncesRequest,
		opts ...grpc.CallOption) (*signrpc.MuSig2RegisterNoncesResponse,
		error)

	// MuSig2Sign sends a MuSig2SignRequest to the remote signer and waits
	// for the corresponding response.
	MuSig2Sign(ctx context.Context,
		in *signrpc.MuSig2SignRequest,
		opts ...grpc.CallOption) (*signrpc.MuSig2SignResponse, error)

	// SignMessage sends a SignMessageReq to the remote signer and waits for
	// the corresponding response.
	SignMessage(ctx context.Context,
		in *signrpc.SignMessageReq,
		opts ...grpc.CallOption) (*signrpc.SignMessageResp, error)

	// SignPsbt sends a SignPsbtRequest to the remote signer and waits for
	// the corresponding response.
	SignPsbt(ctx context.Context, in *walletrpc.SignPsbtRequest,
		opts ...grpc.CallOption) (*walletrpc.SignPsbtResponse, error)
}

// InboundRemoteSigner is an abstraction of the connection to an inbound remote
// signer. An inbound remote signer is a remote signer that allows the
// watch-only node to connect to it via an inbound GRPC connection.
type InboundRemoteSigner struct {
	// Embedded signrpc.SignerClient and walletrpc.WalletKitClient to
	// implement the RemoteSigner interface.
	signrpc.SignerClient

	walletrpc.WalletKitClient

	// The host:port of the remote signer node.
	rpcHost string

	// The path to the TLS certificate of the remote signer node.
	tlsCertPath string

	// The path to the macaroon of the remote signer node.
	macaroonPath string

	// The timeout for the connection to the remote signer node.
	timeout time.Duration
}

// NewInboundRemoteSigner creates a new InboundRemoteSigner instance.
// The function sets up a connection to the remote signer node.
// The returned function is a cleanup function that should be called to close
// the connection when the remote signer is no longer needed.
func NewInboundRemoteSigner(rpcHost string, tlsCertPath string,
	macaroonPath string,
	timeout time.Duration) (*InboundRemoteSigner, func(), error) {

	rpcConn, err := connect(rpcHost, tlsCertPath, macaroonPath, timeout)
	if err != nil {
		return nil, nil, fmt.Errorf("error connecting to the remote "+
			"signing node through RPC: %v", err)
	}

	cleanUp := func() {
		rpcConn.Close()
	}

	remoteSigner := &InboundRemoteSigner{
		SignerClient:    signrpc.NewSignerClient(rpcConn),
		WalletKitClient: walletrpc.NewWalletKitClient(rpcConn),
		rpcHost:         rpcHost,
		tlsCertPath:     tlsCertPath,
		macaroonPath:    macaroonPath,
		timeout:         timeout,
	}

	return remoteSigner, cleanUp, nil
}

// Run feeds lnd with the incoming stream set up by an outbound remote signer
// and then blocks until the stream is closed. Lnd can then send any requests to
// the remote signer through the stream.
//
// NOTE: This is part of the RemoteSigner interface.
func (*InboundRemoteSigner) Run(_ StreamServer) error {
	// If lnd has been configured to use an inbound remote signer, it should
	// not allow an outbound remote signer to connect.
	return errors.New("not supported when remotesigner.signertype is set " +
		"to \"inbound\"")
}

// Ready blocks and returns nil when the remote signer is ready to accept
// requests.
//
// NOTE: This is part of the RemoteSigner interface.
func (r *InboundRemoteSigner) Ready() error {
	// The inbound remote signer is ready as soon we have connected to the
	// remote signer node in the constructor. Therefore, we always return
	// nil here to signal that we are ready.
	return nil
}

// Ping verifies that the remote signer is still responsive.
//
// NOTE: This is part of the RemoteSigner interface.
func (r *InboundRemoteSigner) Ping(timeout time.Duration) error {
	conn, err := connect(
		r.rpcHost, r.tlsCertPath, r.macaroonPath, timeout,
	)
	if err != nil {
		return fmt.Errorf("error connecting to the remote "+
			"signing node through RPC: %v", err)
	}

	return conn.Close()
}

// Timeout returns the set connection timeout for the remote signer.
//
// NOTE: This is part of the RemoteSigner interface.
func (r *InboundRemoteSigner) Timeout() time.Duration {
	return r.timeout
}

// A compile time assertion to ensure InboundRemoteSigner meets the
// RemoteSigner interface.
var _ RemoteSigner = (*InboundRemoteSigner)(nil)

// connect tries to establish an RPC connection to the given host:port with the
// supplied certificate and macaroon.
func connect(hostPort, tlsCertPath, macaroonPath string,
	timeout time.Duration) (*grpc.ClientConn, error) {

	certBytes, err := os.ReadFile(tlsCertPath)
	if err != nil {
		return nil, fmt.Errorf("error reading TLS cert file %v: %w",
			tlsCertPath, err)
	}

	cp := x509.NewCertPool()
	if !cp.AppendCertsFromPEM(certBytes) {
		return nil, fmt.Errorf("credentials: failed to append " +
			"certificate")
	}

	macBytes, err := os.ReadFile(macaroonPath)
	if err != nil {
		return nil, fmt.Errorf("error reading macaroon file %v: %w",
			macaroonPath, err)
	}
	mac := &macaroon.Macaroon{}
	if err := mac.UnmarshalBinary(macBytes); err != nil {
		return nil, fmt.Errorf("error decoding macaroon: %w", err)
	}

	macCred, err := macaroons.NewMacaroonCredential(mac)
	if err != nil {
		return nil, fmt.Errorf("error creating creds: %w", err)
	}

	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(credentials.NewClientTLSFromCert(
			cp, "",
		)),
		grpc.WithPerRPCCredentials(macCred),
		grpc.WithBlock(),
	}

	ctxt, cancel := context.WithTimeout(context.Background(), timeout)
	// In the blocking case, ctx can be used to cancel or expire the pending
	// connection. Once this function returns, the cancellation and
	// expiration of ctx will be noop. Users should call ClientConn.Close to
	// terminate all the pending operations after this function returns.
	defer cancel()

	conn, err := grpc.DialContext(ctxt, hostPort, opts...)
	if err != nil {
		return nil, fmt.Errorf("unable to connect to RPC server: %w",
			err)
	}

	return conn, nil
}
