package walletunlocker_test

import (
	"bytes"
	"io/ioutil"
	"os"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwallet/btcwallet"
	"github.com/lightningnetwork/lnd/walletunlocker"
	"github.com/roasbeef/btcd/chaincfg"
	"github.com/roasbeef/btcwallet/wallet"
	"golang.org/x/net/context"
)

const (
	walletDbName = "wallet.db"
)

var (
	testPassword = []byte("test-password")
	testSeed     = []byte("test-seed-123456789")

	testNetParams = &chaincfg.MainNetParams
)

func createTestWallet(t *testing.T, dir string, netParams *chaincfg.Params) {
	netDir := btcwallet.NetworkDir(dir, netParams)
	loader := wallet.NewLoader(netParams, netDir)
	_, err := loader.CreateNewWallet(testPassword, testPassword, testSeed)
	if err != nil {
		t.Fatalf("failed creating wallet: %v", err)
	}
	err = loader.UnloadWallet()
	if err != nil {
		t.Fatalf("failed unloading wallet: %v", err)
	}
}

// TestCreateWallet checks that CreateWallet correctly returns a password that
// can be used for creating a wallet if no wallet exists from before, and
// returns an error when it already exists.
func TestCreateWallet(t *testing.T) {
	t.Parallel()

	// testDir is empty, meaning wallet was not created from before.
	testDir, err := ioutil.TempDir("", "testcreate")
	if err != nil {
		t.Fatalf("unable to create temp directory: %v", err)
	}
	defer func() {
		os.RemoveAll(testDir)
	}()

	// Create new UnlockerService.
	service := walletunlocker.New(nil, testDir, testNetParams)

	ctx := context.Background()
	req := &lnrpc.CreateWalletRequest{
		Password: testPassword,
	}
	_, err = service.CreateWallet(ctx, req)
	if err != nil {
		t.Fatalf("CreateWallet call failed: %v", err)
	}

	// Password should be sent over the channel.
	select {
	case pw := <-service.CreatePasswords:
		if !bytes.Equal(pw, testPassword) {
			t.Fatalf("expected to receive password %x, got %x",
				testPassword, pw)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("password not received")
	}

	// Create a wallet in testDir.
	createTestWallet(t, testDir, testNetParams)

	// Now calling CreateWallet should fail, since a wallet already exists
	// in the directory.
	_, err = service.CreateWallet(ctx, req)
	if err == nil {
		t.Fatalf("CreateWallet did not fail as expected")
	}
}

// TestUnlockWallet checks that trying to unlock non-existing wallet fail,
// that unlocking existing wallet with wrong passphrase fails, and that
// unlocking existing wallet with correct passphrase succeeds.
func TestUnlockWallet(t *testing.T) {
	t.Parallel()

	// testDir is empty, meaning wallet was not created from before.
	testDir, err := ioutil.TempDir("", "testunlock")
	if err != nil {
		t.Fatalf("unable to create temp directory: %v", err)
	}
	defer func() {
		os.RemoveAll(testDir)
	}()

	// Create new UnlockerService.
	service := walletunlocker.New(nil, testDir, testNetParams)

	ctx := context.Background()
	req := &lnrpc.UnlockWalletRequest{
		Password: testPassword,
	}

	// Should fail to unlock non-existing wallet.
	_, err = service.UnlockWallet(ctx, req)
	if err == nil {
		t.Fatalf("expected call to UnlockWallet to fail")
	}

	// Create a wallet we can try to unlock.
	createTestWallet(t, testDir, testNetParams)

	// Try unlocking this wallet with the wrong passphrase.
	wrongReq := &lnrpc.UnlockWalletRequest{
		Password: []byte("wrong-ofc"),
	}
	_, err = service.UnlockWallet(ctx, wrongReq)
	if err == nil {
		t.Fatalf("expected call to UnlockWallet to fail")
	}

	// With the correct password, we should be able to unlock the wallet.
	_, err = service.UnlockWallet(ctx, req)
	if err != nil {
		t.Fatalf("unable to unlock wallet: %v", err)
	}

	// Password should be sent over the channel.
	select {
	case pw := <-service.UnlockPasswords:
		if !bytes.Equal(pw, testPassword) {
			t.Fatalf("expected to receive password %x, got %x",
				testPassword, pw)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("password not received")
	}

}
