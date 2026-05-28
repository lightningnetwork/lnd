package commands

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// mockNodeInfoClient implements nodeInfoClient with scripted responses.
type mockNodeInfoClient struct {
	infoResp   *lnrpc.GetInfoResponse
	infoErr    error
	walletResp *lnrpc.WalletBalanceResponse
	walletErr  error
	chanResp   *lnrpc.ChannelBalanceResponse
	chanErr    error
}

var _ nodeInfoClient = (*mockNodeInfoClient)(nil)

func (m *mockNodeInfoClient) GetInfo(_ context.Context, _ *lnrpc.GetInfoRequest, _ ...grpc.CallOption) (*lnrpc.GetInfoResponse, error) {
	return m.infoResp, m.infoErr
}

func (m *mockNodeInfoClient) WalletBalance(_ context.Context, _ *lnrpc.WalletBalanceRequest, _ ...grpc.CallOption) (*lnrpc.WalletBalanceResponse, error) {
	return m.walletResp, m.walletErr
}

func (m *mockNodeInfoClient) ChannelBalance(_ context.Context, _ *lnrpc.ChannelBalanceRequest, _ ...grpc.CallOption) (*lnrpc.ChannelBalanceResponse, error) {
	return m.chanResp, m.chanErr
}

// captureStdout runs f and returns everything it wrote to os.Stdout.
func captureStdout(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	require.NoError(t, err)

	old := os.Stdout
	os.Stdout = w

	f()

	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	_, err = io.Copy(&buf, r)
	require.NoError(t, err)
	return buf.String()
}

// makeFullClient returns a mock with all three responses populated.
func makeFullClient() *mockNodeInfoClient {
	return &mockNodeInfoClient{
		infoResp: &lnrpc.GetInfoResponse{
			IdentityPubkey:      "03aabbccdd",
			Alias:               "alice",
			Color:               "#ff0000",
			BlockHeight:         800_000,
			SyncedToChain:       true,
			NumActiveChannels:   2,
			NumInactiveChannels: 1,
			NumPendingChannels:  0,
			NumPeers:            3,
			Chains: []*lnrpc.Chain{
				{Chain: "bitcoin", Network: "mainnet"},
			},
			Uris: []string{"03aabbccdd@1.2.3.4:9735"},
		},
		walletResp: &lnrpc.WalletBalanceResponse{
			ConfirmedBalance:   5_000_000,
			UnconfirmedBalance: 0,
		},
		chanResp: &lnrpc.ChannelBalanceResponse{
			LocalBalance:  &lnrpc.Amount{Sat: 1_000_000},
			RemoteBalance: &lnrpc.Amount{Sat: 500_000},
		},
	}
}

// ── commaSats ────────────────────────────────────────────────────────────────

func TestCommaSats(t *testing.T) {
	tests := []struct {
		sats int64
		want string
	}{
		{0, "0 sats"},
		{1, "1 sats"},
		{999, "999 sats"},
		{1_000, "1,000 sats"},
		{1_234, "1,234 sats"},
		{999_999, "999,999 sats"},
		{1_000_000, "1,000,000 sats"},
		{1_234_567, "1,234,567 sats"},
		{21_000_000_00000000, "2,100,000,000,000,000 sats"},
		{-1_000, "-1,000 sats"},
		{-1_234_567, "-1,234,567 sats"},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.want, func(t *testing.T) {
			require.Equal(t, tc.want, commaSats(tc.sats))
		})
	}
}

// ── nodeInfoFromClient: RPC error propagation ─────────────────────────────────

func TestNodeInfoGetInfoError(t *testing.T) {
	errBoom := errors.New("connection refused")
	client := makeFullClient()
	client.infoErr = errBoom

	err := nodeInfoFromClient(context.Background(), false, client)
	require.ErrorIs(t, err, errBoom)
}

func TestNodeInfoWalletBalanceError(t *testing.T) {
	errBoom := errors.New("wallet locked")
	client := makeFullClient()
	client.walletErr = errBoom

	err := nodeInfoFromClient(context.Background(), false, client)
	require.ErrorIs(t, err, errBoom)
}

func TestNodeInfoChannelBalanceError(t *testing.T) {
	errBoom := errors.New("db unavailable")
	client := makeFullClient()
	client.chanErr = errBoom

	err := nodeInfoFromClient(context.Background(), false, client)
	require.ErrorIs(t, err, errBoom)
}

// ── printNodeInfo: formatted output ──────────────────────────────────────────

func TestPrintNodeInfoIdentity(t *testing.T) {
	client := makeFullClient()
	out := captureStdout(t, func() {
		require.NoError(t, nodeInfoFromClient(context.Background(), false, client))
	})

	require.Contains(t, out, "03aabbccdd")
	require.Contains(t, out, "alice")
	require.Contains(t, out, "#ff0000")
	require.Contains(t, out, "bitcoin/mainnet")
	require.Contains(t, out, "800000")
	require.Contains(t, out, "synced")
	require.Contains(t, out, "03aabbccdd@1.2.3.4:9735")
}

func TestPrintNodeInfoOnChainBalance(t *testing.T) {
	client := makeFullClient()
	out := captureStdout(t, func() {
		require.NoError(t, nodeInfoFromClient(context.Background(), false, client))
	})

	require.Contains(t, out, "5,000,000 sats")
	// No unconfirmed balance — line should not appear.
	require.NotContains(t, out, "Unconfirmed")
}

func TestPrintNodeInfoUnconfirmedBalance(t *testing.T) {
	client := makeFullClient()
	client.walletResp.UnconfirmedBalance = 250_000

	out := captureStdout(t, func() {
		require.NoError(t, nodeInfoFromClient(context.Background(), false, client))
	})

	require.Contains(t, out, "250,000 sats")
	require.Contains(t, out, "Unconfirmed")
}

func TestPrintNodeInfoChannels(t *testing.T) {
	client := makeFullClient()
	out := captureStdout(t, func() {
		require.NoError(t, nodeInfoFromClient(context.Background(), false, client))
	})

	require.Contains(t, out, "active 2")
	require.Contains(t, out, "inactive 1")
	// Capacity = local + remote = 1,500,000.
	require.Contains(t, out, "1,500,000 sats")
	// Local is 1,000,000 / 1,500,000 = 66%.
	require.Contains(t, out, "66% local")
	require.Contains(t, out, "Peers")
	require.Contains(t, out, "3")
}

func TestPrintNodeInfoNoChannels(t *testing.T) {
	client := makeFullClient()
	client.chanResp = &lnrpc.ChannelBalanceResponse{}
	client.infoResp.NumActiveChannels = 0
	client.infoResp.NumInactiveChannels = 0

	out := captureStdout(t, func() {
		require.NoError(t, nodeInfoFromClient(context.Background(), false, client))
	})

	// With zero total capacity, balance/capacity lines must not appear.
	require.NotContains(t, out, "Capacity")
	require.NotContains(t, out, "Balance")
	require.Contains(t, out, "active 0")
}

func TestPrintNodeInfoPendingChannels(t *testing.T) {
	client := makeFullClient()
	client.chanResp.PendingOpenLocalBalance = &lnrpc.Amount{Sat: 400_000}
	client.chanResp.PendingOpenRemoteBalance = &lnrpc.Amount{Sat: 600_000}
	client.infoResp.NumPendingChannels = 1

	out := captureStdout(t, func() {
		require.NoError(t, nodeInfoFromClient(context.Background(), false, client))
	})

	require.Contains(t, out, "Pending open")
	require.Contains(t, out, "400,000 sats")
	require.Contains(t, out, "600,000 sats")
}

func TestPrintNodeInfoNoAlias(t *testing.T) {
	client := makeFullClient()
	client.infoResp.Alias = ""

	out := captureStdout(t, func() {
		require.NoError(t, nodeInfoFromClient(context.Background(), false, client))
	})

	require.Contains(t, out, "(no alias)")
}

func TestPrintNodeInfoNotSynced(t *testing.T) {
	client := makeFullClient()
	client.infoResp.SyncedToChain = false

	out := captureStdout(t, func() {
		require.NoError(t, nodeInfoFromClient(context.Background(), false, client))
	})

	require.Contains(t, out, "NOT synced")
}

func TestPrintNodeInfoNoURIs(t *testing.T) {
	client := makeFullClient()
	client.infoResp.Uris = nil

	out := captureStdout(t, func() {
		require.NoError(t, nodeInfoFromClient(context.Background(), false, client))
	})

	require.NotContains(t, out, "URI")
}

// ── JSON output ───────────────────────────────────────────────────────────────

func TestNodeInfoJSONOutput(t *testing.T) {
	client := makeFullClient()
	out := captureStdout(t, func() {
		require.NoError(t, nodeInfoFromClient(context.Background(), true, client))
	})

	// Top-level keys from the merged struct.
	require.Contains(t, out, `"info"`)
	require.Contains(t, out, `"wallet"`)
	require.Contains(t, out, `"channel"`)
	// Spot-check a field from each sub-response.
	require.Contains(t, out, `"identity_pubkey"`)
	require.Contains(t, out, `"confirmed_balance"`)
	require.Contains(t, out, `"local_balance"`)
	// Must be valid JSON: no formatted text.
	require.True(t, strings.HasPrefix(strings.TrimSpace(out), "{"))
}
