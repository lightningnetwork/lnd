package lnd

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/chainreg"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

// TestConnectPeerNoHost tests that ConnectPeer returns a descriptive error
// when no host is provided.
func TestConnectPeerNoHost(t *testing.T) {
	t.Parallel()

	// Create mock ECDH key for identity checking.
	mockPrivKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	// Create minimal mock for tor.Net interface.
	mockNet := &mockTorNet{}

	// Create rpcServer instance with proper mocks.
	cfg := &Config{
		net:             mockNet,
		ActiveNetParams: chainreg.BitcoinTestNetParams,
	}

	server := &server{
		active:       1,
		identityECDH: &keychain.PrivKeyECDH{PrivKey: mockPrivKey},
	}

	rpcServer := &rpcServer{
		cfg:    cfg,
		server: server,
	}

	// Test with empty host should trigger our error.
	req := &lnrpc.ConnectPeerRequest{
		Addr: &lnrpc.LightningAddress{
			Pubkey: "0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798",
			Host:   "",
		},
	}

	_, err = rpcServer.ConnectPeer(context.Background(), req)

	// Verify error contains expected message.
	require.Error(t, err)
	require.Contains(t, err.Error(), "no addresses known for peer")
	require.Contains(t, err.Error(), req.Addr.Pubkey)

	// Verify correct gRPC error code.
	grpcErr := status.Convert(err)
	require.Equal(t, codes.InvalidArgument, grpcErr.Code())
}

// mockTorNet implements the tor.Net interface for testing.
type mockTorNet struct{}

func (m *mockTorNet) Dial(network, address string, timeout time.Duration) (net.Conn, error) {
	return nil, fmt.Errorf("mock dial")
}

func (m *mockTorNet) LookupHost(host string) ([]string, error) {
	return nil, fmt.Errorf("mock lookup")
}

func (m *mockTorNet) LookupSRV(service, proto, name string, timeout time.Duration) (string, []*net.SRV, error) {
	return "", nil, fmt.Errorf("mock srv")
}

func (m *mockTorNet) ResolveTCPAddr(network, address string) (*net.TCPAddr, error) {
	return nil, fmt.Errorf("mock resolve")
}
