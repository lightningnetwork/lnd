package walletunlocker_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcwallet/snacl"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/btcsuite/btcwallet/wallet"
	"github.com/lightningnetwork/lnd/aezeed"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/btcwallet"
	"github.com/lightningnetwork/lnd/macaroons"
	"github.com/lightningnetwork/lnd/walletunlocker"
	"github.com/stretchr/testify/require"
)

var (
	testPassword = []byte("test-password")
	testSeed     = []byte("test-seed-123456789")
	testMac      = []byte("fakemacaroon")

	testEntropy = [aezeed.EntropySize]byte{
		0x81, 0xb6, 0x37, 0xd8,
		0x63, 0x59, 0xe6, 0x96,
		0x0d, 0xe7, 0x95, 0xe4,
		0x1e, 0x0b, 0x4c, 0xfd,
	}

	testNetParams = &chaincfg.MainNetParams

	testRecoveryWindow uint32 = 150

	defaultTestTimeout = 30 * time.Second

	defaultRootKeyIDContext = macaroons.ContextWithRootKeyID(
		context.Background(), macaroons.DefaultRootKeyID,
	)
)

func testLoaderOpts(testDir string) []btcwallet.LoaderOption {
	dbDir := btcwallet.NetworkDir(testDir, testNetParams)
	return []btcwallet.LoaderOption{
		btcwallet.LoaderWithLocalWalletDB(dbDir, true, time.Minute),
	}
}

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
	loader := wallet.NewLoader(
		netParams, netDir, true, kvdb.DefaultDBTimeout, 0,
	)
	_, err := loader.CreateNewWallet(
		pubPw, privPw, testSeed, time.Time{},
	)
	require.NoError(t, err)
	err = loader.UnloadWallet()
	require.NoError(t, err)
}

func createSeedAndMnemonic(t *testing.T,
	pass []byte) (*aezeed.CipherSeed, aezeed.Mnemonic) {

	cipherSeed, err := aezeed.New(
		keychain.CurrentKeyDerivationVersion, &testEntropy, time.Now(),
	)
	require.NoError(t, err)

	// With the new seed created, we'll convert it into a mnemonic phrase
	// that we'll send over to initialize the wallet.
	mnemonic, err := cipherSeed.ToMnemonic(pass)
	require.NoError(t, err)
	return cipherSeed, mnemonic
}

// openOrCreateTestMacStore opens or creates a bbolt DB and then initializes a
// root key storage for that DB and then unlocks it, creating a root key in the
// process.
func openOrCreateTestMacStore(tempDir string, pw *[]byte,
	netParams *chaincfg.Params) (*macaroons.RootKeyStorage, error) {

	netDir := btcwallet.NetworkDir(tempDir, netParams)
	err := os.MkdirAll(netDir, 0700)
	if err != nil {
		return nil, err
	}
	db, err := kvdb.Create(
		kvdb.BoltBackendName, path.Join(netDir, "macaroons.db"),
		true, kvdb.DefaultDBTimeout, false,
	)
	if err != nil {
		return nil, err
	}

	store, err := macaroons.NewRootKeyStorage(db)
	if err != nil {
		_ = db.Close()
		return nil, err
	}

	err = store.CreateUnlock(pw)
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	_, _, err = store.RootKey(defaultRootKeyIDContext)
	if err != nil {
		_ = store.Close()
		return nil, err
	}

	return store, nil
}

// TestGenSeed tests that the gen seed method generates a valid
// cipher seed mnemonic phrase and user provided source of entropy.
func TestGenSeed(t *testing.T) {
	t.Parallel()

	// First, we'll create a new test directory and unlocker service for
	// that directory.
	testDir := t.TempDir()

	service := walletunlocker.New(
		testNetParams, nil, false, testLoaderOpts(testDir),
	)

	// Now that the service has been created, we'll ask it to generate a
	// new seed for us given a test passphrase.
	aezeedPass := []byte("kek")
	genSeedReq := &lnrpc.GenSeedRequest{
		AezeedPassphrase: aezeedPass,
		SeedEntropy:      testEntropy[:],
	}

	ctx := t.Context()
	seedResp, err := service.GenSeed(ctx, genSeedReq)
	require.NoError(t, err)

	// We should then be able to take the generated mnemonic, and properly
	// decipher both it.
	var mnemonic aezeed.Mnemonic
	copy(mnemonic[:], seedResp.CipherSeedMnemonic[:])
	_, err = mnemonic.ToCipherSeed(aezeedPass)
	require.NoError(t, err)
}

// TestGenSeedGenerateEntropy tests that the gen seed method generates a valid
// cipher seed mnemonic passphrase even when the user doesn't provide its own
// source of entropy.
func TestGenSeedGenerateEntropy(t *testing.T) {
	t.Parallel()

	// First, we'll create a new test directory and unlocker service for
	// that directory.
	testDir := t.TempDir()
	service := walletunlocker.New(
		testNetParams, nil, false, testLoaderOpts(testDir),
	)

	// Now that the service has been created, we'll ask it to generate a
	// new seed for us given a test passphrase. Note that we don't actually
	aezeedPass := []byte("kek")
	genSeedReq := &lnrpc.GenSeedRequest{
		AezeedPassphrase: aezeedPass,
	}

	ctx := t.Context()
	seedResp, err := service.GenSeed(ctx, genSeedReq)
	require.NoError(t, err)

	// We should then be able to take the generated mnemonic, and properly
	// decipher both it.
	var mnemonic aezeed.Mnemonic
	copy(mnemonic[:], seedResp.CipherSeedMnemonic[:])
	_, err = mnemonic.ToCipherSeed(aezeedPass)
	require.NoError(t, err)
}

// TestGenSeedInvalidEntropy tests that if a user attempt to create a seed with
// the wrong number of bytes for the initial entropy, then the proper error is
// returned.
func TestGenSeedInvalidEntropy(t *testing.T) {
	t.Parallel()

	// First, we'll create a new test directory and unlocker service for
	// that directory.
	testDir := t.TempDir()
	service := walletunlocker.New(
		testNetParams, nil, false, testLoaderOpts(testDir),
	)

	// Now that the service has been created, we'll ask it to generate a
	// new seed for us given a test passphrase. However, we'll be using an
	// invalid set of entropy that's 55 bytes, instead of 15 bytes.
	aezeedPass := []byte("kek")
	genSeedReq := &lnrpc.GenSeedRequest{
		AezeedPassphrase: aezeedPass,
		SeedEntropy:      bytes.Repeat([]byte("a"), 55),
	}

	// We should get an error now since the entropy source was invalid.
	ctx := t.Context()
	_, err := service.GenSeed(ctx, genSeedReq)
	require.Error(t, err)
	require.Contains(t, err.Error(), "incorrect entropy length")
}

// TestInitWallet tests that the user is able to properly initialize the wallet
// given an existing cipher seed passphrase.
func TestInitWallet(t *testing.T) {
	t.Parallel()

	// testDir is empty, meaning wallet was not created from before.
	testDir := t.TempDir()

	// Create new UnlockerService.
	service := walletunlocker.New(
		testNetParams, nil, false, testLoaderOpts(testDir),
	)

	// Once we have the unlocker service created, we'll now instantiate a
	// new cipher seed and its mnemonic.
	pass := []byte("test")
	cipherSeed, mnemonic := createSeedAndMnemonic(t, pass)

	// Now that we have all the necessary items, we'll now issue the Init
	// command to the wallet. This should check the validity of the cipher
	// seed, then send over the initialization information over the init
	// channel.
	ctx := t.Context()
	req := &lnrpc.InitWalletRequest{
		WalletPassword:     testPassword,
		CipherSeedMnemonic: mnemonic[:],
		AezeedPassphrase:   pass,
		RecoveryWindow:     int32(testRecoveryWindow),
		StatelessInit:      true,
	}
	errChan := make(chan error, 1)
	go func() {
		response, err := service.InitWallet(ctx, req)
		if err != nil {
			errChan <- err
			return
		}

		if !bytes.Equal(response.AdminMacaroon, testMac) {
			errChan <- fmt.Errorf("mismatched macaroon: "+
				"expected %x, got %x", testMac,
				response.AdminMacaroon)
		}
	}()

	// The same user passphrase, and also the plaintext cipher seed
	// should be sent over and match exactly.
	select {
	case err := <-errChan:
		t.Fatalf("InitWallet call failed: %v", err)

	case msg := <-service.InitMsgs:
		msgSeed := msg.WalletSeed
		require.Equal(t, testPassword, msg.Passphrase)
		require.Equal(
			t, cipherSeed.InternalVersion, msgSeed.InternalVersion,
		)
		require.Equal(t, cipherSeed.Birthday, msgSeed.Birthday)
		require.Equal(t, cipherSeed.Entropy, msgSeed.Entropy)
		require.Equal(t, testRecoveryWindow, msg.RecoveryWindow)
		require.Equal(t, true, msg.StatelessInit)

		// Send a fake macaroon that should be returned in the response
		// in the async code above.
		service.MacResponseChan <- testMac

	case <-time.After(defaultTestTimeout):
		t.Fatalf("password not received")
	}

	// Create a wallet in testDir.
	createTestWallet(t, testDir, testNetParams)

	// Now calling InitWallet should fail, since a wallet already exists in
	// the directory.
	_, err := service.InitWallet(ctx, req)
	require.Error(t, err)

	// Similarly, if we try to do GenSeed again, we should get an error as
	// the wallet already exists.
	_, err = service.GenSeed(ctx, &lnrpc.GenSeedRequest{})
	require.Error(t, err)
}

// TestCreateWalletInvalidEntropy tests that if we attempt to create a wallet
// with an invalid cipher seed, then we'll receive an error.
func TestCreateWalletInvalidEntropy(t *testing.T) {
	t.Parallel()

	// testDir is empty, meaning wallet was not created from before.
	testDir := t.TempDir()

	// Create new UnlockerService.
	service := walletunlocker.New(
		testNetParams, nil, false, testLoaderOpts(testDir),
	)

	// We'll attempt to init the wallet with an invalid cipher seed and
	// passphrase.
	req := &lnrpc.InitWalletRequest{
		WalletPassword:     testPassword,
		CipherSeedMnemonic: []string{"invalid", "seed"},
		AezeedPassphrase:   []byte("fake pass"),
	}

	ctx := t.Context()
	_, err := service.InitWallet(ctx, req)
	require.Error(t, err)
}

// TestUnlockWallet checks that trying to unlock non-existing wallet fails, that
// unlocking existing wallet with wrong passphrase fails, and that unlocking
// existing wallet with correct passphrase succeeds.
func TestUnlockWallet(t *testing.T) {
	t.Parallel()

	// testDir is empty, meaning wallet was not created from before.
	testDir := t.TempDir()

	// Create new UnlockerService that'll also drop the wallet's history on
	// unlock.
	service := walletunlocker.New(
		testNetParams, nil, true, testLoaderOpts(testDir),
	)

	ctx := t.Context()
	req := &lnrpc.UnlockWalletRequest{
		WalletPassword: testPassword,
		RecoveryWindow: int32(testRecoveryWindow),
		StatelessInit:  true,
	}

	// Should fail to unlock non-existing wallet.
	_, err := service.UnlockWallet(ctx, req)
	require.Error(t, err)

	// Create a wallet we can try to unlock.
	createTestWallet(t, testDir, testNetParams)

	// Try unlocking this wallet with the wrong passphrase.
	wrongReq := &lnrpc.UnlockWalletRequest{
		WalletPassword: []byte("wrong-ofc"),
	}
	_, err = service.UnlockWallet(ctx, wrongReq)
	require.Error(t, err)

	// With the correct password, we should be able to unlock the wallet.
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
		require.Equal(t, testPassword, unlockMsg.Passphrase)
		require.Equal(t, testRecoveryWindow, unlockMsg.RecoveryWindow)
		require.Equal(t, true, unlockMsg.StatelessInit)

		// Send a fake macaroon that should be returned in the response
		// in the async code above.
		service.MacResponseChan <- testMac
		require.NoError(t, unlockMsg.UnloadWallet())

	case <-time.After(defaultTestTimeout):
		t.Fatalf("password not received")
	}
}

// TestChangeWalletPasswordNewRootKey tests that we can successfully change the
// wallet's password needed to unlock it and rotate the root key for the
// macaroons in the same process.
func TestChangeWalletPasswordNewRootKey(t *testing.T) {
	t.Parallel()

	// testDir is empty, meaning wallet was not created from before.
	testDir := t.TempDir()

	// Changing the password of the wallet will also try to change the
	// password of the macaroon DB. We create a default DB here but close it
	// immediately so the service does not fail when trying to open it.
	store, err := openOrCreateTestMacStore(
		testDir, &testPassword, testNetParams,
	)
	require.NoError(t, err)
	require.NoError(t, store.Close())

	// Create some files that will act as macaroon files that should be
	// deleted after a password change is successful with a new root key
	// requested.
	var tempFiles []string
	for i := 0; i < 3; i++ {
		file, err := os.CreateTemp(testDir, "")
		if err != nil {
			t.Fatalf("unable to create temp file: %v", err)
		}
		tempFiles = append(tempFiles, file.Name())
		require.NoError(t, file.Close())
	}

	// Create a new UnlockerService with our temp files.
	service := walletunlocker.New(
		testNetParams, tempFiles, false, testLoaderOpts(testDir),
	)
	service.SetMacaroonDB(store.Backend)

	ctx := t.Context()
	newPassword := []byte("hunter2???")

	req := &lnrpc.ChangePasswordRequest{
		CurrentPassword:    testPassword,
		NewPassword:        newPassword,
		NewMacaroonRootKey: true,
	}

	// Changing the password to a non-existing wallet should fail.
	_, err = service.ChangePassword(ctx, req)
	require.Error(t, err)

	// Create a wallet to test changing the password.
	createTestWallet(t, testDir, testNetParams)

	// Attempting to change the wallet's password using an incorrect
	// current password should fail.
	wrongReq := &lnrpc.ChangePasswordRequest{
		CurrentPassword: []byte("wrong-ofc"),
		NewPassword:     newPassword,
	}
	_, err = service.ChangePassword(ctx, wrongReq)
	require.Error(t, err)

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
	require.Error(t, err)

	// When providing the correct wallet's current password and a new
	// password that meets the length requirement, the password change
	// should succeed.
	errChan := make(chan error, 1)
	go doChangePassword(service, req, errChan)

	// The new password should be sent over the channel.
	select {
	case err := <-errChan:
		t.Fatalf("ChangePassword call failed: %v", err)

	case unlockMsg := <-service.UnlockMsgs:
		require.Equal(t, newPassword, unlockMsg.Passphrase)

		// Send a fake macaroon that should be returned in the response
		// in the async code above.
		service.MacResponseChan <- testMac
		require.NoError(t, unlockMsg.UnloadWallet())

	case <-time.After(defaultTestTimeout):
		t.Fatalf("password not received")
	}

	// Wait for the doChangePassword goroutine to finish.
	select {
	case err := <-errChan:
		require.NoError(t, err, "ChangePassword call failed")

	case <-time.After(defaultTestTimeout):
		t.Fatalf("ChangePassword timed out")
	}

	// The files should no longer exist.
	for _, tempFile := range tempFiles {
		f, err := os.Open(tempFile)
		if err == nil {
			_ = f.Close()
			t.Fatal("file exists but it shouldn't")
		}
	}

	// Close the old db first.
	require.NoError(t, store.Backend.Close())

	// Check that the new password can be used to open the db.
	assertPasswordChanged(t, testDir, req.NewPassword)
}

// TestChangeWalletPasswordStateless checks that trying to change the password
// of an existing wallet that was initialized stateless works when the
// --stateless_init flag is set. Also checks that if no password is given,
// the default password is used.
func TestChangeWalletPasswordStateless(t *testing.T) {
	t.Parallel()

	// testDir is empty, meaning wallet was not created from before.
	testDir := t.TempDir()

	// Changing the password of the wallet will also try to change the
	// password of the macaroon DB. We create a default DB here but close it
	// immediately so the service does not fail when trying to open it.
	store, err := openOrCreateTestMacStore(
		testDir, &lnwallet.DefaultPrivatePassphrase, testNetParams,
	)
	require.NoError(t, err)
	require.NoError(t, store.Close())

	// Create a temp file that will act as the macaroon DB file that will
	// be deleted by changing the password.
	tmpFile, err := os.CreateTemp(testDir, "")
	require.NoError(t, err)
	tempMacFile := tmpFile.Name()
	err = tmpFile.Close()
	require.NoError(t, err)

	// Create a file name that does not exist that will be used as a
	// macaroon file reference. The fact that the file does not exist should
	// not throw an error when --stateless_init is used.
	nonExistingFile := path.Join(testDir, "does-not-exist")

	// Create a new UnlockerService with our temp files.
	service := walletunlocker.New(
		testNetParams, []string{
			tempMacFile, nonExistingFile,
		}, false, testLoaderOpts(testDir),
	)
	service.SetMacaroonDB(store.Backend)

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
	ctx := t.Context()
	_, err = service.ChangePassword(ctx, badReq)
	require.Error(t, err)

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
	errChan := make(chan error, 1)
	go doChangePassword(service, req, errChan)

	// Password and recovery window should be sent over the channel.
	select {
	case err := <-errChan:
		t.Fatalf("ChangePassword call failed: %v", err)

	case unlockMsg := <-service.UnlockMsgs:
		require.Equal(t, testPassword, unlockMsg.Passphrase)

		// Send a fake macaroon that should be returned in the response
		// in the async code above.
		service.MacResponseChan <- testMac
		require.NoError(t, unlockMsg.UnloadWallet())

	case <-time.After(defaultTestTimeout):
		t.Fatalf("password not received")
	}

	// Wait for the doChangePassword goroutine to finish.
	select {
	case err := <-errChan:
		require.NoError(t, err, "ChangePassword call failed")

	case <-time.After(defaultTestTimeout):
		t.Fatalf("ChangePassword timed out")
	}

	// Close the old db first.
	require.NoError(t, store.Backend.Close())

	// Check that the new password can be used to open the db.
	assertPasswordChanged(t, testDir, req.NewPassword)
}

func doChangePassword(service *walletunlocker.UnlockerService,
	req *lnrpc.ChangePasswordRequest, errChan chan error) {

	// When providing the correct wallet's current password and a new
	// password that meets the length requirement, the password change
	// should succeed.
	ctx := context.Background()
	response, err := service.ChangePassword(ctx, req)
	if err != nil {
		errChan <- fmt.Errorf("could not change password: %w", err)
		return
	}

	if !bytes.Equal(response.AdminMacaroon, testMac) {
		errChan <- fmt.Errorf("mismatched macaroon: expected %x, got "+
			"%x", testMac, response.AdminMacaroon)
		return
	}

	close(errChan)
}

// assertPasswordChanged asserts that the new password can be used to open the
// store.
func assertPasswordChanged(t *testing.T, testDir string, newPassword []byte) {
	// Open it and read the root key with the new password.
	store, err := openOrCreateTestMacStore(
		testDir, &newPassword, testNetParams,
	)
	require.NoError(t, err)

	// Assert that we can read the root key.
	_, _, err = store.RootKey(defaultRootKeyIDContext)
	require.NoError(t, err)

	// Close the db once done.
	require.NoError(t, store.Close())
	require.NoError(t, store.Backend.Close())
}
