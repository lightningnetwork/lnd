package lnd

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btclog"
	"github.com/btcsuite/btcwallet/wallet"
	"github.com/btcsuite/btcwallet/walletdb"
	proxy "github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/lightninglabs/neutrino"
	"github.com/lightninglabs/neutrino/headerfs"
	"github.com/lightningnetwork/lnd/blockcache"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/lnwallet/btcwallet"
	"github.com/lightningnetwork/lnd/macaroons"
	"github.com/lightningnetwork/lnd/tor"
	"github.com/lightningnetwork/lnd/walletunlocker"
	"github.com/lightningnetwork/lnd/watchtower"
	"github.com/lightningnetwork/lnd/watchtower/wtclient"
	"github.com/lightningnetwork/lnd/watchtower/wtdb"
	"google.golang.org/grpc"
	"gopkg.in/macaroon-bakery.v2/bakery"
)

// GrpcRegistrar is an interface that must be satisfied by an external subserver
// that wants to be able to register its own gRPC server onto lnd's main
// grpc.Server instance.
type GrpcRegistrar interface {
	// RegisterGrpcSubserver is called for each net.Listener on which lnd
	// creates a grpc.Server instance. External subservers implementing this
	// method can then register their own gRPC server structs to the main
	// server instance.
	RegisterGrpcSubserver(*grpc.Server) error
}

// RestRegistrar is an interface that must be satisfied by an external subserver
// that wants to be able to register its own REST mux onto lnd's main
// proxy.ServeMux instance.
type RestRegistrar interface {
	// RegisterRestSubserver is called after lnd creates the main
	// proxy.ServeMux instance. External subservers implementing this method
	// can then register their own REST proxy stubs to the main server
	// instance.
	RegisterRestSubserver(context.Context, *proxy.ServeMux, string,
		[]grpc.DialOption) error
}

// ExternalValidator is an interface that must be satisfied by an external
// macaroon validator.
type ExternalValidator interface {
	macaroons.MacaroonValidator

	// Permissions returns the permissions that the external validator is
	// validating. It is a map between the full HTTP URI of each RPC and its
	// required macaroon permissions. If multiple action/entity tuples are
	// specified per URI, they are all required. See rpcserver.go for a list
	// of valid action and entity values.
	Permissions() map[string][]bakery.Op
}

// DatabaseBuilder is an interface that must be satisfied by the implementation
// that provides lnd's main database backend instances.
type DatabaseBuilder interface {
	// BuildDatabase extracts the current databases that we'll use for
	// normal operation in the daemon. A function closure that closes all
	// opened databases is also returned.
	BuildDatabase(ctx context.Context) (*DatabaseInstances, func(), error)
}

// ImplementationCfg is a struct that holds all configuration items for
// components that can be implemented outside lnd itself.
type ImplementationCfg struct {
	// GrpcRegistrar is a type that can register additional gRPC subservers
	// before the main gRPC server is started.
	GrpcRegistrar

	// RestRegistrar is a type that can register additional REST subservers
	// before the main REST proxy is started.
	RestRegistrar

	// ExternalValidator is a type that can provide external macaroon
	// validation.
	ExternalValidator

	// DatabaseBuilder is a type that can provide lnd's main database
	// backend instances.
	DatabaseBuilder
}

// DefaultWalletImpl is the default implementation of our normal, btcwallet
// backed configuration.
type DefaultWalletImpl struct {
}

// RegisterRestSubserver is called after lnd creates the main proxy.ServeMux
// instance. External subservers implementing this method can then register
// their own REST proxy stubs to the main server instance.
//
// NOTE: This is part of the GrpcRegistrar interface.
func (d *DefaultWalletImpl) RegisterRestSubserver(context.Context,
	*proxy.ServeMux, string, []grpc.DialOption) error {

	return nil
}

// RegisterGrpcSubserver is called for each net.Listener on which lnd creates a
// grpc.Server instance. External subservers implementing this method can then
// register their own gRPC server structs to the main server instance.
//
// NOTE: This is part of the GrpcRegistrar interface.
func (d *DefaultWalletImpl) RegisterGrpcSubserver(*grpc.Server) error {
	return nil
}

// ValidateMacaroon extracts the macaroon from the context's gRPC metadata,
// checks its signature, makes sure all specified permissions for the called
// method are contained within and finally ensures all caveat conditions are
// met. A non-nil error is returned if any of the checks fail.
//
// NOTE: This is part of the ExternalValidator interface.
func (d *DefaultWalletImpl) ValidateMacaroon(ctx context.Context,
	requiredPermissions []bakery.Op, fullMethod string) error {

	// Because the default implementation does not return any permissions,
	// we shouldn't be registered as an external validator at all and this
	// should never be invoked.
	return fmt.Errorf("default implementation does not support external " +
		"macaroon validation")
}

// Permissions returns the permissions that the external validator is
// validating. It is a map between the full HTTP URI of each RPC and its
// required macaroon permissions. If multiple action/entity tuples are specified
// per URI, they are all required. See rpcserver.go for a list of valid action
// and entity values.
//
// NOTE: This is part of the ExternalValidator interface.
func (d *DefaultWalletImpl) Permissions() map[string][]bakery.Op {
	return nil
}

// DatabaseInstances is a struct that holds all instances to the actual
// databases that are used in lnd.
type DatabaseInstances struct {
	// GraphDB is the database that stores the channel graph used for path
	// finding.
	//
	// NOTE/TODO: This currently _needs_ to be the same instance as the
	// ChanStateDB below until the separation of the two databases is fully
	// complete!
	GraphDB *channeldb.DB

	// ChanStateDB is the database that stores all of our node's channel
	// state.
	//
	// NOTE/TODO: This currently _needs_ to be the same instance as the
	// GraphDB above until the separation of the two databases is fully
	// complete!
	ChanStateDB *channeldb.DB

	// HeightHintDB is the database that stores height hints for spends.
	HeightHintDB kvdb.Backend

	// MacaroonDB is the database that stores macaroon root keys.
	MacaroonDB kvdb.Backend

	// DecayedLogDB is the database that stores p2p related encryption
	// information.
	DecayedLogDB kvdb.Backend

	// TowerClientDB is the database that stores the watchtower client's
	// configuration.
	TowerClientDB wtclient.DB

	// TowerServerDB is the database that stores the watchtower server's
	// configuration.
	TowerServerDB watchtower.DB

	// WalletDB is the configuration for loading the wallet database using
	// the btcwallet's loader.
	WalletDB btcwallet.LoaderOption
}

// DefaultDatabaseBuilder is a type that builds the default database backends
// for lnd, using the given configuration to decide what actual implementation
// to use.
type DefaultDatabaseBuilder struct {
	cfg    *Config
	logger btclog.Logger
}

// NewDefaultDatabaseBuilder returns a new instance of the default database
// builder.
func NewDefaultDatabaseBuilder(cfg *Config,
	logger btclog.Logger) *DefaultDatabaseBuilder {

	return &DefaultDatabaseBuilder{
		cfg:    cfg,
		logger: logger,
	}
}

// BuildDatabase extracts the current databases that we'll use for normal
// operation in the daemon. A function closure that closes all opened databases
// is also returned.
func (d *DefaultDatabaseBuilder) BuildDatabase(
	ctx context.Context) (*DatabaseInstances, func(), error) {

	d.logger.Infof("Opening the main database, this might take a few " +
		"minutes...")

	cfg := d.cfg
	if cfg.DB.Backend == lncfg.BoltBackend {
		d.logger.Infof("Opening bbolt database, sync_freelist=%v, "+
			"auto_compact=%v", !cfg.DB.Bolt.NoFreelistSync,
			cfg.DB.Bolt.AutoCompact)
	}

	startOpenTime := time.Now()

	databaseBackends, err := cfg.DB.GetBackends(
		ctx, cfg.graphDatabaseDir(), cfg.networkDir, filepath.Join(
			cfg.Watchtower.TowerDir,
			cfg.registeredChains.PrimaryChain().String(),
			lncfg.NormalizeNetwork(cfg.ActiveNetParams.Name),
		), cfg.WtClient.Active, cfg.Watchtower.Active,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("unable to obtain database "+
			"backends: %v", err)
	}

	// With the full remote mode we made sure both the graph and channel
	// state DB point to the same local or remote DB and the same namespace
	// within that DB.
	dbs := &DatabaseInstances{
		HeightHintDB: databaseBackends.HeightHintDB,
		MacaroonDB:   databaseBackends.MacaroonDB,
		DecayedLogDB: databaseBackends.DecayedLogDB,
		WalletDB:     databaseBackends.WalletDB,
	}
	cleanUp := func() {
		// We can just close the returned close functions directly. Even
		// if we decorate the channel DB with an additional struct, its
		// close function still just points to the kvdb backend.
		for name, closeFunc := range databaseBackends.CloseFuncs {
			if err := closeFunc(); err != nil {
				d.logger.Errorf("Error closing %s "+
					"database: %v", name, err)
			}
		}
	}
	if databaseBackends.Remote {
		d.logger.Infof("Using remote %v database! Creating "+
			"graph and channel state DB instances", cfg.DB.Backend)
	} else {
		d.logger.Infof("Creating local graph and channel state DB " +
			"instances")
	}

	dbOptions := []channeldb.OptionModifier{
		channeldb.OptionSetRejectCacheSize(cfg.Caches.RejectCacheSize),
		channeldb.OptionSetChannelCacheSize(cfg.Caches.ChannelCacheSize),
		channeldb.OptionSetBatchCommitInterval(cfg.DB.BatchCommitInterval),
		channeldb.OptionDryRunMigration(cfg.DryRunMigration),
	}

	// We want to pre-allocate the channel graph cache according to what we
	// expect for mainnet to speed up memory allocation.
	if cfg.ActiveNetParams.Name == chaincfg.MainNetParams.Name {
		dbOptions = append(
			dbOptions, channeldb.OptionSetPreAllocCacheNumNodes(
				channeldb.DefaultPreAllocCacheNumNodes,
			),
		)
	}

	// Otherwise, we'll open two instances, one for the state we only need
	// locally, and the other for things we want to ensure are replicated.
	dbs.GraphDB, err = channeldb.CreateWithBackend(
		databaseBackends.GraphDB, dbOptions...,
	)
	switch {
	// Give the DB a chance to dry run the migration. Since we know that
	// both the channel state and graph DBs are still always behind the same
	// backend, we know this would be applied to both of those DBs.
	case err == channeldb.ErrDryRunMigrationOK:
		d.logger.Infof("Graph DB dry run migration successful")
		return nil, nil, err

	case err != nil:
		cleanUp()

		err := fmt.Errorf("unable to open graph DB: %v", err)
		d.logger.Error(err)
		return nil, nil, err
	}

	// For now, we don't _actually_ split the graph and channel state DBs on
	// the code level. Since they both are based upon the *channeldb.DB
	// struct it will require more refactoring to fully separate them. With
	// the full remote mode we at least know for now that they both point to
	// the same DB backend (and also namespace within that) so we only need
	// to apply any migration once.
	//
	// TODO(guggero): Once the full separation of anything graph related
	// from the channeldb.DB is complete, the decorated instance of the
	// channel state DB should be created here individually instead of just
	// using the same struct (and DB backend) instance.
	dbs.ChanStateDB = dbs.GraphDB

	// Wrap the watchtower client DB and make sure we clean up.
	if cfg.WtClient.Active {
		dbs.TowerClientDB, err = wtdb.OpenClientDB(
			databaseBackends.TowerClientDB,
		)
		if err != nil {
			cleanUp()

			err := fmt.Errorf("unable to open %s database: %v",
				lncfg.NSTowerClientDB, err)
			d.logger.Error(err)
			return nil, nil, err
		}
	}

	// Wrap the watchtower server DB and make sure we clean up.
	if cfg.Watchtower.Active {
		dbs.TowerServerDB, err = wtdb.OpenTowerDB(
			databaseBackends.TowerServerDB,
		)
		if err != nil {
			cleanUp()

			err := fmt.Errorf("unable to open %s database: %v",
				lncfg.NSTowerServerDB, err)
			d.logger.Error(err)
			return nil, nil, err
		}
	}

	openTime := time.Since(startOpenTime)
	d.logger.Infof("Database(s) now open (time_to_open=%v)!", openTime)

	return dbs, cleanUp, nil
}

// waitForWalletPassword blocks until a password is provided by the user to
// this RPC server.
func waitForWalletPassword(cfg *Config,
	pwService *walletunlocker.UnlockerService,
	loaderOpts []btcwallet.LoaderOption, shutdownChan <-chan struct{}) (
	*walletunlocker.WalletUnlockParams, error) {

	// Wait for user to provide the password.
	ltndLog.Infof("Waiting for wallet encryption password. Use `lncli " +
		"create` to create a wallet, `lncli unlock` to unlock an " +
		"existing wallet, or `lncli changepassword` to change the " +
		"password of an existing wallet and unlock it.")

	// We currently don't distinguish between getting a password to be used
	// for creation or unlocking, as a new wallet db will be created if
	// none exists when creating the chain control.
	select {

	// The wallet is being created for the first time, we'll check to see
	// if the user provided any entropy for seed creation. If so, then
	// we'll create the wallet early to load the seed.
	case initMsg := <-pwService.InitMsgs:
		password := initMsg.Passphrase
		cipherSeed := initMsg.WalletSeed
		extendedKey := initMsg.WalletExtendedKey
		recoveryWindow := initMsg.RecoveryWindow

		// Before we proceed, we'll check the internal version of the
		// seed. If it's greater than the current key derivation
		// version, then we'll return an error as we don't understand
		// this.
		const latestVersion = keychain.KeyDerivationVersion
		if cipherSeed != nil &&
			cipherSeed.InternalVersion != latestVersion {

			return nil, fmt.Errorf("invalid internal "+
				"seed version %v, current version is %v",
				cipherSeed.InternalVersion,
				keychain.KeyDerivationVersion)
		}

		loader, err := btcwallet.NewWalletLoader(
			cfg.ActiveNetParams.Params, recoveryWindow,
			loaderOpts...,
		)
		if err != nil {
			return nil, err
		}

		// With the seed, we can now use the wallet loader to create
		// the wallet, then pass it back to avoid unlocking it again.
		var (
			birthday  time.Time
			newWallet *wallet.Wallet
		)
		switch {
		// A normal cipher seed was given, use the birthday encoded in
		// it and create the wallet from that.
		case cipherSeed != nil:
			birthday = cipherSeed.BirthdayTime()
			newWallet, err = loader.CreateNewWallet(
				password, password, cipherSeed.Entropy[:],
				birthday,
			)

		// No seed was given, we're importing a wallet from its extended
		// private key.
		case extendedKey != nil:
			birthday = initMsg.ExtendedKeyBirthday
			newWallet, err = loader.CreateNewWalletExtendedKey(
				password, password, extendedKey, birthday,
			)

		default:
			// The unlocker service made sure either the cipher seed
			// or the extended key is set so, we shouldn't get here.
			// The default case is just here for readability and
			// completeness.
			err = fmt.Errorf("cannot create wallet, neither seed " +
				"nor extended key was given")
		}
		if err != nil {
			// Don't leave the file open in case the new wallet
			// could not be created for whatever reason.
			if err := loader.UnloadWallet(); err != nil {
				ltndLog.Errorf("Could not unload new "+
					"wallet: %v", err)
			}
			return nil, err
		}

		// For new wallets, the ResetWalletTransactions flag is a no-op.
		if cfg.ResetWalletTransactions {
			ltndLog.Warnf("Ignoring reset-wallet-transactions " +
				"flag for new wallet as it has no effect")
		}

		return &walletunlocker.WalletUnlockParams{
			Password:        password,
			Birthday:        birthday,
			RecoveryWindow:  recoveryWindow,
			Wallet:          newWallet,
			ChansToRestore:  initMsg.ChanBackups,
			UnloadWallet:    loader.UnloadWallet,
			StatelessInit:   initMsg.StatelessInit,
			MacResponseChan: pwService.MacResponseChan,
		}, nil

	// The wallet has already been created in the past, and is simply being
	// unlocked. So we'll just return these passphrases.
	case unlockMsg := <-pwService.UnlockMsgs:
		// Resetting the transactions is something the user likely only
		// wants to do once so we add a prominent warning to the log to
		// remind the user to turn off the setting again after
		// successful completion.
		if cfg.ResetWalletTransactions {
			ltndLog.Warnf("Dropped all transaction history from " +
				"on-chain wallet. Remember to disable " +
				"reset-wallet-transactions flag for next " +
				"start of lnd")
		}

		return &walletunlocker.WalletUnlockParams{
			Password:        unlockMsg.Passphrase,
			RecoveryWindow:  unlockMsg.RecoveryWindow,
			Wallet:          unlockMsg.Wallet,
			ChansToRestore:  unlockMsg.ChanBackups,
			UnloadWallet:    unlockMsg.UnloadWallet,
			StatelessInit:   unlockMsg.StatelessInit,
			MacResponseChan: pwService.MacResponseChan,
		}, nil

	// If we got a shutdown signal we just return with an error immediately
	case <-shutdownChan:
		return nil, fmt.Errorf("shutting down")
	}
}

// initNeutrinoBackend inits a new instance of the neutrino light client
// backend given a target chain directory to store the chain state.
func initNeutrinoBackend(cfg *Config, chainDir string,
	blockCache *blockcache.BlockCache) (*neutrino.ChainService,
	func(), error) {

	// Both channel validation flags are false by default but their meaning
	// is the inverse of each other. Therefore both cannot be true. For
	// every other case, the neutrino.validatechannels overwrites the
	// routing.assumechanvalid value.
	if cfg.NeutrinoMode.ValidateChannels && cfg.Routing.AssumeChannelValid {
		return nil, nil, fmt.Errorf("can't set both " +
			"neutrino.validatechannels and routing." +
			"assumechanvalid to true at the same time")
	}
	cfg.Routing.AssumeChannelValid = !cfg.NeutrinoMode.ValidateChannels

	// First we'll open the database file for neutrino, creating the
	// database if needed. We append the normalized network name here to
	// match the behavior of btcwallet.
	dbPath := filepath.Join(
		chainDir, lncfg.NormalizeNetwork(cfg.ActiveNetParams.Name),
	)

	// Ensure that the neutrino db path exists.
	if err := os.MkdirAll(dbPath, 0700); err != nil {
		return nil, nil, err
	}

	dbName := filepath.Join(dbPath, "neutrino.db")
	db, err := walletdb.Create(
		"bdb", dbName, !cfg.SyncFreelist, cfg.DB.Bolt.DBTimeout,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("unable to create neutrino "+
			"database: %v", err)
	}

	headerStateAssertion, err := parseHeaderStateAssertion(
		cfg.NeutrinoMode.AssertFilterHeader,
	)
	if err != nil {
		db.Close()
		return nil, nil, err
	}

	// With the database open, we can now create an instance of the
	// neutrino light client. We pass in relevant configuration parameters
	// required.
	config := neutrino.Config{
		DataDir:      dbPath,
		Database:     db,
		ChainParams:  *cfg.ActiveNetParams.Params,
		AddPeers:     cfg.NeutrinoMode.AddPeers,
		ConnectPeers: cfg.NeutrinoMode.ConnectPeers,
		Dialer: func(addr net.Addr) (net.Conn, error) {
			dialAddr := addr
			if tor.IsOnionFakeIP(addr) {
				// Because the Neutrino address manager only
				// knows IP addresses, we need to turn any fake
				// tcp6 address that actually encodes an Onion
				// v2 address back into the hostname
				// representation before we can pass it to the
				// dialer.
				var err error
				dialAddr, err = tor.FakeIPToOnionHost(addr)
				if err != nil {
					return nil, err
				}
			}

			return cfg.net.Dial(
				dialAddr.Network(), dialAddr.String(),
				cfg.ConnectionTimeout,
			)
		},
		NameResolver: func(host string) ([]net.IP, error) {
			if tor.IsOnionHost(host) {
				// Neutrino internally uses btcd's address
				// manager which only operates on an IP level
				// and does not understand onion hosts. We need
				// to turn an onion host into a fake
				// representation of an IP address to make it
				// possible to connect to a block filter backend
				// that serves on an Onion v2 hidden service.
				fakeIP, err := tor.OnionHostToFakeIP(host)
				if err != nil {
					return nil, err
				}

				return []net.IP{fakeIP}, nil
			}

			addrs, err := cfg.net.LookupHost(host)
			if err != nil {
				return nil, err
			}

			ips := make([]net.IP, 0, len(addrs))
			for _, strIP := range addrs {
				ip := net.ParseIP(strIP)
				if ip == nil {
					continue
				}

				ips = append(ips, ip)
			}

			return ips, nil
		},
		AssertFilterHeader: headerStateAssertion,
		BlockCache:         blockCache.Cache,
		BroadcastTimeout:   cfg.NeutrinoMode.BroadcastTimeout,
		PersistToDisk:      cfg.NeutrinoMode.PersistFilters,
	}

	neutrino.MaxPeers = 8
	neutrino.BanDuration = time.Hour * 48
	neutrino.UserAgentName = cfg.NeutrinoMode.UserAgentName
	neutrino.UserAgentVersion = cfg.NeutrinoMode.UserAgentVersion

	neutrinoCS, err := neutrino.NewChainService(config)
	if err != nil {
		db.Close()
		return nil, nil, fmt.Errorf("unable to create neutrino light "+
			"client: %v", err)
	}

	if err := neutrinoCS.Start(); err != nil {
		db.Close()
		return nil, nil, err
	}

	cleanUp := func() {
		if err := neutrinoCS.Stop(); err != nil {
			ltndLog.Infof("Unable to stop neutrino light client: %v", err)
		}
		db.Close()
	}

	return neutrinoCS, cleanUp, nil
}

// parseHeaderStateAssertion parses the user-specified neutrino header state
// into a headerfs.FilterHeader.
func parseHeaderStateAssertion(state string) (*headerfs.FilterHeader, error) {
	if len(state) == 0 {
		return nil, nil
	}

	split := strings.Split(state, ":")
	if len(split) != 2 {
		return nil, fmt.Errorf("header state assertion %v in "+
			"unexpected format, expected format height:hash", state)
	}

	height, err := strconv.ParseUint(split[0], 10, 32)
	if err != nil {
		return nil, fmt.Errorf("invalid filter header height: %v", err)
	}

	hash, err := chainhash.NewHashFromStr(split[1])
	if err != nil {
		return nil, fmt.Errorf("invalid filter header hash: %v", err)
	}

	return &headerfs.FilterHeader{
		Height:     uint32(height),
		FilterHash: *hash,
	}, nil
}
