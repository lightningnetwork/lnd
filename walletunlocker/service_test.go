package walletunlocker_test

import (
	"bytes"
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcwallet/snacl"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/btcsuite/btcwallet/wallet"
	"github.com/lightningnetwork/lnd/aezeed"
	"github.com/lightningnetwork/lnd/channeldb/kvdb"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/btcwallet"
	"github.com/lightningnetwork/lnd/macaroons"
	"github.com/lightningnetwork/lnd/walletunlocker"
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

	testRecoveryWindow uint32 = 150
)

func createTestWallet(t *testing.T, dir string, netParams *chaincfg.Params) {
	createTestWalletWithPw(t, testPassword, testPassword, dir, netParams)
}

func createTestWalletWithPw(t *testing.T, pubPw, privPw []byte, dir string,
	netParams *chaincfg.Params) {

	// Instruct waddrmgr to use the cranked down scrypt parameters when
	// creating new wallet encryption keys.
	fastScrypt := waddrmgr.FastScryptOptions
	keyGen := func(passphrase *[]byte, config *waddrmgr.ScryptOptions) (
		*snacl.SecretKey, error) {

		return snacl.NewSecretKey(
			passphrase, fastScrypt.N, fastScrypt.R, fastScrypt.P,
		)
	}
	waddrmgr.SetSecretKeyGen(keyGen)

	// Create a new test wallet that uses fast scrypt as KDF.
	netDir := btcwallet.NetworkDir(dir, netParams)
	loader := wallet.NewLoader(netParams, netDir, true, 0)
	_, err := loader.CreateNewWallet(
		pubPw, privPw, testSeed, time.Time{},
	)
	if err != nil {
		t.Fatalf("failed creating wallet: %v", err)
	}
	err = loader.UnloadWallet()
	if err != nil {
		t.Fatalf("failed unloading wallet: %v", err)
	}
}

func createSeedAndMnemonic(t *testing.T,
	pass []byte) (*aezeed.CipherSeed, aezeed.Mnemonic) {
	cipherSeed, err := aezeed.New(
		keychain.KeyDerivationVersion, &testEntropy, time.Now(),
	)
	if err != nil {
		t.Fatalf("unable to create seed: %v", err)
	}

	// With the new seed created, we'll convert it into a mnemonic phrase
	// that we'll send over to initialize the wallet.
	mnemonic, err := cipherSeed.ToMnemonic(pass)
	if err != nil {
		t.Fatalf("unable to create mnemonic: %v", err)
	}
	return cipherSeed, mnemonic
}

// openOrCreateTestMacStore opens or creates a bbolt DB and then initializes a
// root key storage for that DB and then unlocks it, creating a root key in the
// process.
func openOrCreateTestMacStore(t *testing.T, tempDir string,
	pw *[]byte) *macaroons.RootKeyStorage {

	db, err := kvdb.Create(
		kvdb.BoltBackendName, path.Join(tempDir, macaroons.DBFilename),
		true,
	)
	if err != nil {
		t.Fatalf("Error opening store DB: %v", err)
	}

	store, err := macaroons.NewRootKeyStorage(db)
	if err != nil {
		_ = db.Close()
		t.Fatalf("Error creating root key store: %v", err)
	}

	err = store.CreateUnlock(pw)
	if err != nil {
		t.Fatalf("Error unlocking root key store: %v", err)
	}
	_, _, err = store.RootKey(context.Background())
	if err != nil {
		t.Fatalf("Error reading root key: %v", err)
	}

	return store
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
	defer os.RemoveAll(testDir)

	service := walletunlocker.New(testDir, testNetParams, true, "", nil)

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
	service := walletunlocker.New(testDir, testNetParams, true, "", nil)

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
	service := walletunlocker.New(testDir, testNetParams, true, "", nil)

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
	service := walletunlocker.New(testDir, testNetParams, true, "", nil)

	// Once we have the unlocker service created, we'll now instantiate a
	// new cipher seed and its mnemonic.
	pass := []byte("test")
	cipherSeed, mnemonic := createSeedAndMnemonic(t, pass)

	// Now that we have all the necessary items, we'll now issue the Init
	// command to the wallet. This should check the validity of the cipher
	// seed, then send over the initialization information over the init
	// channel.
	ctx := context.Background()
	req := &lnrpc.InitWalletRequest{
		WalletPassword:     testPassword,
		CipherSeedMnemonic: []string(mnemonic[:]),
		AezeedPassphrase:   pass,
		RecoveryWindow:     int32(testRecoveryWindow),
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
		if msg.RecoveryWindow != testRecoveryWindow {
			t.Fatalf("mismatched recovery window: expected %v,"+
				"got %v", testRecoveryWindow,
				msg.RecoveryWindow)
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

// TestInitWalletStateless tests the stateless wallet initialization where the
// admin macaroon is sent back in the response.
func TestInitWalletStateless(t *testing.T) {
	// testDir is empty, meaning wallet was not created from before.
	testDir, err := ioutil.TempDir("", "teststateless")
	if err != nil {
		t.Fatalf("unable to create temp directory: %v", err)
	}
	defer func() {
		os.RemoveAll(testDir)
	}()

	// Create new UnlockerService with a test mnemonic.
	service := walletunlocker.New(testDir, testNetParams, true, "", nil)
	pass := []byte("test")
	_, mnemonic := createSeedAndMnemonic(t, pass)

	// Now that we have all the necessary items, we'll now issue the Init
	// command to the wallet. This should check the validity of the cipher
	// seed, then send over the initialization information over the init
	// channel.
	ctx := context.Background()
	req := &lnrpc.InitWalletRequest{
		WalletPassword:     testPassword,
		CipherSeedMnemonic: mnemonic[:],
		AezeedPassphrase:   pass,
		RecoveryWindow:     int32(testRecoveryWindow),
		StatelessInit:      true,
	}

	// Since we requested stateless initialization, the service will block
	// until it receives the macaroon through the channel provided in the
	// message in InitMsgs. So we need to call the service async and then
	// wait for the init message to arrive so we can send back a fake
	// macaroon.
	fakeMac := []byte("fakemacaroon")
	errChan := make(chan error, 1)
	go func() {
		response, err := service.InitWallet(ctx, req)
		if err != nil {
			errChan <- err
			return
		}

		if !bytes.Equal(response.AdminMacaroon, fakeMac) {
			errChan <- fmt.Errorf("mismatched macaroon: "+
				"expected %x, got %x", fakeMac,
				response.AdminMacaroon)
		}
	}()
	select {
	case err := <-errChan:
		t.Fatalf("InitWallet call failed: %v", err)

	case msg := <-service.InitMsgs:
		// Send a fake macaroon that should be returned in the response
		// in the async code above.
		msg.MacResponseChannel <- fakeMac
	case <-time.After(10 * time.Second):
		t.Fatalf("password not received")
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
	service := walletunlocker.New(testDir, testNetParams, true, "", nil)

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
	// testDir is empty, meaning wallet was not created from before.
	testDir, err := ioutil.TempDir("", "testunlock")
	if err != nil {
		t.Fatalf("unable to create temp directory: %v", err)
	}
	defer func() {
		os.RemoveAll(testDir)
	}()

	// Create new UnlockerService.
	service := walletunlocker.New(testDir, testNetParams, true, "", nil)

	ctx := context.Background()
	req := &lnrpc.UnlockWalletRequest{
		WalletPassword: testPassword,
		RecoveryWindow: int32(testRecoveryWindow),
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

	// Password and recovery window should be sent over the channel.
	select {
	case unlockMsg := <-service.UnlockMsgs:
		if !bytes.Equal(unlockMsg.Passphrase, testPassword) {
			t.Fatalf("expected to receive password %x, got %x",
				testPassword, unlockMsg.Passphrase)
		}
		if unlockMsg.RecoveryWindow != testRecoveryWindow {
			t.Fatalf("expected to receive recovery window %d, "+
				"got %d", testRecoveryWindow,
				unlockMsg.RecoveryWindow)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("password not received")
	}
}

// TestUnlockWalletStateless checks that trying to unlock an existing wallet
// that was initialized stateless can be unlocked when the --stateless_init
// flat is set.
func TestUnlockWalletStateless(t *testing.T) {
	// testDir is empty, meaning wallet was not created from before.
	testDir, err := ioutil.TempDir("", "testunlock")
	if err != nil {
		t.Fatalf("unable to create temp directory: %v", err)
	}
	defer func() {
		os.RemoveAll(testDir)
	}()

	// Create new UnlockerService.
	service := walletunlocker.New(testDir, testNetParams, true, "", nil)

	ctx := context.Background()
	req := &lnrpc.UnlockWalletRequest{
		WalletPassword: testPassword,
		RecoveryWindow: int32(testRecoveryWindow),
		StatelessInit:  true,
	}

	// Create a wallet we can try to unlock.
	createTestWallet(t, testDir, testNetParams)

	// Since we indicated the wallet was initialized stateless, the service
	// will block until it receives the macaroon through the channel
	// provided in the message in UnlockMsgs. So we need to call the service
	// async and then wait for the unlock message to arrive so we can send
	// back a fake macaroon.
	errChan := make(chan error, 1)
	go func() {
		// With the correct password, we should be able to unlock the
		// wallet.
		_, err := service.UnlockWallet(ctx, req)
		if err != nil {
			errChan <- err
		}
	}()

	// Password and recovery window should be sent over the channel.
	select {
	case err := <-errChan:
		t.Fatalf("UnlockWallet call failed: %v", err)

	case unlockMsg := <-service.UnlockMsgs:
		if !bytes.Equal(unlockMsg.Passphrase, testPassword) {
			t.Fatalf("expected to receive password %x, got %x",
				testPassword, unlockMsg.Passphrase)
		}
		if unlockMsg.RecoveryWindow != testRecoveryWindow {
			t.Fatalf("expected to receive recovery window %d, "+
				"got %d", testRecoveryWindow,
				unlockMsg.RecoveryWindow)
		}

		// Send a fake macaroon that should be returned in the response
		// in the async code above.
		unlockMsg.MacResponseChannel <- []byte("fakemacaroon")
	case <-time.After(30 * time.Second):
		t.Fatalf("password not received")
	}
}

// TestChangeWalletPassword tests that we can successfully change the wallet's
// password needed to unlock it.
func TestChangeWalletPassword(t *testing.T) {
	// testDir is empty, meaning wallet was not created from before.
	testDir, err := ioutil.TempDir("", "testchangepassword")
	if err != nil {
		t.Fatalf("unable to create temp directory: %v", err)
	}
	defer os.RemoveAll(testDir)

	// Changing the password of the wallet will also try to change the
	// password of the macaroon DB. We create a default DB here but close it
	// immediately so the service does not fail when trying to open it.
	store := openOrCreateTestMacStore(t, testDir, &testPassword)
	store.Close()

	// Create some files that will act as macaroon files that should be
	// deleted after a password change is successful.
	var tempFiles []string
	for i := 0; i < 3; i++ {
		file, err := ioutil.TempFile(testDir, "")
		if err != nil {
			t.Fatalf("unable to create temp file: %v", err)
		}
		tempFiles = append(tempFiles, file.Name())
		file.Close()
	}

	// Create a new UnlockerService with our temp files.
	service := walletunlocker.New(
		testDir, testNetParams, true, testDir, tempFiles,
	)

	ctx := context.Background()
	newPassword := []byte("hunter2???")

	req := &lnrpc.ChangePasswordRequest{
		CurrentPassword: testPassword,
		NewPassword:     newPassword,
	}

	// Changing the password to a non-existing wallet should fail.
	_, err = service.ChangePassword(ctx, req)
	if err == nil {
		t.Fatal("expected call to ChangePassword to fail")
	}

	// Create a wallet to test changing the password.
	createTestWallet(t, testDir, testNetParams)

	// Attempting to change the wallet's password using an incorrect
	// current password should fail.
	wrongReq := &lnrpc.ChangePasswordRequest{
		CurrentPassword: []byte("wrong-ofc"),
		NewPassword:     newPassword,
	}
	_, err = service.ChangePassword(ctx, wrongReq)
	if err == nil {
		t.Fatal("expected call to ChangePassword to fail")
	}

	// The files should still exist after an unsuccessful attempt to change
	// the wallet's password.
	for _, tempFile := range tempFiles {
		if _, err := os.Stat(tempFile); os.IsNotExist(err) {
			t.Fatal("file does not exist but it should")
		}
	}

	// Attempting to change the wallet's password using an invalid
	// new password should fail.
	wrongReq.NewPassword = []byte("8")
	_, err = service.ChangePassword(ctx, wrongReq)
	if err == nil {
		t.Fatal("expected call to ChangePassword to fail")
	}

	// When providing the correct wallet's current password and a new
	// password that meets the length requirement, the password change
	// should succeed.
	_, err = service.ChangePassword(ctx, req)
	if err != nil {
		t.Fatalf("unable to change wallet's password: %v", err)
	}

	// The files should still exist since we didn't change the root key.
	for _, tempFile := range tempFiles {
		if _, err := os.Stat(tempFile); os.IsNotExist(err) {
			t.Fatal("file does not exist but it should")
		}
	}

	// The new password should be sent over the channel.
	select {
	case unlockMsg := <-service.UnlockMsgs:
		if !bytes.Equal(unlockMsg.Passphrase, newPassword) {
			t.Fatalf("expected to receive password %x, got %x",
				testPassword, unlockMsg.Passphrase)
		}

		// Close the macaroon DB and try to open it and read the root
		// key with the new password.
		store = openOrCreateTestMacStore(t, testDir, &newPassword)
		_, _, err = store.RootKey(context.Background())
		if err != nil {
			t.Fatalf("unable to read root key: %v", err)
		}

		err = store.Close()
		if err != nil {
			t.Fatalf("unable to close store: %v", err)
		}

	case <-time.After(30 * time.Second):
		t.Fatalf("password not received")
	}
}

// TestChangeWalletPasswordStateless checks that trying to change the password
// of an existing wallet that was initialized stateless works when when the
// --stateless_init flat is set. Also checks that if no password is given,
// the default password is used.
func TestChangeWalletPasswordStateless(t *testing.T) {
	ctx := context.Background()

	// testDir is empty, meaning wallet was not created from before.
	testDir, err := ioutil.TempDir("", "testchangepasswordstateless")
	if err != nil {
		t.Fatalf("unable to create temp directory: %v", err)
	}

	// Changing the password of the wallet will also try to change the
	// password of the macaroon DB. We create a default DB here but close it
	// immediately so the service does not fail when trying to open it.
	store := openOrCreateTestMacStore(
		t, testDir, &lnwallet.DefaultPrivatePassphrase,
	)
	err = store.Close()
	if err != nil {
		t.Fatalf("unable to close store: %v", err)
	}

	// Create a temp file that will act as the macaroon DB file that will
	// be deleted by changing the password.
	tmpFile, err := ioutil.TempFile(testDir, "")
	if err != nil {
		t.Fatalf("unable to create temp file: %v", err)
	}
	tempMacFile := tmpFile.Name()
	err = tmpFile.Close()
	if err != nil {
		t.Fatalf("unable to close file: %v", err)
	}

	// Create a file name that does not exist that will be used as a
	// macaroon file reference. The fact that the file does not exist should
	// not throw an error when --stateless_init is used.
	nonExistingFile := path.Join(testDir, "does-not-exist")

	// Create a new UnlockerService with our temp files.
	service := walletunlocker.New(
		testDir, testNetParams, true, testDir,
		[]string{tempMacFile, nonExistingFile},
	)

	// Create a wallet we can try to unlock. We use the default password
	// so we can check that the unlocker service defaults to this when
	// we give it an empty CurrentPassword to indicate we come from a
	// --noencryptwallet state.
	createTestWalletWithPw(
		t, lnwallet.DefaultPublicPassphrase,
		lnwallet.DefaultPrivatePassphrase, testDir, testNetParams,
	)

	// We make sure that we get a proper error message if we forget to
	// add the --stateless_init flag but the macaroon files don't exist.
	badReq := &lnrpc.ChangePasswordRequest{
		NewPassword:        testPassword,
		NewMacaroonRootKey: true,
	}
	_, err = service.ChangePassword(ctx, badReq)
	if err == nil {
		t.Fatalf("expected call to ChangePassword to fail")
	}

	// Prepare the correct request we are going to send to the unlocker
	// service. We don't provide a current password to indicate there
	// was none set before.
	req := &lnrpc.ChangePasswordRequest{
		NewPassword:        testPassword,
		StatelessInit:      true,
		NewMacaroonRootKey: true,
	}

	// Since we indicated the wallet was initialized stateless, the service
	// will block until it receives the macaroon through the channel
	// provided in the message in UnlockMsgs. So we need to call the service
	// async and then wait for the unlock message to arrive so we can send
	// back a fake macaroon.
	fakeMac := []byte("fakemacaroon")
	errChan := make(chan error, 1)
	go func() {
		// When providing the correct wallet's current password and a
		// new password that meets the length requirement, the password
		// change should succeed.
		response, err := service.ChangePassword(ctx, req)
		if err != nil {
			errChan <- err
			return
		}

		if !bytes.Equal(response.AdminMacaroon, fakeMac) {
			errChan <- fmt.Errorf("mismatched macaroon: expected "+
				"%x, got %x", fakeMac, response.AdminMacaroon)
		}

		// Close the macaroon DB and try to open it and read the root
		// key with the new password.
		store = openOrCreateTestMacStore(t, testDir, &testPassword)
		_, _, err = store.RootKey(context.Background())
		if err != nil {
			errChan <- err
			return
		}

		// Do cleanup now. Since we are in a go func, the defer at the
		// top of the outer would not work, because it would delete
		// the directory before we could check the content in here.
		err = store.Close()
		if err != nil {
			errChan <- err
			return
		}
		err = os.RemoveAll(testDir)
		if err != nil {
			errChan <- err
			return
		}
	}()

	// Password and recovery window should be sent over the channel.
	select {
	case err := <-errChan:
		t.Fatalf("ChangePassword call failed: %v", err)

	case unlockMsg := <-service.UnlockMsgs:
		if !bytes.Equal(unlockMsg.Passphrase, testPassword) {
			t.Fatalf("expected to receive password %x, got %x",
				testPassword, unlockMsg.Passphrase)
		}

		// Send a fake macaroon that should be returned in the response
		// in the async code above.
		unlockMsg.MacResponseChannel <- fakeMac

	case <-time.After(80 * time.Second):
		_ = os.RemoveAll(testDir)
		t.Fatalf("password not received")
	}
}
