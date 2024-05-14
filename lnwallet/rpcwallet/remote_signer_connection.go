package rpcwallet

import (
	"context"
	"time"

	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"google.golang.org/grpc"
)

type (
	StreamClient = walletrpc.WalletKit_SignCoordinatorStreamsClient
	StreamServer = walletrpc.WalletKit_SignCoordinatorStreamsServer
)

// RemoteSignerConnection is an interface that abstracts the communication with
// a remote signer. It extends the RemoteSignerRequests interface, and adds some
// additional methods to manage the connection and verify the health of the
// remote signer.
type RemoteSignerConnection interface {
	// RemoteSignerRequests is an interface that defines the requests that
	// can be sent to a remote signer.
	RemoteSignerRequests

	// Timeout returns the set connection timeout for the remote signer.
	Timeout() time.Duration

	// Ready returns a channel that nil gets sent over once the remote
	// signer is ready to accept requests.
	Ready(ctx context.Context) chan error

	// Stop gracefully disconnects from the remote signer.
	Stop()

	// Ping verifies that the remote signer is still responsive.
	Ping(ctx context.Context, timeout time.Duration) error
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

	// MuSig2Cleanup sends a MuSig2CleanupRequest to the remote signer and
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

	// MuSig2RegisterCombinedNonce sends a MuSig2RegisterCombinedNonce to
	// the remote signer and waits for the corresponding response.
	MuSig2RegisterCombinedNonce(ctx context.Context,
		in *signrpc.MuSig2RegisterCombinedNonceRequest,
		opts ...grpc.CallOption) (
		*signrpc.MuSig2RegisterCombinedNonceResponse, error)

	// MuSig2GetCombinedNonce sends a MuSig2GetCombinedNonceRequest to the
	// remote signer and waits for the corresponding response.
	MuSig2GetCombinedNonce(ctx context.Context,
		in *signrpc.MuSig2GetCombinedNonceRequest,
		opts ...grpc.CallOption) (
		*signrpc.MuSig2GetCombinedNonceResponse, error)
}
