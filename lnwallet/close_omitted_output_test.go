package lnwallet

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/stretchr/testify/require"
)

// TestCooperativeCloseOmittedRemoteOutput verifies both signatures cover the
// same transaction when the closer omits its non-dust output.
func TestCooperativeCloseOmittedRemoteOutput(t *testing.T) {
	alice, bob, err := CreateTestChannels(
		t, channeldb.SingleFunderTweaklessBit,
	)
	require.NoError(t, err)
	aliceScript, bobScript := bobsPrivKey, testHdSeed[:]
	fee := btcutil.Amount(10000)
	omitAlice := WithCustomCoopSort(func(tx *wire.MsgTx) error {
		var outs []*wire.TxOut
		for _, out := range tx.TxOut {
			if !bytes.Equal(out.PkScript, aliceScript) {
				outs = append(outs, out)
			}
		}
		tx.TxOut = outs

		return nil
	})
	aliceSig, aliceTx, _, err := alice.CreateCloseProposal(
		fee, aliceScript, bobScript, omitAlice,
	)
	require.NoError(t, err)
	bobSig, bobTx, _, err := bob.CreateCloseProposal(
		fee, bobScript, aliceScript, WithOmittedRemoteCloseOutput(),
	)
	require.NoError(t, err)
	require.Len(t, aliceTx.TxOut, 1)
	require.Equal(t, aliceTx.TxHash(), bobTx.TxHash())
	tx, _, err := bob.CompleteCooperativeClose(
		bobSig, aliceSig, bobScript, aliceScript, fee,
		WithOmittedRemoteCloseOutput(),
	)
	require.NoError(t, err)
	require.Equal(t, aliceTx.TxHash(), tx.TxHash())
}
