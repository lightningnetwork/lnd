// Based on: https://github.com/btcsuite/btcwallet/blob/master/walletsetup.go
/*
* Copyright (c) 2014-2015 The btcsuite developers
*
* Permission to use, copy, modify, and distribute this software for any
* purpose with or without fee is hereby granted, provided that the above
* copyright notice and this permission notice appear in all copies.
*
* THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES
* WITH REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF
* MERCHANTABILITY AND FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR
* ANY SPECIAL, DIRECT, INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES
* WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS, WHETHER IN AN
* ACTION OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS ACTION, ARISING OUT OF
* OR IN CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE.
 */

package lnwallet

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh/terminal"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/btcsuite/btcutil/hdkeychain"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/btcsuite/btcwallet/wallet"
	"github.com/btcsuite/btcwallet/walletdb"
	_ "github.com/btcsuite/btcwallet/walletdb/bdb"
)

var (
	// TODO(roasbeef): lnwallet config file
	lnwalletHomeDir    = btcutil.AppDataDir("lnwallet", false)
	defaultDataDir     = lnwalletHomeDir
	defaultLogFilename = "lnwallet.log"

	defaultLogDirname = "logs"

	// defaultPubPassphrase is the default public wallet passphrase which is
	// used when the user indicates they do not want additional protection
	// provided by having all public data in the wallet encrypted by a
	// passphrase only known to them.
	defaultPubPassphrase = []byte("public")

	defaultLogDir = filepath.Join(lnwalletHomeDir, defaultLogDirname)

	walletDbName = "lnwallet.db"
)

// filesExists reports whether the named file or directory exists.
func fileExists(name string) bool {
	if _, err := os.Stat(name); err != nil {
		if os.IsNotExist(err) {
			return false
		}
	}
	return true
}

// networkDir returns the directory name of a network directory to hold wallet
// files.
func networkDir(dataDir string, chainParams *chaincfg.Params) string {
	netname := chainParams.Name

	// For now, we must always name the testnet data directory as "testnet"
	// and not "testnet3" or any other version, as the chaincfg testnet3
	// paramaters will likely be switched to being named "testnet3" in the
	// future.  This is done to future proof that change, and an upgrade
	// plan to move the testnet3 data directory can be worked out later.
	if chainParams.Net == wire.TestNet3 {
		netname = "testnet"
	}

	return filepath.Join(dataDir, netname)
}

// checkCreateDir checks that the path exists and is a directory.
// If path does not exist, it is created.
func checkCreateDir(path string) error {
	if fi, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			// Attempt data directory creation
			if err = os.MkdirAll(path, 0700); err != nil {
				return fmt.Errorf("cannot create directory: %s", err)
			}
		} else {
			return fmt.Errorf("error checking directory: %s", err)
		}
	} else {
		if !fi.IsDir() {
			return fmt.Errorf("path '%s' is not a directory", path)
		}
	}

	return nil
}

// createWallet generates a new wallet. The new wallet will reside at the
// provided path.
// TODO(roasbeef): maybe pass in config after all for testing purposes?
func createWallet(privPass, pubPass, userSeed []byte,
	dbPath string) error {
	// TODO(roasbeef): replace with tadge's seed format?
	hdSeed := userSeed
	var seedErr error
	if userSeed == nil {
		hdSeed, seedErr = hdkeychain.GenerateSeed(hdkeychain.RecommendedSeedLen)
		if seedErr != nil {
			return seedErr
		}
	}

	// Create the wallet.
	fmt.Println("Creating the wallet...")

	// Create the wallet database backed by bolt db.
	db, err := walletdb.Create("bdb", dbPath)
	if err != nil {
		return err
	}

	// Create the address manager.
	namespace, err := db.Namespace(waddrmgrNamespaceKey)
	if err != nil {
		return err
	}
	manager, err := waddrmgr.Create(namespace, hdSeed, []byte(pubPass),
		[]byte(privPass), ActiveNetParams, nil)
	if err != nil {
		return err
	}

	manager.Close()
	fmt.Println("The lnwallet has been created successfully.")
	return nil
}

// openDb opens and returns a walletdb.DB (boltdb here) given the directory and
// dbname
func openDb(directory string, dbname string) (walletdb.DB, error) {
	dbPath := filepath.Join(directory, dbname)

	// Ensure that the network directory exists.
	if err := checkCreateDir(directory); err != nil {
		return nil, err
	}

	// Open the database using the boltdb backend.
	return walletdb.Open("bdb", dbPath)
}

// promptSeed is used to prompt for the wallet seed which maybe required during
// upgrades.
func promptSeed() ([]byte, error) {
	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Print("Enter existing wallet seed: ")
		seedStr, err := reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		seedStr = strings.TrimSpace(strings.ToLower(seedStr))

		seed, err := hex.DecodeString(seedStr)
		if err != nil || len(seed) < hdkeychain.MinSeedBytes ||
			len(seed) > hdkeychain.MaxSeedBytes {

			fmt.Printf("Invalid seed specified.  Must be a "+
				"hexadecimal value that is at least %d bits and "+
				"at most %d bits\n", hdkeychain.MinSeedBytes*8,
				hdkeychain.MaxSeedBytes*8)
			continue
		}

		return seed, nil
	}
}

// promptPrivPassPhrase is used to prompt for the private passphrase which maybe
// required during upgrades.
func promptPrivPassPhrase() ([]byte, error) {
	prompt := "Enter the private passphrase of your wallet: "
	for {
		fmt.Print(prompt)
		pass, err := terminal.ReadPassword(int(os.Stdin.Fd()))
		if err != nil {
			return nil, err
		}
		fmt.Print("\n")
		pass = bytes.TrimSpace(pass)
		if len(pass) == 0 {
			continue
		}

		return pass, nil
	}
}

// openWallet returns a wallet. The function handles opening an existing wallet
// database, the address manager and the transaction store and uses the values
// to open a wallet.Wallet
func openWallet(pubPass []byte, dbDir string) (*wallet.Wallet, walletdb.DB, error) {
	db, err := openDb(dbDir, walletDbName)
	if err != nil {
		return nil, nil, fmt.Errorf("Failed to open database: %v", err)
	}

	addrMgrNS, err := db.Namespace(waddrmgrNamespaceKey)
	if err != nil {
		return nil, nil, err
	}
	txMgrNS, err := db.Namespace(wtxmgrNamespaceKey)
	if err != nil {
		return nil, nil, err
	}

	// TODO(roasbeef): pass these in as funcs instead, priv pass already
	// loaded into memory, use tadge's format to read HD seed.
	cbs := &waddrmgr.OpenCallbacks{
		ObtainSeed:        promptSeed,
		ObtainPrivatePass: promptPrivPassPhrase,
	}
	w, err := wallet.Open(pubPass, ActiveNetParams, db, addrMgrNS, txMgrNS,
		cbs)
	return w, db, err
}
