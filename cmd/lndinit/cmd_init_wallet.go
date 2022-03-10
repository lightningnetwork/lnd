package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcwallet/wallet"
	"github.com/jessevdk/go-flags"
	"github.com/lightninglabs/protobuf-hex-display/jsonpb"
	"github.com/lightningnetwork/lnd/aezeed"
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

const (
	defaultNoFreelistSync = true

	defaultDirPermissions os.FileMode = 0700

	defaultBitcoinNetwork = "mainnet"

	typeFile = "file"
	typeRpc  = "rpc"
)

var (
	defaultWalletDBTimeout = 2 * time.Second
	defaultStartupTimeout  = 10 * time.Second
)

type secretSourceFile struct {
	Seed           string `long:"seed" description:"The full path to the file that contains the seed; if the file does not exist, lndinit will exit with code EXIT_CODE_INPUT_MISSING (129)"`
	SeedPassphrase string `long:"seed-passphrase" description:"The full path to the file that contains the seed passphrase; if not set, no passphrase will be used; if set but the file does not exist, lndinit will exit with code EXIT_CODE_INPUT_MISSING (129)"`
	WalletPassword string `long:"wallet-password" description:"The full path to the file that contains the wallet password; if the file does not exist, lndinit will exit with code EXIT_CODE_INPUT_MISSING (129)"`
}

type secretSourceK8s struct {
	Namespace               string `long:"namespace" description:"The Kubernetes namespace the secret is located in"`
	SecretName              string `long:"secret-name" description:"The name of the Kubernetes secret"`
	SeedEntryName           string `long:"seed-entry-name" description:"The name of the entry within the secret that contains the seed"`
	SeedPassphraseEntryName string `long:"seed-passphrase-entry-name" description:"The name of the entry within the secret that contains the seed passphrase"`
	WalletPasswordEntryName string `long:"wallet-password-entry-name" description:"The name of the entry within the secret that contains the wallet password"`
	Base64                  bool   `long:"base64" description:"Encode as base64 when storing and decode as base64 when reading"`
}

type initTypeFile struct {
	OutputWalletDir  string `long:"output-wallet-dir" description:"The directory in which the wallet.db file should be initialized"`
	ValidatePassword bool   `long:"validate-password" description:"If a wallet file already exists in the output wallet directory, validate that it can be unlocked with the given password; this will try to decrypt the wallet and will take several seconds to complete"`
}

type initTypeRpc struct {
	Server       string `long:"server" description:"The host:port of the RPC server to connect to"`
	TLSCertPath  string `long:"tls-cert-path" description:"The full path to the RPC server's TLS certificate"`
	WatchOnly    bool   `long:"watch-only" description:"Don't require a seed to be set, initialize the wallet as watch-only; requires the accounts-file flag to be specified"`
	AccountsFile string `long:"accounts-file" description:"The JSON file that contains all accounts xpubs for initializing a watch-only wallet"`
}

type initWalletCommand struct {
	Network      string            `long:"network" description:"The Bitcoin network to initialize the wallet for, required for wallet internals" choice:"mainnet" choice:"testnet" choice:"testnet3" choice:"regtest" choice:"simnet"`
	SecretSource string            `long:"secret-source" description:"Where to read the secrets from to initialize the wallet with" choice:"file" choice:"k8s"`
	File         *secretSourceFile `group:"Flags for reading the secrets from files (use when --secret-source=file)" namespace:"file"`
	K8s          *secretSourceK8s  `group:"Flags for reading the secrets from Kubernetes (use when --secret-source=k8s)" namespace:"k8s"`
	InitType     string            `long:"init-type" description:"How to initialize the wallet" choice:"file" choice:"rpc"`
	InitFile     *initTypeFile     `group:"Flags for initializing the wallet as a file (use when --init-type=file)" namespace:"init-file"`
	InitRpc      *initTypeRpc      `group:"Flags for initializing the wallet through RPC (use when --init-type=rpc)" namespace:"init-rpc"`
}

func newInitWalletCommand() *initWalletCommand {
	return &initWalletCommand{
		Network:      defaultBitcoinNetwork,
		SecretSource: storageFile,
		File:         &secretSourceFile{},
		K8s: &secretSourceK8s{
			Namespace: defaultK8sNamespace,
		},
		InitType: typeFile,
		InitFile: &initTypeFile{},
		InitRpc: &initTypeRpc{
			Server: defaultRPCServer,
		},
	}
}

func (x *initWalletCommand) Register(parser *flags.Parser) error {
	_, err := parser.AddCommand(
		"init-wallet",
		"Initialize an lnd wallet database",
		"Create an lnd wallet.db database file initialized with the "+
			"given wallet seed and password",
		x,
	)
	return err
}

func (x *initWalletCommand) Execute(_ []string) error {
	// Do we require a seed? We don't if we do an RPC based, watch-only
	// initialization.
	requireSeed := (x.InitType == typeFile) ||
		(x.InitType == typeRpc && !x.InitRpc.WatchOnly)

	// First find out where we want to read the secrets from.
	var (
		seed           string
		seedPassPhrase string
		walletPassword string
		err            error
	)
	switch x.SecretSource {
	// Read all secrets from individual files.
	case storageFile:
		if requireSeed {
			log("Reading seed from file")
			seed, err = readFile(x.File.Seed)
			if err != nil {
				return err
			}
		}

		// The seed passphrase is optional.
		if x.File.SeedPassphrase != "" {
			log("Reading seed passphrase from file")
			seedPassPhrase, err = readFile(x.File.SeedPassphrase)
			if err != nil {
				return err
			}
		}

		log("Reading wallet password from file")
		walletPassword, err = readFile(x.File.WalletPassword)
		if err != nil {
			return err
		}

	// Read passphrase from Kubernetes secret.
	case storageK8s:
		k8sSecret := &k8sSecretOptions{
			Namespace:  x.K8s.Namespace,
			SecretName: x.K8s.SecretName,
			Base64:     x.K8s.Base64,
		}
		k8sSecret.SecretEntryName = x.K8s.SeedEntryName

		if requireSeed {
			log("Reading seed from k8s secret %s (namespace %s)",
				x.K8s.SecretName, x.K8s.Namespace)
			seed, _, err = readK8s(k8sSecret)
			if err != nil {
				return err
			}
		}

		// The seed passphrase is optional.
		if x.K8s.SeedPassphraseEntryName != "" {
			log("Reading seed passphrase from k8s secret %s "+
				"(namespace %s)", x.K8s.SecretName,
				x.K8s.Namespace)
			k8sSecret.SecretEntryName = x.K8s.SeedPassphraseEntryName
			seedPassPhrase, _, err = readK8s(k8sSecret)
			if err != nil {
				return err
			}
		}

		log("Reading wallet password from k8s secret %s (namespace %s)",
			x.K8s.SecretName, x.K8s.Namespace)
		k8sSecret.SecretEntryName = x.K8s.WalletPasswordEntryName
		walletPassword, _, err = readK8s(k8sSecret)
		if err != nil {
			return err
		}

	}

	switch x.InitType {
	case typeFile:
		cipherSeed, err := checkSeed(seed, seedPassPhrase)
		if err != nil {
			return err
		}

		// The output directory must be specified explicitly. We don't
		// want to assume any defaults here!
		walletDir := lncfg.CleanAndExpandPath(
			x.InitFile.OutputWalletDir,
		)
		if walletDir == "" {
			return fmt.Errorf("must specify output wallet " +
				"directory")
		}
		if strings.HasSuffix(walletDir, ".db") {
			return fmt.Errorf("output wallet directory must not " +
				"be a file")
		}

		return createWalletFile(
			cipherSeed, walletPassword, walletDir, x.Network,
			x.InitFile.ValidatePassword,
		)

	case typeRpc:
		var (
			seedWords []string
			watchOnly *lnrpc.WatchOnly
		)

		if requireSeed {
			_, err = checkSeed(seed, seedPassPhrase)
			if err != nil {
				return err
			}
			seedWords = strings.Split(seed, " ")
		}

		// Only when initializing the wallet through RPC is it possible
		// to create a watch-only wallet. If we do, we don't require a
		// seed to be present but instead want to read an accounts JSON
		// file that contains all the wallet's xpubs.
		if x.InitRpc.WatchOnly {
			// For initializing a watch-only wallet we need the
			// accounts JSON file.
			log("Reading accounts from file")
			accountsBytes, err := readFile(x.InitRpc.AccountsFile)
			if err != nil {
				return err
			}

			jsonAccts := &walletrpc.ListAccountsResponse{}
			err = jsonpb.Unmarshal(
				strings.NewReader(accountsBytes), jsonAccts,
			)
			if err != nil {
				return fmt.Errorf("error parsing JSON: %v", err)
			}
			if len(jsonAccts.Accounts) == 0 {
				return fmt.Errorf("cannot import empty " +
					"account list")
			}

			rpcAccounts, err := walletrpc.AccountsToWatchOnly(
				jsonAccts.Accounts,
			)
			if err != nil {
				return fmt.Errorf("error converting JSON "+
					"accounts to RPC: %v", err)
			}

			watchOnly = &lnrpc.WatchOnly{
				MasterKeyBirthdayTimestamp: 0,
				Accounts:                   rpcAccounts,
			}
		}

		return createWalletRpc(
			seedWords, seedPassPhrase, walletPassword,
			x.InitRpc.Server, x.InitRpc.TLSCertPath, x.Network,
			watchOnly,
		)

	default:
		return fmt.Errorf("invalid init type %s", x.InitType)
	}
}

func createWalletFile(cipherSeed *aezeed.CipherSeed, walletPassword, walletDir,
	network string, validatePassword bool) error {

	// The wallet directory must either not exist yet or be a directory.
	stat, err := os.Stat(walletDir)
	switch {
	case os.IsNotExist(err):
		err = os.MkdirAll(walletDir, defaultDirPermissions)
		if err != nil {
			return fmt.Errorf("error creating directory %s: %v",
				walletDir, err)
		}

	case !stat.IsDir():
		return fmt.Errorf("output wallet directory must not be a file")
	}

	// We should now be able to properly determine if a wallet already
	// exists or not. Depending on the flags, we either create or validate
	// the wallet now.
	walletFile := filepath.Join(walletDir, wallet.WalletDBName)
	switch {
	case lnrpc.FileExists(walletFile) && !validatePassword:
		return fmt.Errorf("wallet file %s exists: %v", walletFile,
			errTargetExists)

	case !lnrpc.FileExists(walletFile):
		return createWallet(
			walletDir, cipherSeed, []byte(walletPassword),
			network,
		)

	default:
		return validateWallet(
			walletDir, []byte(walletPassword), network,
		)
	}
}

func createWallet(walletDir string, cipherSeed *aezeed.CipherSeed,
	walletPassword []byte, network string) error {

	log("Creating new wallet in %s", walletDir)

	// The network parameters are needed for some wallet internal things
	// like the chain genesis hash and timestamp.
	netParams, err := getNetworkParams(network)
	if err != nil {
		return err
	}

	// Create the wallet now.
	loader := wallet.NewLoader(
		netParams, walletDir, defaultNoFreelistSync,
		defaultWalletDBTimeout, 0,
	)

	_, err = loader.CreateNewWallet(
		walletPassword, walletPassword, cipherSeed.Entropy[:],
		cipherSeed.BirthdayTime(),
	)
	if err != nil {
		return fmt.Errorf("error creating wallet from seed: %v", err)
	}

	// Close the wallet properly to release the file lock on the DB.
	if err := loader.UnloadWallet(); err != nil {
		return fmt.Errorf("error unloading wallet after creation: %v",
			err)
	}

	log("Wallet created successfully in %s", walletDir)

	return nil
}

func validateWallet(walletDir string, walletPassword []byte,
	network string) error {

	log("Validating password for wallet in %s", walletDir)

	// The network parameters are needed for some wallet internal things
	// like the chain genesis hash and timestamp.
	netParams, err := getNetworkParams(network)
	if err != nil {
		return err
	}

	// Try to load the wallet now. This will fail if the wallet is already
	// loaded by another process or does not exist yet.
	loader := wallet.NewLoader(
		netParams, walletDir, defaultNoFreelistSync,
		defaultWalletDBTimeout, 0,
	)
	_, err = loader.OpenExistingWallet(walletPassword, false)
	if err != nil {
		return fmt.Errorf("error validating wallet password: %v", err)
	}

	if err := loader.UnloadWallet(); err != nil {
		return fmt.Errorf("error unloading wallet after validation: %v",
			err)
	}

	log("Wallet password validated successfully")

	return nil
}

func createWalletRpc(seedWords []string, seedPassword, walletPassword,
	rpcServer, tlsPath, network string, watchOnly *lnrpc.WatchOnly) error {

	// First, we want to make sure the wallet doesn't actually exist. We
	// need to arrive at that state within a reasonable time too, otherwise
	// it might mean we skipped the state, in which case we abort anyway.
	// Because we set a timeout, we don't actually need to listen for
	// interrupts, as we'll bail out after the timeout anyway.
	quit := make(chan struct{})
	err := waitUntilStatus(
		rpcServer, lnrpc.WalletState_NON_EXISTING,
		defaultStartupTimeout, quit,
	)
	if err != nil {
		return fmt.Errorf("error waiting for lnd startup: %v", err)
	}

	// We are now certain that the wallet doesn't exist yet, so we can go
	// ahead and try to create it.
	client, err := getUnlockerConnection(rpcServer, tlsPath)
	if err != nil {
		return fmt.Errorf("error creating wallet unlocker connextion: "+
			"%v", err)
	}

	ctxb := context.Background()
	_, err = client.InitWallet(ctxb, &lnrpc.InitWalletRequest{
		CipherSeedMnemonic: seedWords,
		AezeedPassphrase:   []byte(seedPassword),
		WalletPassword:     []byte(walletPassword),
		WatchOnly:          watchOnly,
	})
	return err
}

func checkSeed(seed, seedPassPhrase string) (*aezeed.CipherSeed, error) {
	// Decrypt the seed now to make sure we got valid data before we
	// check anything else.
	seedWords := strings.Split(seed, " ")
	if len(seedWords) != aezeed.NumMnemonicWords {
		return nil, fmt.Errorf("invalid seed, expected %d words but "+
			"got %d", aezeed.NumMnemonicWords, len(seedWords))
	}
	var seedMnemonic aezeed.Mnemonic
	copy(seedMnemonic[:], seedWords)
	cipherSeed, err := seedMnemonic.ToCipherSeed([]byte(seedPassPhrase))
	if err != nil {
		return nil, fmt.Errorf("error decrypting seed with "+
			"passphrase: %v", err)
	}

	return cipherSeed, nil
}

func getNetworkParams(network string) (*chaincfg.Params, error) {
	switch strings.ToLower(network) {
	case "mainnet":
		return &chaincfg.MainNetParams, nil

	case "testnet", "testnet3":
		return &chaincfg.TestNet3Params, nil

	case "regtest":
		return &chaincfg.RegressionNetParams, nil

	case "simnet":
		return &chaincfg.SimNetParams, nil

	default:
		return nil, fmt.Errorf("unknown network: %v", network)
	}
}

func getUnlockerConnection(rpcServer,
	tlsPath string) (lnrpc.WalletUnlockerClient, error) {

	creds, err := credentials.NewClientTLSFromFile(tlsPath, "")
	if err != nil {
		return nil, fmt.Errorf("error loading TLS certificate "+
			"from %s: %v", tlsPath, err)
	}

	// We need to use a custom dialer so we can also connect to unix sockets
	// and not just TCP addresses.
	genericDialer := lncfg.ClientAddressDialer(defaultRPCPort)
	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(creds),
		grpc.WithContextDialer(genericDialer),
	}

	conn, err := grpc.Dial(rpcServer, opts...)
	if err != nil {
		return nil, fmt.Errorf("unable to connect to RPC server: %v",
			err)
	}

	return lnrpc.NewWalletUnlockerClient(conn), nil
}
