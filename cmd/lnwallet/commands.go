// Copyright (c) 2013-2017 The btcsuite developers
// Copyright (c) 2015-2016 The Decred developers
// Copyright (C) 2015-2018 The Lightning Network Developers

package main

import (
	"encoding/hex"
	"fmt"
	"github.com/btcsuite/btcwallet/snacl"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/btcsuite/btcwallet/wallet"
	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/urfave/cli"

	// This is required to register bdb as a valid walletdb driver. In the
	// init function of the package, it registers itself. The import is used
	// to activate the side effects w/o actually binding the package name to
	// a file-level variable.
	_ "github.com/btcsuite/btcwallet/walletdb/bdb"
)

var (
	// Namespace from github.com/btcsuite/btcwallet/wallet/wallet.go
	waddrmgrNamespaceKey = []byte("waddrmgr")

	// Bucket names from github.com/btcsuite/btcwallet/waddrmgr/db.go
	mainBucketName    = []byte("main")
	masterPrivKeyName = []byte("mpriv")
	cryptoPrivKeyName = []byte("cpriv")
	masterHDPrivName  = []byte("mhdpriv")

	defaultAccount  = uint32(waddrmgr.DefaultAccountNum)
	walletFile      string
	publicWalletPw  = lnwallet.DefaultPublicPassphrase
	privateWalletPw = lnwallet.DefaultPrivatePassphrase
	openCallbacks   = &waddrmgr.OpenCallbacks{
		ObtainSeed:        noConsole,
		ObtainPrivatePass: noConsole,
	}
)

func openWalletDbFile(ctx *cli.Context) (walletdb.DB, error) {
	args := ctx.Args()

	// Parse and clean up wallet file parameter.
	switch {
	case ctx.IsSet("wallet_file"):
		walletFile = ctx.String("wallet_file")
	case args.Present():
		walletFile = args.First()
		args = args.Tail()
	default:
		return nil, fmt.Errorf("Wallet-file argument missing")
	}
	walletFile = cleanAndExpandPath(walletFile)

	// Ask the user for the wallet password. If it's empty, the default
	// password will be used, since the lnd wallet is always encrypted.
	pw := readPassword(ctx, "Input wallet password: ")
	if len(pw) > 0 {
		publicWalletPw = pw
		privateWalletPw = pw
	}

	// Try to load and open the wallet.
	db, err := walletdb.Open("bdb", walletFile)
	if err != nil {
		return nil, fmt.Errorf("Failed to open database: %v", err)
	}
	return db, nil
}

func openAndUnlockWallet(ctx *cli.Context) (walletdb.DB, *wallet.Wallet, func(),
	error) {

	// openWalletDbFile also reads the passwords and sets them globally so
	// we can use them later.
	db, err := openWalletDbFile(ctx)
	if err != nil {
		closeWalletDb(db)
		return nil, nil, nil, err
	}

	netParams, _ := getNetParams(ctx)
	w, err := wallet.Open(
		db, publicWalletPw, openCallbacks, netParams, 0,
	)
	if err != nil {
		closeWalletDb(db)
		return nil, nil, nil, err
	}

	w.Start()
	cleanup := func() {
		w.Stop()
		closeWalletDb(db)
	}

	err = w.Unlock(privateWalletPw, nil)
	if err != nil {
		cleanup()
		return nil, nil, nil, err
	}
	return db, w, cleanup, nil
}

func closeWalletDb(db walletdb.DB) {
	err := db.Close()
	if err != nil {
		fmt.Printf("Error closing database: %v", err)
	}
}

func decryptRootKey(db walletdb.DB, privPassphrase []byte) ([]byte, error) {
	// Step 1: Load the encryption parameters and encrypted keys from the
	// database.
	var masterKeyPrivParams []byte
	var cryptoKeyPrivEnc []byte
	var masterHDPrivEnc []byte
	err := walletdb.View(db, func(tx walletdb.ReadTx) error {
		ns := tx.ReadBucket(waddrmgrNamespaceKey)
		if ns == nil {
			return fmt.Errorf(
				"namespace '%s' does not exist",
				waddrmgrNamespaceKey,
			)
		}

		mainBucket := ns.NestedReadBucket(mainBucketName)
		if mainBucket == nil {
			return fmt.Errorf(
				"bucket '%s' does not exist",
				mainBucketName,
			)
		}

		val := mainBucket.Get(masterPrivKeyName)
		if val != nil {
			masterKeyPrivParams = make([]byte, len(val))
			copy(masterKeyPrivParams, val)
		}
		val = mainBucket.Get(cryptoPrivKeyName)
		if val != nil {
			cryptoKeyPrivEnc = make([]byte, len(val))
			copy(cryptoKeyPrivEnc, val)
		}
		val = mainBucket.Get(masterHDPrivName)
		if val != nil {
			masterHDPrivEnc = make([]byte, len(val))
			copy(masterHDPrivEnc, val)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Step 2: Unmarshal the master private key parameters and derive
	// key from passphrase.
	var masterKeyPriv snacl.SecretKey
	if err := masterKeyPriv.Unmarshal(masterKeyPrivParams); err != nil {
		return nil, err
	}
	if err := masterKeyPriv.DeriveKey(&privPassphrase); err != nil {
		return nil, err
	}

	// Step 3: Decrypt the keys in the correct order.
	cryptoKeyPriv := &snacl.CryptoKey{}
	cryptoKeyPrivBytes, err := masterKeyPriv.Decrypt(cryptoKeyPrivEnc)
	if err != nil {
		return nil, err
	}
	copy(cryptoKeyPriv[:], cryptoKeyPrivBytes)
	return cryptoKeyPriv.Decrypt(masterHDPrivEnc)
}

var walletInfoCommand = cli.Command{
	Name:      "walletinfo",
	Usage:     "Show all relevant info of a lnd wallet.db file.",
	ArgsUsage: "wallet-file",
	Description: `
	Show information about the specified lnd wallet.
	Information includes the public key, number of addresses used and if
	--with_root_key is set, the BIP32 extended root key.`,
	Flags: []cli.Flag{
		cli.StringFlag{
			Name:  "wallet_file",
			Usage: "the path to the wallet.db file",
		},
		cli.BoolFlag{
			Name:  "with_root_key",
			Usage: "also show the BIP32 extended root key",
		},
	},
	Action: walletInfo,
}

func walletInfo(ctx *cli.Context) error {
	// Show command help if no arguments were provided.
	if ctx.NArg() == 0 && ctx.NumFlags() == 0 {
		return cli.ShowCommandHelp(ctx, "walletinfo")
	}

	db, w, cleanup, err := openAndUnlockWallet(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	// Derive the identity public key.
	_, coinType := getNetParams(ctx)
	keyRing := keychain.NewBtcWalletKeyRing(w, coinType)
	idPrivKey, err := keyRing.DerivePrivKey(keychain.KeyDescriptor{
		KeyLocator: keychain.KeyLocator{
			Family: keychain.KeyFamilyNodeKey,
			Index:  0,
		},
	})
	if err != nil {
		return err
	}
	idPrivKey.Curve = btcec.S256()
	fmt.Printf(
		"Identity Pubkey: %s\n",
		hex.EncodeToString(idPrivKey.PubKey().SerializeCompressed()),
	)

	// Print information about the different addresses in use.
	printScopeInfo(
		"np2wkh", w,
		w.Manager.ScopesForExternalAddrType(
			waddrmgr.NestedWitnessPubKey,
		),
	)
	printScopeInfo(
		"p2wkh", w,
		w.Manager.ScopesForExternalAddrType(
			waddrmgr.WitnessPubKey,
		),
	)

	// Decrypt HD master extended root key.
	if ctx.IsSet("with_root_key") {
		masterHDPrivKey, err := decryptRootKey(db, privateWalletPw)
		if err != nil {
			return err
		}
		fmt.Printf("BIP32 extended root key: %s\n", masterHDPrivKey)
	}
	return db.Close()
}

func printScopeInfo(name string, w *wallet.Wallet, scopes []waddrmgr.KeyScope) {
	for _, scope := range scopes {
		props, err := w.AccountProperties(scope, defaultAccount)
		if err != nil {
			fmt.Printf("Error fetching account properties: %v", err)
		}
		fmt.Printf("Scope: %s\n", scope.String())
		fmt.Printf(
			"  Number of internal (change) %s addresses: %d\n",
			name, props.InternalKeyCount,
		)
		fmt.Printf(
			"  Number of external %s addresses: %d\n", name,
			props.ExternalKeyCount,
		)
	}
}

var dumpWalletCommand = cli.Command{
	Name:      "dumpwallet",
	Usage:     "Dump the private keys of a lnd wallet.db file.",
	ArgsUsage: "wallet-file",
	Description: `
	Generate a bitcoind compatible dump of the lnd wallet.
	All used private keys and addresses are dumped as a text representation
	that can then be imported by bitcoind.

	ATTENTION: Obviously this only dumps keys/addresses with normal on-chain
	funds on them. Coins locked in channels will not be included!`,
	Flags: []cli.Flag{
		cli.StringFlag{
			Name:  "wallet_file",
			Usage: "the path to the wallet.db file",
		},
	},
	Action: dumpWallet,
}

func dumpWallet(ctx *cli.Context) error {
	// Show command help if no arguments were provided.
	if ctx.NArg() == 0 && ctx.NumFlags() == 0 {
		return cli.ShowCommandHelp(ctx, "dumpwallet")
	}

	db, w, cleanup, err := openAndUnlockWallet(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	// Now collect all the information we can about the default account,
	// get all addresses and their private key.
	amount, err := w.CalculateBalance(0)
	if err != nil {
		return err
	}
	block := w.Manager.SyncedTo()
	fmt.Printf("# Wallet dump created by lnwallet %s\n", ctx.App.Version)
	fmt.Printf("# * Created on %s\n", time.Now().UTC())
	fmt.Printf("# * Best block at time of backup was %d (%s),\n",
		block.Height, block.Hash.String())
	fmt.Printf("#   mined on %s", block.Timestamp.UTC())
	fmt.Printf("# * Total balance: %.8f\n\n", amount.ToBTC())

	addrs, err := w.AccountAddresses(defaultAccount)
	if err != nil {
		return err
	}
	var empty struct{}
	for _, addr := range addrs {
		privateKey, err := w.DumpWIFPrivateKey(addr)
		if err != nil {
			return fmt.Errorf("error getting address info: %v", err)
		}
		fmt.Printf(
			"%s 1970-01-01T00:00:01Z label= # addr=%s",
			privateKey, addr.EncodeAddress(),
		)
		list := make(map[string]struct{})
		list[addr.EncodeAddress()] = empty
		unspent, err := w.ListUnspent(0, 999999, list)
		if err != nil {
			return err
		}
		for _, u := range unspent {
			fmt.Printf(" unspent=%f", u.Amount)
		}
		fmt.Println()
	}

	w.Stop()
	return db.Close()
}
