package btcwallet

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/btcsuite/btcwallet/wallet"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/accountbackup"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
)

// readAccountBackup reads only synthetic test-wallet metadata and locates the
// named record without exposing its xpub in logs or failure messages.
func readAccountBackup(t *testing.T, path, name string) accountbackup.Account {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	snap, err := accountbackup.Decode(data)
	require.NoError(t, err)
	for _, a := range snap.Accounts {
		if a.Name == name {
			return a
		}
	}
	t.Fatal("named account missing from durable recovery record")

	return accountbackup.Account{}
}

// TestAccountBackupIssuance exercises real wallet allocation and PSBT change.
// Failed persistence occurs after allocation but must prevent a useful result.
func TestAccountBackupIssuance(t *testing.T) {
	w, miner := newTestWallet(t, netParams, seedBytes)
	t.Cleanup(func() { require.NoError(t, w.Stop()) })
	path := filepath.Join(t.TempDir(), "accounts.json")
	w.cfg.AccountBackupPath = path
	w.cfg.AccountBackupCreate = true
	require.NoError(t, w.openAccountBackup())
	_, err := w.CreateAccount(waddrmgr.KeyScopeBIP0086, "treasury")
	require.NoError(t, err)
	require.EqualValues(
		t, 0, readAccountBackup(t, path, "treasury").Internal,
	)
	addr, err := w.NewAddress(lnwallet.TaprootPubkey, false, "treasury")
	require.NoError(t, err)
	require.EqualValues(
		t, 1, readAccountBackup(t, path, "treasury").External,
	)
	script, err := txscript.PayToAddrScript(addr)
	require.NoError(t, err)
	_, err = miner.SendOutputs(
		[]*wire.TxOut{
			{Value: 100000, PkScript: script},
		},
		1000,
	)
	require.NoError(t, err)
	require.Eventually(
		t,
		func() bool {
			balance, err := w.ConfirmedBalance(0, "treasury")
			return err == nil && balance == 100000
		},
		30*time.Second, 20*time.Millisecond,
	)
	target, err := miner.NewAddress()
	require.NoError(t, err)
	targetScript, err := txscript.PayToAddrScript(target)
	require.NoError(t, err)
	makePacket := func() *psbt.Packet {
		tx := wire.NewMsgTx(2)
		tx.AddTxOut(&wire.TxOut{Value: 20000, PkScript: targetScript})
		packet, err := psbt.NewFromUnsignedTx(tx)
		require.NoError(t, err)

		return packet
	}
	packet := makePacket()
	_, err = w.FundPsbt(
		packet, 0, chainfee.SatPerKWeight(2500),
		"treasury", nil, wallet.CoinSelectionLargest, nil,
	)
	require.NoError(t, err)
	recorded := readAccountBackup(t, path, "treasury")
	require.Positive(t, recorded.Internal)
	require.NoError(t, w.FinalizePsbt(packet, "treasury"))
	_, err = psbt.Extract(packet)
	require.NoError(t, err)

	// Preserve the good copy while making the configured destination
	// unreadable.
	require.NoError(t, os.Rename(path, path+".saved"))
	require.NoError(t, os.Mkdir(path, 0700))
	addr, err = w.NewAddress(lnwallet.TaprootPubkey, false, "treasury")
	require.Error(t, err)
	require.Nil(t, addr)
	_, err = w.FundPsbt(
		makePacket(), 0, chainfee.SatPerKWeight(2500),
		"treasury", nil, wallet.CoinSelectionLargest, nil,
	)
	require.Error(t, err)
	_, err = w.NewAddress(
		lnwallet.TaprootPubkey, false, lnwallet.DefaultAccountName,
	)
	require.NoError(
		t, err,
		"default account must not depend on account backup storage",
	)
	require.NoError(t, os.Remove(path))
	require.NoError(t, os.Rename(path+".saved", path))
	_, err = w.LastUnusedAddress(lnwallet.TaprootPubkey, "treasury")
	require.NoError(t, err)
	latest := readAccountBackup(t, path, "treasury")
	require.Greater(t, latest.External, recorded.External)
	require.Greater(t, latest.Internal, recorded.Internal)
}
