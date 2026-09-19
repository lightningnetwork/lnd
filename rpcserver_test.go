package lnd

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/signal"
	"github.com/lightningnetwork/lnd/subscribe"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
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

// TestSlowSubscriptionClientError verifies that an evicted subscription is
// translated into an explicit gRPC slow-consumer error.
func TestSlowSubscriptionClientError(t *testing.T) {
	t.Parallel()

	server := subscribe.NewServerWithQueueSize(1)
	require.NoError(t, server.Start())
	t.Cleanup(func() {
		require.NoError(t, server.Stop())
	})

	client, err := server.Subscribe()
	require.NoError(t, err)

	// The first update fills the queue and the second evicts the client.
	require.NoError(t, server.SendUpdate(1))
	require.NoError(t, server.SendUpdate(2))

	select {
	case <-client.Quit():
	case <-time.After(time.Second):
		t.Fatal("slow client was not evicted")
	}

	err = subscriptionClientError(client)
	require.Equal(t, codes.ResourceExhausted, status.Code(err))
	require.ErrorContains(t, err, subscribe.ErrSlowConsumer.Error())
}

// blockingCustomMessageStream models a custom-message RPC whose transport is
// unable to accept the first response.
type blockingCustomMessageStream struct {
	grpc.ServerStream

	ctx         context.Context
	sendStarted chan struct{}
	sendRelease chan struct{}
}

// Context returns the lifetime of the test stream.
func (s *blockingCustomMessageStream) Context() context.Context {
	return s.ctx
}

// Send blocks until the test releases the simulated transport.
func (s *blockingCustomMessageStream) Send(*lnrpc.CustomMessage) error {
	close(s.sendStarted)

	select {
	case <-s.sendRelease:
		return nil
	case <-s.ctx.Done():
		return s.ctx.Err()
	}
}

// TestSubscribeCustomMessagesSlowClient verifies that the custom-message RPC
// returns ResourceExhausted after its bounded subscription evicts it.
func TestSubscribeCustomMessagesSlowClient(t *testing.T) {
	t.Parallel()

	const queueSize = 10

	messageServer := subscribe.NewServerWithQueueSize(queueSize)
	require.NoError(t, messageServer.Start())
	t.Cleanup(func() {
		require.NoError(t, messageServer.Stop())
	})

	rpc := &rpcServer{
		server: &server{
			customMessageServer: messageServer,
		},
	}

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	stream := &blockingCustomMessageStream{
		ctx:         ctx,
		sendStarted: make(chan struct{}),
		sendRelease: make(chan struct{}),
	}
	rpcResult := make(chan error, 1)
	go func() {
		rpcResult <- rpc.SubscribeCustomMessages(
			&lnrpc.SubscribeCustomMessagesRequest{}, stream,
		)
	}()

	update := &CustomMessage{
		Msg: &lnwire.Custom{
			Type: 32_769,
			Data: []byte{1},
		},
	}

	// Dispatch until the RPC subscription is active and its first Send is
	// blocked in the simulated transport.
	deadline := time.After(5 * time.Second)
sendFirstUpdate:
	for {
		require.NoError(t, messageServer.SendUpdate(update))

		select {
		case <-stream.sendStarted:
			break sendFirstUpdate
		case <-deadline:
			t.Fatal("custom-message RPC did not start sending")
		case <-time.After(10 * time.Millisecond):
		}
	}

	// One update fills the queue, the next evicts the client, and the last
	// acts as a barrier proving that eviction has completed.
	for i := 0; i < queueSize+2; i++ {
		require.NoError(t, messageServer.SendUpdate(update))
	}

	close(stream.sendRelease)

	select {
	case err := <-rpcResult:
		require.Equal(t, codes.ResourceExhausted, status.Code(err))
		require.ErrorContains(
			t, err, subscribe.ErrSlowConsumer.Error(),
		)
	case <-time.After(time.Second):
		t.Fatal("custom-message RPC did not return after eviction")
	}
}

// TestStopDaemonBeforeRPCStartup makes sure StopDaemon can be called during
// the wallet-unlocked startup window, before addDeps has populated the
// rpcServer's server dependencies and while r.server is still nil. That
// startup state can last for an extended period when lnd is waiting for an
// outbound remote signer to connect before the full RPC server becomes active
// when lnd is configured to use an outbound remote signer.
func TestStopDaemonBeforeRPCStartup(t *testing.T) {
	interceptor, err := signal.Intercept()
	require.NoError(t, err)

	r := &rpcServer{
		interceptor: interceptor,
		server:      nil,
	}

	resp, err := r.StopDaemon(t.Context(), &lnrpc.StopRequest{})
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
