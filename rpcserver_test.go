package lnd

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/lightningnetwork/lnd/signal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// canceledInvoiceDBMock supplies only the database operations used by the
// canceled-invoice RPC, leaving the concrete registry's lookup path intact.
type canceledInvoiceDBMock struct {
	invoices.InvoiceDB
	mock.Mock
}

// Assert the embedded interface and overridden operations satisfy the registry
// and RPC database contract without implementing unrelated storage methods.
var _ invoices.InvoiceDB = (*canceledInvoiceDBMock)(nil)

// LookupInvoice returns the arranged snapshot through the registry's normal
// lookup path so deletion policy is tested at the RPC boundary.
func (m *canceledInvoiceDBMock) LookupInvoice(ctx context.Context,
	ref invoices.InvoiceRef) (invoices.Invoice, error) {

	args := m.Called(ctx, ref)
	invoice, _ := args.Get(0).(invoices.Invoice)
	return invoice, args.Error(1)
}

// DeleteInvoice requires an explicit expectation so any deletion of a released
// payment record fails the test rather than silently discarding its history.
func (m *canceledInvoiceDBMock) DeleteInvoice(ctx context.Context,
	refs []invoices.InvoiceDeleteRef) error {

	args := m.Called(ctx, refs)
	return args.Error(0)
}

// TestDeleteCanceledInvoiceReleaseHistory checks that canceled-invoice deletion
// preserves ordinary release history even when all released HTLCs failed.
func TestDeleteCanceledInvoiceReleaseHistory(t *testing.T) {
	t.Parallel()

	// Arrange canceled snapshots with a known preimage in every case.
	// Only the HTLC evidence distinguishes an unreleased cancellation
	// from released collection failures, including zero received credit.
	preimage := lntypes.Preimage{1}
	hash := preimage.Hash()
	testCases := []struct {
		name        string
		htlcStates  []invoices.HtlcState
		amtPaid     lnwire.MilliSatoshi
		allowDelete bool
	}{
		{
			name: "unreleased cancellation",
			htlcStates: []invoices.HtlcState{
				invoices.HtlcStateCanceled,
			},
			allowDelete: true,
		},
		{
			name: "all released HTLCs failed",
			htlcStates: []invoices.HtlcState{
				invoices.HtlcStateFailed,
			},
		},
		{
			name: "partially collected payment",
			htlcStates: []invoices.HtlcState{
				invoices.HtlcStateSettled,
				invoices.HtlcStateFailed,
			},
			amtPaid: 500,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Arrange the registry's lookup; require one delete
			// only for the unreleased fixture. The
			// unstarted registry performs lookup without workers.
			ctx := t.Context()
			htlcs := make(
				map[models.CircuitKey]*invoices.InvoiceHTLC,
			)
			invoice := invoices.Invoice{
				State:    invoices.ContractCanceled,
				AddIndex: 1,
				AmtPaid:  tc.amtPaid,
				Terms: invoices.ContractTerm{
					PaymentPreimage: &preimage,
				},
				Htlcs: htlcs,
			}
			for n, state := range tc.htlcStates {
				invoice.Htlcs[models.CircuitKey{
					HtlcID: uint64(n),
				}] = &invoices.InvoiceHTLC{
					State: state,
				}
			}
			idb := &canceledInvoiceDBMock{}
			idb.Test(t)
			t.Cleanup(func() {
				idb.AssertExpectations(t)
			})
			idb.On(
				"LookupInvoice", ctx,
				invoices.InvoiceRefByHash(hash),
			).Return(invoice, nil).Once()
			if tc.allowDelete {
				refs := []invoices.InvoiceDeleteRef{
					{
						PayHash:  hash,
						AddIndex: invoice.AddIndex,
					},
				}
				idb.On(
					"DeleteInvoice", ctx, refs,
				).Return(nil).Once()
			}
			r := &rpcServer{
				server: &server{
					invoicesDB: idb,
					invoices: invoices.NewRegistry(
						idb, nil,
						&invoices.RegistryConfig{},
					),
				},
			}

			// Act through the public canceled-only deletion method;
			// released records must not reach database deletion.
			resp, err := r.DeleteCanceledInvoice(
				ctx, &lnrpc.DelCanceledInvoiceReq{
					InvoiceHash: hash.String(),
				},
			)

			// Assert unreleased cancellations remain deletable;
			// both released failures return the documented status
			// without a response; cleanup verifies database calls.
			if tc.allowDelete {
				require.NoError(t, err)
				require.NotNil(t, resp)
			} else {
				require.Equal(
					t, codes.FailedPrecondition,
					status.Code(err),
				)
				require.Nil(t, resp)
			}
		})
	}
}

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

// TestMarshalDBEdgeVersions checks that the graph RPC marshalers read the
// direction, the disabled state and the last update of a policy from the
// fields that its gossip version carries them in.
func TestMarshalDBEdgeVersions(t *testing.T) {
	t.Parallel()

	lastUpdate := time.Unix(1_700_000_000, 0)

	tests := []struct {
		name string

		// node1 and node2 are the policies of NodeKey1 and NodeKey2.
		// The test hands them to marshalDBEdge in both orders, so the
		// marshaler has to place them by their direction.
		node1, node2 *models.ChannelEdgePolicy

		wantDisabled   bool
		wantLastUpdate uint32
	}{
		{
			name: "v1",
			node1: &models.ChannelEdgePolicy{
				Version:     lnwire.GossipVersion1,
				FeeBaseMSat: 1,
				LastUpdate:  lastUpdate,
				ChannelFlags: lnwire.
					ChanUpdateDisabled,
			},
			node2: &models.ChannelEdgePolicy{
				Version:      lnwire.GossipVersion1,
				FeeBaseMSat:  2,
				LastUpdate:   lastUpdate,
				ChannelFlags: lnwire.ChanUpdateDirection,
			},
			wantDisabled:   true,
			wantLastUpdate: uint32(lastUpdate.Unix()),
		},
		{
			// A v2 policy carries its direction in SecondPeer and
			// its disabled state in DisableFlags, and leaves the
			// v1 ChannelFlags and LastUpdate unset.
			name: "v2",
			node1: &models.ChannelEdgePolicy{
				Version:     lnwire.GossipVersion2,
				FeeBaseMSat: 1,
				DisableFlags: lnwire.ChanUpdateDisableIncoming |
					lnwire.ChanUpdateDisableOutgoing,
				LastBlockHeight: 800_000,
			},
			node2: &models.ChannelEdgePolicy{
				Version:         lnwire.GossipVersion2,
				FeeBaseMSat:     2,
				SecondPeer:      true,
				LastBlockHeight: 800_001,
			},
			wantDisabled: true,

			// A v2 policy has no unix time, so the last update is
			// zero rather than the year-2042 value that converting
			// a zero time.Time to uint32 produces.
			wantLastUpdate: 0,
		},
	}

	for _, test := range tests {
		for _, reversed := range []bool{false, true} {
			name := fmt.Sprintf("%s/reversed=%v", test.name,
				reversed)

			t.Run(name, func(t *testing.T) {
				t.Parallel()

				c1, c2 := test.node1, test.node2
				if reversed {
					c1, c2 = c2, c1
				}

				edge := marshalDBEdge(
					&models.ChannelEdgeInfo{ChannelID: 1},
					c1, c2, false,
				)

				require.EqualValues(
					t, 1, edge.Node1Policy.FeeBaseMsat,
				)
				require.EqualValues(
					t, 2, edge.Node2Policy.FeeBaseMsat,
				)

				require.Equal(
					t, test.wantDisabled,
					edge.Node1Policy.Disabled,
				)
				require.False(t, edge.Node2Policy.Disabled)

				require.Equal(
					t, test.wantLastUpdate,
					edge.Node1Policy.LastUpdate,
				)
				require.Equal(
					t, test.wantLastUpdate, edge.LastUpdate,
				)
			})
		}
	}
}

// TestChannelPolicyFromRequest checks which fields of a PolicyUpdateRequest are
// applied, in particular that legacy requests keep applying zero values while
// partial updates leave unspecified fields unchanged.
func TestChannelPolicyFromRequest(t *testing.T) {
	t.Parallel()

	const (
		maxCltvExpiry = 2016
		zeroFee       = lnwire.MilliSatoshi(0)
	)

	tests := []struct {
		name   string
		req    *lnrpc.PolicyUpdateRequest
		want   routing.ChannelPolicy
		errStr string
	}{
		{
			// Existing clients don't send zero values on the wire,
			// so they must still be applied as zero.
			name: "legacy request applies zero fees",
			req: &lnrpc.PolicyUpdateRequest{
				TimeLockDelta: 40,
			},
			want: routing.ChannelPolicy{
				FeeSchema: routing.FeeSchema{
					BaseFee: fn.Some(zeroFee),
					FeeRate: fn.Some(uint32(0)),
				},
				TimeLockDelta: fn.Some(uint32(40)),
			},
		},
		{
			name: "legacy request rejects zero time lock delta",
			req: &lnrpc.PolicyUpdateRequest{
				BaseFeeMsat: 1000,
			},
			errStr: "time lock delta of 0 is too small",
		},
		{
			name: "partial update keeps unspecified fields",
			req: &lnrpc.PolicyUpdateRequest{
				PartialUpdate: &lnrpc.PartialPolicyUpdate{},
				MaxHtlcMsat:   5000,
			},
			want: routing.ChannelPolicy{
				MaxHTLC: 5000,
			},
		},
		{
			name: "partial update applies specified zero fees",
			req: &lnrpc.PolicyUpdateRequest{
				PartialUpdate: &lnrpc.PartialPolicyUpdate{
					BaseFeeMsat: true,
					FeeRate:     true,
				},
			},
			want: routing.ChannelPolicy{
				FeeSchema: routing.FeeSchema{
					BaseFee: fn.Some(zeroFee),
					FeeRate: fn.Some(uint32(0)),
				},
			},
		},
		{
			name: "partial update applies fee rate ppm only",
			req: &lnrpc.PolicyUpdateRequest{
				PartialUpdate: &lnrpc.PartialPolicyUpdate{
					FeeRate: true,
				},
				FeeRatePpm: 250,
			},
			want: routing.ChannelPolicy{
				FeeSchema: routing.FeeSchema{
					FeeRate: fn.Some(uint32(250)),
				},
			},
		},
		{
			name: "partial update applies time lock delta only",
			req: &lnrpc.PolicyUpdateRequest{
				PartialUpdate: &lnrpc.PartialPolicyUpdate{
					TimeLockDelta: true,
				},
				TimeLockDelta: 80,
			},
			want: routing.ChannelPolicy{
				TimeLockDelta: fn.Some(uint32(80)),
			},
		},
		{
			name: "partial update validates time lock delta",
			req: &lnrpc.PolicyUpdateRequest{
				PartialUpdate: &lnrpc.PartialPolicyUpdate{
					TimeLockDelta: true,
				},
				TimeLockDelta: 2,
			},
			errStr: "time lock delta of 2 is too small",
		},
		{
			// A marked zero is applied, not read as "keep".
			name: "partial update rejects marked zero time lock",
			req: &lnrpc.PolicyUpdateRequest{
				PartialUpdate: &lnrpc.PartialPolicyUpdate{
					TimeLockDelta: true,
				},
			},
			errStr: "time lock delta of 0 is too small",
		},
		{
			name: "partial update rejects unmarked base fee",
			req: &lnrpc.PolicyUpdateRequest{
				PartialUpdate: &lnrpc.PartialPolicyUpdate{},
				BaseFeeMsat:   1000,
			},
			errStr: "base_fee_msat set but not marked",
		},
		{
			name: "partial update rejects unmarked fee rate",
			req: &lnrpc.PolicyUpdateRequest{
				PartialUpdate: &lnrpc.PartialPolicyUpdate{},
				FeeRate:       0.001,
			},
			errStr: "fee_rate or fee_rate_ppm set but not marked",
		},
		{
			name: "partial update rejects unmarked time lock",
			req: &lnrpc.PolicyUpdateRequest{
				PartialUpdate: &lnrpc.PartialPolicyUpdate{},
				TimeLockDelta: 80,
			},
			errStr: "time_lock_delta set but not marked",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := channelPolicyFromRequest(
				tc.req, maxCltvExpiry, false,
			)
			if tc.errStr != "" {
				require.ErrorContains(t, err, tc.errStr)
				return
			}

			require.NoError(t, err)

			// Inbound fees aren't under test here.
			tc.want.InboundFee = fn.None[models.InboundFee]()
			require.Equal(t, tc.want, got)
		})
	}
}
