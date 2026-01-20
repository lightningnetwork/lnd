package lnd

import (
	"fmt"
	"testing"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

// TestForwardingHistoryMaxEvents tests that the ForwardingHistory RPC
// enforces the maximum number of events that can be requested.
func TestForwardingHistoryMaxEvents(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		maxEvents uint32
		expectErr bool
	}{
		{
			name:      "within limit",
			maxEvents: 50000,
			expectErr: false,
		},
		{
			name:      "at limit",
			maxEvents: channeldb.MaxResponseEvents,
			expectErr: false,
		},
		{
			name:      "exceeds limit",
			maxEvents: 50001,
			expectErr: true,
		},
		{
			name:      "far exceeds limit",
			maxEvents: 999999999,
			expectErr: true,
		},
		{
			name:      "zero defaults to 100",
			maxEvents: 0,
			expectErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a minimal rpcServer for testing.
			cdb := channeldb.OpenForTesting(t, t.TempDir())
			r := &rpcServer{
				server: &server{
					miscDB: cdb.Backend,
					implCfg: &ImplementationCfg{
						AuxComponents: AuxComponents{},
					},
				},
			}

			req := &lnrpc.ForwardingHistoryRequest{
				NumMaxEvents: tt.maxEvents,
			}

			// Note: This will fail when trying to flush forwarding
			// events because htlcSwitch is nil, but that's after
			// the validation we're testing. We just check if we
			// get the expected error for max events.
			_, err := r.ForwardingHistory(nil, req)

			if tt.expectErr {
				require.Error(t, err)
				require.Contains(t, err.Error(),
					"exceeds maximum allowed")
			} else if tt.maxEvents != 0 && tt.maxEvents <= channeldb.MaxResponseEvents {
				// For valid non-zero values, we expect a different
				// error (htlcSwitch nil), not the max events error.
				if err != nil {
					require.NotContains(t, err.Error(),
						"exceeds maximum allowed")
				}
			}
		})
	}
}
