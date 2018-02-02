package walletunlocker_test

import (
	"bytes"
	"io/ioutil"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/aezeed"
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

	testEntropy = [aezeed.EntropySize]byte{
		0x81, 0xb6, 0x37, 0xd8,
		0x63, 0x59, 0xe6, 0x96,
		0x0d, 0xe7, 0x95, 0xe4,
		0x1e, 0x0b, 0x4c, 0xfd,
	}

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

// TestGenSeedUserEntropy tests that the gen seed method generates a valid
// cipher seed mnemonic phrase and user provided source of entropy.
func TestGenSeed(t *testing.T) {
	t.Parallel()

	// First, we'll create a new test directory and unlocker service for
	// that directory.
	testDir, err := ioutil.TempDir("", "testcreate")
	if err != nil {
		t.Fatalf("unable to create temp directory: %v", err)
	}
	defer func() {
		os.RemoveAll(testDir)
	}()
	service := walletunlocker.New(nil, testDir, testNetParams)

	// Now that the service has been created, we'll ask it to generate a
	// new seed for us given a test passphrase.
	aezeedPass := []byte("kek")
	genSeedReq := &lnrpc.GenSeedRequest{
		AezeedPassphrase: aezeedPass,
		SeedEntropy:      testEntropy[:],
	}

	ctx := context.Background()
	seedResp, err := service.GenSeed(ctx, genSeedReq)
	if err != nil {
		t.Fatalf("unable to generate seed: %v", err)
	}

	// We should then be able to take the generated mnemonic, and properly
	// decipher both it.
	var mnemonic aezeed.Mnemonic
	copy(mnemonic[:], seedResp.CipherSeedMnemonic[:])
	_, err = mnemonic.ToCipherSeed(aezeedPass)
	if err != nil {
		t.Fatalf("unable to decipher cipher seed: %v", err)
	}
}

// TestGenSeedInvalidEntropy tests that the gen seed method generates a valid
// cipher seed mnemonic pass phrase even when the user doesn't provide its own
// source of entropy.
func TestGenSeedGenerateEntropy(t *testing.T) {
	t.Parallel()

	// First, we'll create a new test directory and unlocker service for
	// that directory.
	testDir, err := ioutil.TempDir("", "testcreate")
	if err != nil {
		t.Fatalf("unable to create temp directory: %v", err)
	}
	defer func() {
		os.RemoveAll(testDir)
	}()
	service := walletunlocker.New(nil, testDir, testNetParams)

	// Now that the service has been created, we'll ask it to generate a
	// new seed for us given a test passphrase. Note that we don't actually
	aezeedPass := []byte("kek")
	genSeedReq := &lnrpc.GenSeedRequest{
		AezeedPassphrase: aezeedPass,
	}

	ctx := context.Background()
	seedResp, err := service.GenSeed(ctx, genSeedReq)
	if err != nil {
		t.Fatalf("unable to generate seed: %v", err)
	}

	// We should then be able to take the generated mnemonic, and properly
	// decipher both it.
	var mnemonic aezeed.Mnemonic
	copy(mnemonic[:], seedResp.CipherSeedMnemonic[:])
	_, err = mnemonic.ToCipherSeed(aezeedPass)
	if err != nil {
		t.Fatalf("unable to decipher cipher seed: %v", err)
	}
}

// TestGenSeedInvalidEntropy tests that if a user attempt to create a seed with
// the wrong number of bytes for the initial entropy, then the proper error is
// returned.
func TestGenSeedInvalidEntropy(t *testing.T) {
	t.Parallel()

	// First, we'll create a new test directory and unlocker service for
	// that directory.
	testDir, err := ioutil.TempDir("", "testcreate")
	if err != nil {
		t.Fatalf("unable to create temp directory: %v", err)
	}
	defer func() {
		os.RemoveAll(testDir)
	}()
	service := walletunlocker.New(nil, testDir, testNetParams)

	// Now that the service has been created, we'll ask it to generate a
	// new seed for us given a test passphrase. However, we'll be using an
	// invalid set of entropy that's 55 bytes, instead of 15 bytes.
	aezeedPass := []byte("kek")
	genSeedReq := &lnrpc.GenSeedRequest{
		AezeedPassphrase: aezeedPass,
		SeedEntropy:      bytes.Repeat([]byte("a"), 55),
	}

	// We should get an error now since the entropy source was invalid.
	ctx := context.Background()
	_, err = service.GenSeed(ctx, genSeedReq)
	if err == nil {
		t.Fatalf("seed creation should've failed")
	}

	if !strings.Contains(err.Error(), "incorrect entropy length") {
		t.Fatalf("wrong error, expected incorrect entropy length")
	}
}

// TestInitWallet tests that the user is able to properly initialize the wallet
// given an existing cipher seed passphrase.
func TestInitWallet(t *testing.T) {
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

	// Once we have the unlocker service created, we'll now instantiate a
	// new cipher seed instance.
	cipherSeed, err := aezeed.New(0, &testEntropy, time.Now())
	if err != nil {
		t.Fatalf("unable to create seed: %v", err)
	}

	// With the new seed created, we'll convert it into a mnemonic phrase
	// that we'll send over to initialize the wallet.
	pass := []byte("test")
	mnemonic, err := cipherSeed.ToMnemonic(pass)
	if err != nil {
		t.Fatalf("unable to create mnemonic: %v", err)
	}

	// Now that we have all the necessary items, we'll now issue the Init
	// command to the wallet. This should check the validity of the cipher
	// seed, then send over the initialization information over the init
	// channel.
	ctx := context.Background()
	req := &lnrpc.InitWalletRequest{
		WalletPassword:     testPassword,
		CipherSeedMnemonic: []string(mnemonic[:]),
		AezeedPassphrase:   pass,
	}
	_, err = service.InitWallet(ctx, req)
	if err != nil {
		t.Fatalf("InitWallet call failed: %v", err)
	}

	// The same user passphrase, and also the plaintext cipher seed
	// should be sent over and match exactly.
	select {
	case msg := <-service.InitMsgs:
		if !bytes.Equal(msg.Passphrase, testPassword) {
			t.Fatalf("expected to receive password %x, "+
				"got %x", testPassword, msg.Passphrase)
		}
		if msg.WalletSeed.InternalVersion != cipherSeed.InternalVersion {
			t.Fatalf("mismatched versions: expected %v, "+
				"got %v", cipherSeed.InternalVersion,
				msg.WalletSeed.InternalVersion)
		}
		if msg.WalletSeed.Birthday != cipherSeed.Birthday {
			t.Fatalf("mismatched birthday: expected %v, "+
				"got %v", cipherSeed.Birthday,
				msg.WalletSeed.Birthday)
		}
		if msg.WalletSeed.Entropy != cipherSeed.Entropy {
			t.Fatalf("mismatched versions: expected %x, "+
				"got %x", cipherSeed.Entropy[:],
				msg.WalletSeed.Entropy[:])
		}

	case <-time.After(3 * time.Second):
		t.Fatalf("password not received")
	}

	// Create a wallet in testDir.
	createTestWallet(t, testDir, testNetParams)

	// Now calling InitWallet should fail, since a wallet already exists in
	// the directory.
	_, err = service.InitWallet(ctx, req)
	if err == nil {
		t.Fatalf("InitWallet did not fail as expected")
	}

	// Similarly, if we try to do GenSeed again, we should get an error as
	// the wallet already exists.
	_, err = service.GenSeed(ctx, &lnrpc.GenSeedRequest{})
	if err == nil {
		t.Fatalf("seed generation should have failed")
	}
}

// TestInitWalletInvalidCipherSeed tests that if we attempt to create a wallet
// with an invalid cipher seed, then we'll receive an error.
func TestCreateWalletInvalidEntropy(t *testing.T) {
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

	// We'll attempt to init the wallet with an invalid cipher seed and
	// passphrase.
	req := &lnrpc.InitWalletRequest{
		WalletPassword:     testPassword,
		CipherSeedMnemonic: []string{"invalid", "seed"},
		AezeedPassphrase:   []byte("fake pass"),
	}

	ctx := context.Background()
	_, err = service.InitWallet(ctx, req)
	if err == nil {
		t.Fatalf("wallet creation should have failed")
	}
}

// TestUnlockWallet checks that trying to unlock non-existing wallet fail, that
// unlocking existing wallet with wrong passphrase fails, and that unlocking
// existing wallet with correct passphrase succeeds.
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
		WalletPassword: testPassword,
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
		WalletPassword: []byte("wrong-ofc"),
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
