package lnd

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/cert"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/watchonlyrpc"
	"github.com/lightningnetwork/lnd/lnwallet/rpcwallet"
	"github.com/lightningnetwork/lnd/rpcperms"
	"github.com/lightningnetwork/lnd/signal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func TestGetAllPermissions(t *testing.T) {
	perms := GetAllPermissions()

	// Currently there are 16 entity:action pairs in use.
	assert.Equal(t, len(perms), 16)
}

// mockDataParser is a mock implementation of the AuxDataParser interface.
type mockDataParser struct {
}

// InlineParseCustomData replaces any custom data binary blob in the given RPC
// message with its corresponding JSON formatted data. This transforms the
// binary (likely TLV encoded) data to a human-readable JSON representation
// (still as byte slice).
func (m *mockDataParser) InlineParseCustomData(msg proto.Message) error {
	switch m := msg.(type) {
	case *lnrpc.ChannelBalanceResponse:
		m.CustomChannelData = []byte(`{"foo": "bar"}`)

		return nil

	default:
		return fmt.Errorf("mock only supports ChannelBalanceResponse")
	}
}

func TestAuxDataParser(t *testing.T) {
	// We create an empty channeldb, so we can fetch some channels.
	cdb := channeldb.OpenForTesting(t, t.TempDir())

	r := &rpcServer{
		server: &server{
			chanStateDB: cdb.ChannelStateDB(),
			implCfg: &ImplementationCfg{
				AuxComponents: AuxComponents{
					AuxDataParser: fn.Some[AuxDataParser](
						&mockDataParser{},
					),
				},
			},
		},
	}

	// With the aux data parser in place, we should get a formatted JSON
	// in the custom channel data field.
	resp, err := r.ChannelBalance(nil, &lnrpc.ChannelBalanceRequest{})
	require.NoError(t, err)
	require.NotNil(t, resp)
	require.Equal(t, []byte(`{"foo": "bar"}`), resp.CustomChannelData)

	// If we don't supply the aux data parser, we should get the raw binary
	// data. Which in this case is just two VarInt fields (1 byte each) that
	// represent the value of 0 (zero active and zero pending channels).
	r.server.implCfg.AuxComponents.AuxDataParser = fn.None[AuxDataParser]()

	resp, err = r.ChannelBalance(nil, &lnrpc.ChannelBalanceRequest{})
	require.NoError(t, err)
	require.NotNil(t, resp)
	require.Equal(t, []byte{0x00, 0x00}, resp.CustomChannelData)
}

// TestStopDaemonBeforeRPCStartup preserves shutdown before wallet loading, when
// neither recovery access nor the full server dependencies exist.
func TestStopDaemonBeforeRPCStartup(t *testing.T) {
	// Arrange startup before wallet loading, using the normal waiting
	// state.
	interceptor, err := signal.Intercept()
	require.NoError(t, err)

	r := &rpcServer{
		interceptor: interceptor,
		server:      nil,
		interceptorChain: rpcperms.NewInterceptorChain(
			rpcsLog, true, nil,
		),
	}

	// Act before addDeps or any wallet recovery reader has been installed.
	resp, err := r.StopDaemon(t.Context(), &lnrpc.StopRequest{})
	// Assert the early RPC still requests a normal daemon shutdown.
	require.NoError(t, err)
	require.Equal(t, "shutdown initiated, check logs for progress",
		resp.Status)

	select {
	case <-interceptor.ShutdownChannel():

	case <-time.After(time.Second):
		t.Fatal("expected shutdown request to be delivered")
	}
}

// TestRpcCommitmentType tests the rpcCommitmentType returns the corect
// commitment type given a channel type.
func TestRpcCommitmentType(t *testing.T) {
	tests := []struct {
		name     string
		chanType channeldb.ChannelType
		want     lnrpc.CommitmentType
	}{
		{
			name: "tapscript overlay",
			chanType: channeldb.SimpleTaprootFeatureBit |
				channeldb.TapscriptRootBit,
			want: lnrpc.CommitmentType_SIMPLE_TAPROOT_OVERLAY,
		},
		{
			name:     "simple taproot",
			chanType: channeldb.SimpleTaprootFeatureBit,
			want:     lnrpc.CommitmentType_SIMPLE_TAPROOT,
		},
		{
			name:     "lease expiration",
			chanType: channeldb.LeaseExpirationBit,
			want:     lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE,
		},
		{
			name:     "anchors",
			chanType: channeldb.AnchorOutputsBit,
			want:     lnrpc.CommitmentType_ANCHORS,
		},
		{
			name:     "tweakless",
			chanType: channeldb.SingleFunderTweaklessBit,
			want:     lnrpc.CommitmentType_STATIC_REMOTE_KEY,
		},
		{
			name:     "legacy",
			chanType: channeldb.SingleFunderBit,
			want:     lnrpc.CommitmentType_LEGACY,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(
				t, tt.want, rpcCommitmentType(tt.chanType),
			)
		})
	}
}

// recoveryReaderMock observes the wallet query without introducing another
// production interface or storing a duplicate copy of recovery progress.
type recoveryReaderMock struct {
	mock.Mock
}

// GetRecoveryInfo returns the test's current recovery result for one RPC.
func (m *recoveryReaderMock) GetRecoveryInfo() (bool, float64, error) {
	args := m.Called()
	progress, _ := args.Get(1).(float64)

	return args.Bool(0), progress, args.Error(2)
}

// TestStopDaemonRecoveryGuard enforces live recovery state before signer
// readiness, including unavailable or failing recovery queries.
func TestStopDaemonRecoveryGuard(t *testing.T) {
	cases := []struct {
		name       string
		known      bool
		recovering bool
		progress   float64
		backendErr error
		wantCode   codes.Code
	}{
		{
			name:     "unknown unlocked recovery",
			wantCode: codes.FailedPrecondition,
		},
		{
			name:       "rescan before signer readiness",
			known:      true,
			recovering: true,
			progress:   0.5,
			wantCode:   codes.FailedPrecondition,
		},
		{
			name:       "completed recovery",
			known:      true,
			recovering: true,
			progress:   1,
		},
		{
			name:       "recovery query failure",
			known:      true,
			backendErr: errors.New("wallet query failed"),
			wantCode:   codes.Unknown,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange a real signal handler and interceptor state.
			// Wait for the previous handler's deferred global reset
			// before reusing it.
			var interceptor signal.Interceptor
			require.Eventually(t, func() bool {
				var err error
				interceptor, err = signal.Intercept()

				return err == nil
			}, time.Second, time.Millisecond)
			t.Cleanup(func() {
				interceptor.RequestShutdown()
				<-interceptor.ShutdownChannel()
			})
			chain := rpcperms.NewInterceptorChain(
				rpcsLog, true, nil,
			)
			require.NoError(t, chain.Start())
			t.Cleanup(func() {
				require.NoError(t, chain.Stop())
			})
			chain.SetWalletUnlocked()
			r := &rpcServer{
				interceptor:      interceptor,
				interceptorChain: chain,
			}
			reader := &recoveryReaderMock{}
			if tc.known {
				reader.On("GetRecoveryInfo").Return(
					tc.recovering, tc.progress,
					tc.backendErr,
				).Once()
				r.setRecoveryReader(reader.GetRecoveryInfo)
			}

			// Act through the RPC before addDeps has supplied
			// r.server; the published recovery callback must
			// suffice during startup.
			response, err := r.StopDaemon(
				t.Context(), &lnrpc.StopRequest{},
			)

			// Assert the live query is used exactly once when
			// available, refusals carry a stable RPC code, and only
			// permitted shutdown reaches the real signal handler.
			reader.AssertExpectations(t)
			require.Equal(t, tc.wantCode, status.Code(err))
			if tc.wantCode != codes.OK {
				require.Nil(t, response)
				if tc.backendErr != nil {
					require.ErrorIs(t, err, tc.backendErr)
				}
				select {
				case <-interceptor.ShutdownChannel():
					t.Fatal("rejected shutdown reached " +
						"signal handler")
				case <-time.After(10 * time.Millisecond):
				}

				return
			}
			require.NoError(t, err)
			require.NotNil(t, response)
			select {
			case <-interceptor.ShutdownChannel():
			case <-time.After(time.Second):
				t.Fatal("permitted shutdown did not " +
					"reach signal handler")
			}
		})
	}
}

// TestRemoteSignerMandatoryMiddleware proves unregistered main-chain mandatory
// middleware cannot block authenticated registration on the dedicated server.
func TestRemoteSignerMandatoryMiddleware(t *testing.T) {
	// Arrange the production dedicated-server startup path with mandatory
	// middleware configured but never registered on the main chain.
	// Transport TLS remains real; macaroon authorization has its separate
	// daemon itest.
	cfg := DefaultConfig()
	cfg.NoMacaroons = true
	cfg.RPCMiddleware.Enable = true
	cfg.RPCMiddleware.Mandatory = []string{"unregistered"}
	cfg.RemoteSigner.Enable = true
	cfg.RemoteSigner.ExperimentalAllowInboundConnection = true
	cfg.RemoteSigner.ExperimentalRPCListeners = []string{"127.0.0.1:0"}
	mainChain := rpcperms.NewInterceptorChain(
		rpcsLog, true, cfg.RPCMiddleware.Mandatory,
	)
	require.NoError(t, mainChain.Start())
	t.Cleanup(func() {
		require.NoError(t, mainChain.Stop())
	})
	mainChain.SetWalletUnlocked()
	coordinator := rpcwallet.NewInboundConnection(time.Second, time.Second)
	t.Cleanup(coordinator.Stop)
	certBytes, keyBytes, err := cert.GenCertPair(
		"mandatory middleware test", nil, nil, false, time.Hour,
	)
	require.NoError(t, err)
	pair, err := tls.X509KeyPair(certBytes, keyBytes)
	require.NoError(t, err)
	tlsCredentials := credentials.NewTLS(cert.TLSConfFromCert(pair))
	server, listeners, chain, err := startInboundWatchOnlyRPCServer(
		&cfg, []grpc.ServerOption{
			grpc.Creds(tlsCredentials),
		}, keepalive.ServerParameters{}, keepalive.EnforcementPolicy{},
		mainChain, coordinator,
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		server.Stop()
		require.NoError(t, chain.Stop())
	})
	roots := x509.NewCertPool()
	require.True(t, roots.AppendCertsFromPEM(certBytes))
	clientCredentials := credentials.NewClientTLSFromCert(roots, "")
	connection, err := grpc.NewClient(
		listeners[0].Addr().String(),
		grpc.WithTransportCredentials(clientCredentials),
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, connection.Close())
	})
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	client := watchonlyrpc.NewWatchOnlyClient(connection)
	stream, err := client.SignCoordinatorStreams(ctx)
	require.NoError(t, err)
	challenge := make([]byte, 32)
	version := watchonlyrpc.RegistrationVersion

	// Act by registering over the actual TLS stream while the main server
	// still has no middleware registration or active signing dependencies.
	require.NoError(t, stream.Send(&watchonlyrpc.SignCoordinatorResponse{
		RefRequestId: 1,
		SignResponseType: &rpcwallet.RSRegistration{
			SignerRegistration: &watchonlyrpc.SignerRegistration{
				RegistrationChallenge: challenge,
				RegistrationInfo:      version,
			},
		},
	}))
	response, err := stream.Recv()

	// Assert registration completes with a connection-bound proof instead
	// of waiting forever for middleware that can only register later.
	require.NoError(t, err)
	complete := response.GetRegistrationResponse().GetRegistrationComplete()
	require.NotNil(t, complete)
	require.NoError(t, watchonlyrpc.VerifyRegistrationProof(
		stream.Context(), challenge, complete.GetRegistrationInfo(),
		complete.GetSignature(),
	))

	// The probe follows proof verification; replying proves the dedicated
	// stream can admit work without waiting for main-server middleware.
	ping, err := stream.Recv()
	require.NoError(t, err)
	require.True(t, ping.GetPing())
	require.NoError(t, stream.Send(&rpcwallet.RSResponse{
		RefRequestId: ping.GetRequestId(),
		SignResponseType: &watchonlyrpc.SignCoordinatorResponse_Pong{
			Pong: true,
		},
	}))
	require.NoError(t, coordinator.WaitUntilConnected(ctx))
}
