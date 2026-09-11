package itest

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/lnwallet/accountbackup"
	"github.com/stretchr/testify/require"
)

// testWalletAccountBackupRecovery proves that the RPC issuance boundary
// persists both branches, fails closed without storage, and retains enough
// evidence to recover and spend internal change after losing the wallet.
func testWalletAccountBackupRecovery(ht *lntest.HarnessTest) {
	if runtime.GOOS == "windows" {
		ht.Skip("account backup requires directory fsync, " +
			"which native Windows does not support here")
	}

	const (
		name     = "treasury"
		addrType = walletrpc.AddressType_TAPROOT_PUBKEY
	)
	backup := filepath.Join(ht.TempDir(), "accounts.json")
	protected := []string{"--wallet-account-backup=" + backup}
	password := []byte("account-recovery-itest-password")
	alice, mnemonic, _ := ht.NewNodeWithSeed(
		"account-original", append(protected,
			"--wallet-account-backup-create"), password, false,
	)

	// An earlier account makes the recovery sensitive to the original
	// derivation index, rather than merely reproducing the account name.
	createAccounts := func(n *node.HarnessNode) *walletrpc.Account {
		ht.Helper()
		for _, account := range []string{"earlier", name} {
			n.RPC.XCreateAccount(&walletrpc.XCreateAccountRequest{
				Name:        account,
				AddressType: addrType,
			})
		}

		return n.RPC.ListAccounts(&walletrpc.ListAccountsRequest{
			Name: name,
		}).Accounts[0]
	}
	original := createAccounts(alice)
	// Families above the eagerly initialized range must also be recreated
	// during maintenance. DeriveKey creates the original raw account.
	keyLoc := &signrpc.KeyLocator{KeyFamily: 300, KeyIndex: 0}
	rawKey := alice.RPC.DeriveKey(keyLoc)
	addr := alice.RPC.NewAddress(&lnrpc.NewAddressRequest{
		Type:    lnrpc.AddressType_TAPROOT_PUBKEY,
		Account: name,
	}).Address
	receive := readAccountBackup(ht, backup, name)
	require.EqualValues(ht, 2, receive.Index)
	require.EqualValues(ht, 1, receive.External)
	require.Equal(ht, original.ExtendedPublicKey, receive.XPub)
	rawRecord := readAccountBackup(ht, backup, "act:300")
	require.EqualValues(ht, 1017, rawRecord.Purpose)
	require.EqualValues(ht, 300, rawRecord.Index)

	const deposit = btcutil.Amount(1_000_000)
	ht.SendOutputsWithoutChange([]*wire.TxOut{{
		Value:    int64(deposit),
		PkScript: ht.PayToAddrScript(ht.DecodeAddress(addr)),
	}}, defaultCreateAccountFeeRate)
	ht.MineBlocksAndAssertNumTxes(1, 1)
	ht.AssertWalletAccountBalance(alice, name, int64(deposit), 0)

	// A failed write must not return an address or funded PSBT. Replacing
	// the file with a directory also works when the test runs as root.
	require.NoError(ht, os.Rename(backup, backup+".saved"))
	require.NoError(ht, os.Mkdir(backup, 0700))
	ctx, cancel := context.WithTimeout(ht.Context(), wait.DefaultTimeout)
	defer cancel()
	failedAddr, err := alice.RPC.WalletKit.NextAddr(
		ctx, &walletrpc.AddrRequest{
			Account: name,
			Type:    walletrpc.AddressType_TAPROOT_PUBKEY,
		},
	)
	require.ErrorContains(ht, err, "persist account recovery")
	require.Nil(ht, failedAddr)

	dest := ht.NewNode("account-recipient", nil).RPC.NewAddress(
		&lnrpc.NewAddressRequest{
			Type: lnrpc.AddressType_TAPROOT_PUBKEY,
		},
	).Address
	fundReq := &walletrpc.FundPsbtRequest{
		Template: &walletrpc.FundPsbtRequest_Raw{
			Raw: &walletrpc.TxTemplate{
				Outputs: map[string]uint64{dest: 200_000},
			},
		},
		Fees: &walletrpc.FundPsbtRequest_SatPerVbyte{
			SatPerVbyte: 5,
		},
		Account: name,
	}
	failedPsbt, err := alice.RPC.WalletKit.FundPsbt(ctx, fundReq)
	require.ErrorContains(ht, err, "persist account recovery")
	require.Nil(ht, failedPsbt)
	require.NoError(ht, os.Remove(backup))
	require.NoError(ht, os.Rename(backup+".saved", backup))

	// Read the file immediately after FundPsbt returns, before signing or
	// publication. A later timer cannot satisfy this assertion.
	funded := alice.RPC.FundPsbt(fundReq)
	saved := readAccountBackup(ht, backup, name)
	current := alice.RPC.ListAccounts(&walletrpc.ListAccountsRequest{
		Name: name,
	}).Accounts[0]
	require.Equal(ht, current.ExternalKeyCount, saved.External)
	require.Equal(ht, current.InternalKeyCount, saved.Internal)
	require.Positive(ht, saved.Internal)
	require.Greater(ht, saved.External, receive.External)
	expected := publishAccountBackupPsbt(ht, alice, funded, name)
	ht.AssertWalletAccountBalance(alice, name, expected, 0)

	// Normal restart must accept the existing file without reenrollment.
	ht.RestartNodeWithExtraArgs(alice, protected)
	require.Equal(ht, saved, readAccountBackup(ht, backup, name))
	ht.Shutdown(alice)
	require.NoDirExists(ht, alice.Cfg.DBDir())

	// A new data directory receives only the seed. Recreate the original
	// account index and receive branch, deliberately omitting change.
	restored := ht.RestoreNodeWithSeed(
		"account-restored", nil, password, mnemonic, "", 0, nil,
	)
	rebuilt := createAccounts(restored)
	require.Equal(ht, original.DerivationPath, rebuilt.DerivationPath)
	require.Equal(ht, original.ExtendedPublicKey, rebuilt.ExtendedPublicKey)
	rebuiltKey := restored.RPC.DeriveKey(keyLoc)
	require.Equal(ht, rawKey.RawKeyBytes, rebuiltKey.RawKeyBytes)
	derive := func(change bool, count uint32) {
		ht.Helper()
		for range count {
			_, err := restored.RPC.WalletKit.NextAddr(
				ht.Context(), &walletrpc.AddrRequest{
					Account: name,
					Type:    addrType,
					Change:  change,
				},
			)
			require.NoError(ht, err)
		}
	}
	derive(false, saved.External)

	// Launch the stopped wallet directly so an expected startup failure
	// does not wait for the harness's successful-RPC startup timeout.
	require.NoError(ht, restored.Stop())
	passwordFile := filepath.Join(ht.TempDir(), "password")
	require.NoError(ht, os.WriteFile(passwordFile, password, 0600))
	restored.SetExtraArgs(append(protected,
		"--wallet-unlock-password-file="+passwordFile))
	startupCtx, startupCancel := context.WithTimeout(
		ht.Context(), wait.DefaultTimeout,
	)
	output, err := exec.CommandContext(
		startupCtx, restored.Cfg.LndBinary,
		restored.Cfg.GenArgs()...,
	).CombinedOutput()
	require.Error(ht, err)
	require.NoError(ht, startupCtx.Err(), "startup did not fail promptly")
	startupCancel()
	require.Contains(ht, string(output), "reconstruct both "+
		"named-account branches before startup")
	require.Equal(ht, saved, readAccountBackup(ht, backup, name))

	// Rescanning without the guard demonstrates the silent loss this
	// feature prevents: all remaining funds are on the missing branch.
	restored.SetExtraArgs([]string{
		"--reset-wallet-transactions",
	})
	require.NoError(ht, restored.StartWithNoAuth(ht.Context()))
	require.NoError(ht, restored.Unlock(&lnrpc.UnlockWalletRequest{
		WalletPassword: password,
	}))
	ht.WaitForBlockchainSync(restored)
	ht.AssertWalletAccountBalance(restored, name, 0, 0)
	derive(true, saved.Internal)
	ht.RestartNodeWithExtraArgs(restored, append(protected,
		"--reset-wallet-transactions"))
	ht.AssertWalletAccountBalance(restored, name, expected, 0)

	// A confirmed spend proves recovery of signing keys as well as the
	// public balance and the internal change outpoint.
	fundReq.Template = &walletrpc.FundPsbtRequest_Raw{
		Raw: &walletrpc.TxTemplate{
			Outputs: map[string]uint64{dest: 100_000},
		},
	}
	funded = restored.RPC.FundPsbt(fundReq)
	remaining := publishAccountBackupPsbt(ht, restored, funded, name)
	require.Less(ht, remaining, expected-100_000)
	ht.AssertWalletAccountBalance(restored, name, remaining, 0)
	ht.Logf("Recovered %d sat of internal change and confirmed "+
		"a spend; %d sat remains", expected, remaining)
}

// readAccountBackup reads durable evidence directly, without another wallet
// call or a polling interval that could hide an asynchronous write.
func readAccountBackup(ht *lntest.HarnessTest, path,
	name string) accountbackup.Account {

	ht.Helper()
	data, err := os.ReadFile(path)
	require.NoError(ht, err)
	snapshot, err := accountbackup.Decode(data)
	require.NoError(ht, err)
	for _, account := range snapshot.Accounts {
		if account.Name == name {
			return account
		}
	}
	ht.Fatalf("missing recovery record for %s", name)

	return accountbackup.Account{}
}

// publishAccountBackupPsbt confirms a named-account spend and returns the
// exact value of its change output for the subsequent recovery assertion.
func publishAccountBackupPsbt(ht *lntest.HarnessTest, n *node.HarnessNode,
	funded *walletrpc.FundPsbtResponse, name string) int64 {

	ht.Helper()
	require.GreaterOrEqual(ht, funded.ChangeOutputIndex, int32(0))
	packet, err := psbt.NewFromRawBytes(
		bytes.NewReader(funded.FundedPsbt), false,
	)
	require.NoError(ht, err)
	change := packet.UnsignedTx.TxOut[funded.ChangeOutputIndex].Value
	finalized := n.RPC.FinalizePsbt(&walletrpc.FinalizePsbtRequest{
		FundedPsbt: funded.FundedPsbt,
		Account:    name,
	})
	n.RPC.PublishTransaction(&walletrpc.Transaction{
		TxHex: finalized.RawFinalTx,
	})
	ht.MineBlocksAndAssertNumTxes(1, 1)

	return change
}
