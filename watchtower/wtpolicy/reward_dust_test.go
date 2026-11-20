package wtpolicy_test

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/watchtower/blob"
	"github.com/lightningnetwork/lnd/watchtower/wtpolicy"
	"github.com/stretchr/testify/require"
)

func TestRewardOutputDust(t *testing.T) {
	sweepScript := make([]byte, 22)
	rewardScript := make([]byte, 34)
	sweepDust := lnwallet.DustLimitForSize(len(sweepScript))
	rewardDust := lnwallet.DustLimitForSize(len(rewardScript))
	require.Less(t, sweepDust, rewardDust)
	tests := []struct {
		name          string
		sweep, reward btcutil.Amount
		dusty         bool
	}{
		{"dusty reward", 10000, rewardDust - 1, true},
		{"dusty sweep", sweepDust - 1, 10000, true},
		{"each at own limit", sweepDust, rewardDust, false},
		{"sweep below reward limit", sweepDust, 10000, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := wtpolicy.Policy{TxPolicy: wtpolicy.TxPolicy{
				BlobType:   blob.TypeRewardCommit,
				RewardBase: uint32(tc.reward),
			}}
			outs, err := p.ComputeJusticeTxOuts(
				tc.sweep+tc.reward, 0,
				sweepScript, rewardScript,
			)
			if tc.dusty {
				require.ErrorIs(
					t, err, wtpolicy.ErrCreatesDust,
				)
				require.Nil(t, outs)

				return
			}
			require.NoError(t, err)
			require.Len(t, outs, 2)
			require.Equal(t, int64(tc.sweep), outs[0].Value)
			require.Equal(t, int64(tc.reward), outs[1].Value)
			require.Equal(t, sweepScript, outs[0].PkScript)
			require.Equal(t, rewardScript, outs[1].PkScript)
		})
	}

	// Reversing script sizes must not lower the sweep dust threshold.
	p := wtpolicy.Policy{TxPolicy: wtpolicy.TxPolicy{
		BlobType: blob.TypeRewardCommit, RewardBase: uint32(sweepDust),
	}}
	_, err := p.ComputeJusticeTxOuts(
		rewardDust-1+sweepDust, 0, rewardScript, sweepScript,
	)
	require.ErrorIs(t, err, wtpolicy.ErrCreatesDust)

	// Altruist sessions continue to produce a single sweep output.
	p = wtpolicy.Policy{TxPolicy: wtpolicy.TxPolicy{
		BlobType: blob.TypeAltruistCommit,
	}}
	outs, err := p.ComputeJusticeTxOuts(sweepDust, 0, sweepScript, nil)
	require.NoError(t, err)
	require.Len(t, outs, 1)
	require.Equal(t, int64(sweepDust), outs[0].Value)
}
