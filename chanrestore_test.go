package lnd

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightningnetwork/lnd/chanbackup"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntest/mock"
	"github.com/stretchr/testify/require"
)

func TestOpenChannelShellTapscriptRoot(t *testing.T) {
	t.Parallel()

	privateKey, _ := btcec.PrivKeyFromBytes([]byte{1})
	restorer := &chanDBRestorer{
		secretKeys: &mock.SecretKeyRing{
			RootKey: privateKey,
		},
	}

	tapscriptRoot := chainhash.Hash{1}
	backup := chanbackup.Single{
		Version: chanbackup.TapscriptRootVersion,
		ShaChainRootDesc: keychain.KeyDescriptor{
			PubKey: privateKey.PubKey(),
		},
		CloseTxInputs: fn.Some(chanbackup.CloseTxInputs{
			TapscriptRoot: fn.Some(tapscriptRoot),
		}),
	}

	shell, err := restorer.openChannelShell(backup)
	require.NoError(t, err)
	require.Equal(
		t, tapscriptRoot, shell.Chan.TapscriptRoot.UnwrapOrFail(t),
	)

	t.Run("backup without close tx inputs", func(t *testing.T) {
		backup.CloseTxInputs = fn.None[chanbackup.CloseTxInputs]()

		shell, err := restorer.openChannelShell(backup)
		require.NoError(t, err)
		require.True(t, shell.Chan.TapscriptRoot.IsNone())
	})
}
