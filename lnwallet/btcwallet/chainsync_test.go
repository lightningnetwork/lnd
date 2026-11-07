package btcwallet

import (
	"testing"
	"time"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btcwallet/waddrmgr"
	basewallet "github.com/btcsuite/btcwallet/wallet"
	"github.com/lightningnetwork/lnd/lnmock"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// mockSyncedWallet is a minimal base wallet fake that satisfies the
// base.Interface used by BtcWallet. It embeds the interface so the compile-time
// contract is met, but only the two methods IsSynced actually calls are
// implemented; any other call would panic on the nil embedded interface, which
// is what we want as a guard against the test drifting.
type mockSyncedWallet struct {
	basewallet.Interface

	stamp       waddrmgr.BlockStamp
	chainSynced bool
}

func (m *mockSyncedWallet) SyncedTo() waddrmgr.BlockStamp {
	return m.stamp
}

func (m *mockSyncedWallet) ChainSynced() bool {
	return m.chainSynced
}

// TestIsSyncedStaleTip asserts that the tip-staleness check in IsSynced is
// skipped when the wallet is configured for a local network (regtest/simnet)
// but still enforced otherwise. On local networks blocks are only mined on
// demand, so an idle chain tip whose timestamp is well in the past must not
// flip synced_to_chain to false.
func TestIsSyncedStaleTip(t *testing.T) {
	t.Parallel()

	const bestHeight = int32(100)

	testCases := []struct {
		name       string
		isLocalNet bool
		staleTip   bool
		wantSynced bool
	}{{
		name:       "local net stale tip is still synced",
		isLocalNet: true,
		staleTip:   true,
		wantSynced: true,
	}, {
		name:       "non-local net stale tip is not synced",
		isLocalNet: false,
		staleTip:   true,
		wantSynced: false,
	}, {
		name:       "non-local net fresh tip is synced",
		isLocalNet: false,
		staleTip:   false,
		wantSynced: true,
	}}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Pick a tip timestamp that is either comfortably
			// outside the 2-hour staleness window or right at the
			// current time.
			tipTime := time.Now()
			if tc.staleTip {
				tipTime = tipTime.Add(-3 * time.Hour)
			}

			header := &wire.BlockHeader{Timestamp: tipTime}
			bestHash := header.BlockHash()

			mockChain := &lnmock.MockChain{}
			mockChain.On("GetBestBlock").Return(
				&bestHash, bestHeight, nil,
			)
			mockChain.On("GetBlockHeader", mock.Anything).Return(
				header, nil,
			)

			// The wallet reports itself fully caught up to the same
			// height as the backend's best block, so the only thing
			// left to decide sync status is the tip-staleness check.
			w := &BtcWallet{
				wallet: &mockSyncedWallet{
					stamp: waddrmgr.BlockStamp{
						Height:    bestHeight,
						Hash:      bestHash,
						Timestamp: tipTime,
					},
					chainSynced: true,
				},
				cfg: &Config{
					ChainSource: mockChain,
					IsLocalNet:  tc.isLocalNet,
				},
			}

			synced, _, err := w.IsSynced()
			require.NoError(t, err)
			require.Equal(t, tc.wantSynced, synced)
		})
	}
}
