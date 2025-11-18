package lnd

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/btcsuite/btcwallet/chain"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/btcsuite/btcwallet/wallet"
	"github.com/btcsuite/btcwallet/walletdb"
	proxy "github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/lightninglabs/neutrino"
	"github.com/lightninglabs/neutrino/blockntfns"
	"github.com/lightninglabs/neutrino/headerfs"
	"github.com/lightninglabs/neutrino/pushtx"
	"github.com/lightningnetwork/lnd/blockcache"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/chainreg"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/funding"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	graphdbmig1 "github.com/lightningnetwork/lnd/graph/db/migration1"
	graphmig1sqlc "github.com/lightningnetwork/lnd/graph/db/migration1/sqlc"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/btcwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chancloser"
	"github.com/lightningnetwork/lnd/lnwallet/rpcwallet"
	"github.com/lightningnetwork/lnd/macaroons"
	"github.com/lightningnetwork/lnd/msgmux"
	paymentsdb "github.com/lightningnetwork/lnd/payments/db"
	"github.com/lightningnetwork/lnd/rpcperms"
	"github.com/lightningnetwork/lnd/signal"
	"github.com/lightningnetwork/lnd/sqldb"
	"github.com/lightningnetwork/lnd/sqldb/sqlc"
	"github.com/lightningnetwork/lnd/sweep"
	"github.com/lightningnetwork/lnd/walletunlocker"
	"github.com/lightningnetwork/lnd/watchtower"
	"github.com/lightningnetwork/lnd/watchtower/wtclient"
	"github.com/lightningnetwork/lnd/watchtower/wtdb"
	"google.golang.org/grpc"
	"gopkg.in/macaroon-bakery.v2/bakery"
)

const (
	// invoiceMigrationBatchSize is the number of invoices that will be
	// migrated in a single batch.
	invoiceMigrationBatchSize = 1000

	// invoiceMigration is the version of the migration that will be used to
	// migrate invoices from the kvdb to the sql database.
	invoiceMigration = 7

	// graphMigration is the version number for the graph migration
	// that migrates the KV graph to the native SQL schema.
	graphMigration = 10
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

// WalletConfigBuilder is an interface that must be satisfied by a custom wallet
// implementation.
type WalletConfigBuilder interface {
	// BuildWalletConfig is responsible for creating or unlocking and then
	// fully initializing a wallet.
	BuildWalletConfig(context.Context, *DatabaseInstances, *AuxComponents,
		*rpcperms.InterceptorChain,
		[]*ListenerWithSignal) (*chainreg.PartialChainControl,
		*btcwallet.Config, func(), error)
}

// ChainControlBuilder is an interface that must be satisfied by a custom wallet
// implementation.
type ChainControlBuilder interface {
	// BuildChainControl is responsible for creating a fully populated chain
	// control instance from a wallet.
	BuildChainControl(*chainreg.PartialChainControl,
		*btcwallet.Config) (*chainreg.ChainControl, func(), error)
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

	// WalletConfigBuilder is a type that can provide a wallet configuration
	// with a fully loaded and unlocked wallet.
	WalletConfigBuilder

	// ChainControlBuilder is a type that can provide a custom wallet
	// implementation.
	ChainControlBuilder

	// AuxComponents is a set of auxiliary components that can be used by
	// lnd for certain custom channel types.
	AuxComponents
}

// AuxComponents is a set of auxiliary components that can be used by lnd for
// certain custom channel types.
type AuxComponents struct {
	// AuxLeafStore is an optional data source that can be used by custom
	// channels to fetch+store various data.
	AuxLeafStore fn.Option[lnwallet.AuxLeafStore]

	// TrafficShaper is an optional traffic shaper that can be used to
	// control the outgoing channel of a payment.
	TrafficShaper fn.Option[htlcswitch.AuxTrafficShaper]

	// MsgRouter is an optional message router that if set will be used in
	// place of a new blank default message router.
	MsgRouter fn.Option[msgmux.Router]

	// AuxFundingController is an optional controller that can be used to
	// modify the way we handle certain custom channel types. It's also
	// able to automatically handle new custom protocol messages related to
	// the funding process.
	AuxFundingController fn.Option[funding.AuxFundingController]

	// AuxSigner is an optional signer that can be used to sign auxiliary
	// leaves for certain custom channel types.
	AuxSigner fn.Option[lnwallet.AuxSigner]

	// AuxDataParser is an optional data parser that can be used to parse
	// auxiliary data for certain custom channel types.
	AuxDataParser fn.Option[AuxDataParser]

	// AuxChanCloser is an optional channel closer that can be used to
	// modify the way a coop-close transaction is constructed.
	AuxChanCloser fn.Option[chancloser.AuxChanCloser]

	// AuxSweeper is an optional interface that can be used to modify the
	// way sweep transaction are generated.
	AuxSweeper fn.Option[sweep.AuxSweeper]

	// AuxContractResolver is an optional interface that can be used to
	// modify the way contracts are resolved.
	AuxContractResolver fn.Option[lnwallet.AuxContractResolver]

	// AuxChannelNegotiator is an optional interface that allows aux channel
	// implementations to inject and process custom records over channel
	// related wire messages.
	AuxChannelNegotiator fn.Option[lnwallet.AuxChannelNegotiator]
}

// DefaultWalletImpl is the default implementation of our normal, btcwallet
// backed configuration.
type DefaultWalletImpl struct {
	cfg         *Config
	logger      btclog.Logger
	interceptor signal.Interceptor

	watchOnly        bool
	migrateWatchOnly bool
	pwService        *walletunlocker.UnlockerService
}

// NewDefaultWalletImpl creates a new default wallet implementation.
func NewDefaultWalletImpl(cfg *Config, logger btclog.Logger,
	interceptor signal.Interceptor, watchOnly bool) *DefaultWalletImpl {

	return &DefaultWalletImpl{
		cfg:         cfg,
		logger:      logger,
		interceptor: interceptor,
		watchOnly:   watchOnly,
		pwService:   createWalletUnlockerService(cfg),
	}
}

// RegisterRestSubserver is called after lnd creates the main proxy.ServeMux
// instance. External subservers implementing this method can then register
// their own REST proxy stubs to the main server instance.
//
// NOTE: This is part of the GrpcRegistrar interface.
func (d *DefaultWalletImpl) RegisterRestSubserver(ctx context.Context,
	mux *proxy.ServeMux, restProxyDest string,
	restDialOpts []grpc.DialOption) error {

	return lnrpc.RegisterWalletUnlockerHandlerFromEndpoint(
		ctx, mux, restProxyDest, restDialOpts,
	)
}

// RegisterGrpcSubserver is called for each net.Listener on which lnd creates a
// grpc.Server instance. External subservers implementing this method can then
// register their own gRPC server structs to the main server instance.
//
// NOTE: This is part of the GrpcRegistrar interface.
func (d *DefaultWalletImpl) RegisterGrpcSubserver(s *grpc.Server) error {
	lnrpc.RegisterWalletUnlockerServer(s, d.pwService)

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

// BuildWalletConfig is responsible for creating or unlocking and then
// fully initializing a wallet.
//
// NOTE: This is part of the WalletConfigBuilder interface.
func (d *DefaultWalletImpl) BuildWalletConfig(ctx context.Context,
	dbs *DatabaseInstances, aux *AuxComponents,
	interceptorChain *rpcperms.InterceptorChain,
	grpcListeners []*ListenerWithSignal) (*chainreg.PartialChainControl,
	*btcwallet.Config, func(), error) {

	// Keep track of our various cleanup functions. We use a defer function
	// as well to not repeat ourselves with every return statement.
	var (
		cleanUpTasks []func()
		earlyExit    = true
		cleanUp      = func() {
			for _, fn := range cleanUpTasks {
				if fn == nil {
					continue
				}

				fn()
			}
		}
	)
	defer func() {
		if earlyExit {
			cleanUp()
		}
	}()

	// Initialize a new block cache.
	blockCache := blockcache.NewBlockCache(d.cfg.BlockCacheSize)

	// Before starting the wallet, we'll create and start our Neutrino
	// light client instance, if enabled, in order to allow it to sync
	// while the rest of the daemon continues startup.
	mainChain := d.cfg.Bitcoin
	var neutrinoCS *neutrino.ChainService
	if mainChain.Node == "neutrino" {
		neutrinoBackend, neutrinoCleanUp, err := initNeutrinoBackend(
			ctx, d.cfg, mainChain.ChainDir, blockCache,
		)
		if err != nil {
			err := fmt.Errorf("unable to initialize neutrino "+
				"backend: %v", err)
			d.logger.Error(err)
			return nil, nil, nil, err
		}
		cleanUpTasks = append(cleanUpTasks, neutrinoCleanUp)
		neutrinoCS = neutrinoBackend
	}

	var (
		walletInitParams = walletunlocker.WalletUnlockParams{
			// In case we do auto-unlock, we need to be able to send
			// into the channel without blocking so we buffer it.
			MacResponseChan: make(chan []byte, 1),
		}
		privateWalletPw = lnwallet.DefaultPrivatePassphrase
		publicWalletPw  = lnwallet.DefaultPublicPassphrase
	)

	// If the user didn't request a seed, then we'll manually assume a
	// wallet birthday of now, as otherwise the seed would've specified
	// this information.
	walletInitParams.Birthday = time.Now()

	d.pwService.SetLoaderOpts([]btcwallet.LoaderOption{dbs.WalletDB})
	d.pwService.SetMacaroonDB(dbs.MacaroonDB)
	walletExists, err := d.pwService.WalletExists()
	if err != nil {
		return nil, nil, nil, err
	}

	if !walletExists {
		interceptorChain.SetWalletNotCreated()
	} else {
		interceptorChain.SetWalletLocked()
	}

	// If we've started in auto unlock mode, then a wallet should already
	// exist because we don't want to enable the RPC unlocker in that case
	// for security reasons (an attacker could inject their seed since the
	// RPC is unauthenticated). Only if the user explicitly wants to allow
	// wallet creation we don't error out here.
	if d.cfg.WalletUnlockPasswordFile != "" && !walletExists &&
		!d.cfg.WalletUnlockAllowCreate {

		return nil, nil, nil, fmt.Errorf("wallet unlock password file " +
			"was specified but wallet does not exist; initialize " +
			"the wallet before using auto unlocking")
	}

	// What wallet mode are we running in? We've already made sure the no
	// seed backup and auto unlock aren't both set during config parsing.
	switch {
	// No seed backup means we're also using the default password.
	case d.cfg.NoSeedBackup:
		// We continue normally, the default password has already been
		// set above.

	// A password for unlocking is provided in a file.
	case d.cfg.WalletUnlockPasswordFile != "" && walletExists:
		d.logger.Infof("Attempting automatic wallet unlock with " +
			"password provided in file")
		pwBytes, err := os.ReadFile(d.cfg.WalletUnlockPasswordFile)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("error reading "+
				"password from file %s: %v",
				d.cfg.WalletUnlockPasswordFile, err)
		}

		// Remove any newlines at the end of the file. The lndinit tool
		// won't ever write a newline but maybe the file was provisioned
		// by another process or user.
		pwBytes = bytes.TrimRight(pwBytes, "\r\n")

		// We have the password now, we can ask the unlocker service to
		// do the unlock for us.
		unlockedWallet, unloadWalletFn, err := d.pwService.LoadAndUnlock(
			pwBytes, 0,
		)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("error unlocking "+
				"wallet with password from file: %v", err)
		}

		cleanUpTasks = append(cleanUpTasks, func() {
			if err := unloadWalletFn(); err != nil {
				d.logger.Errorf("Could not unload wallet: %v",
					err)
			}
		})

		privateWalletPw = pwBytes
		publicWalletPw = pwBytes
		walletInitParams.Wallet = unlockedWallet
		walletInitParams.UnloadWallet = unloadWalletFn

	// If none of the automatic startup options are selected, we fall back
	// to the default behavior of waiting for the wallet creation/unlocking
	// over RPC.
	default:
		if err := d.interceptor.Notifier.NotifyReady(false); err != nil {
			return nil, nil, nil, err
		}

		params, err := waitForWalletPassword(
			d.cfg, d.pwService, []btcwallet.LoaderOption{dbs.WalletDB},
			d.interceptor.ShutdownChannel(),
		)
		if err != nil {
			err := fmt.Errorf("unable to set up wallet password "+
				"listeners: %v", err)
			d.logger.Error(err)
			return nil, nil, nil, err
		}

		walletInitParams = *params
		privateWalletPw = walletInitParams.Password
		publicWalletPw = walletInitParams.Password
		cleanUpTasks = append(cleanUpTasks, func() {
			if err := walletInitParams.UnloadWallet(); err != nil {
				d.logger.Errorf("Could not unload wallet: %v",
					err)
			}
		})

		if walletInitParams.RecoveryWindow > 0 {
			d.logger.Infof("Wallet recovery mode enabled with "+
				"address lookahead of %d addresses",
				walletInitParams.RecoveryWindow)
		}
	}

	var macaroonService *macaroons.Service
	if !d.cfg.NoMacaroons {
		// Create the macaroon authentication/authorization service.
		rootKeyStore, err := macaroons.NewRootKeyStorage(dbs.MacaroonDB)
		if err != nil {
			return nil, nil, nil, err
		}
		macaroonService, err = macaroons.NewService(
			rootKeyStore, "lnd", walletInitParams.StatelessInit,
			macaroons.IPLockChecker, macaroons.IPRangeLockChecker,
			macaroons.CustomChecker(interceptorChain),
		)
		if err != nil {
			err := fmt.Errorf("unable to set up macaroon "+
				"authentication: %v", err)
			d.logger.Error(err)
			return nil, nil, nil, err
		}
		cleanUpTasks = append(cleanUpTasks, func() {
			if err := macaroonService.Close(); err != nil {
				d.logger.Errorf("Could not close macaroon "+
					"service: %v", err)
			}
		})

		// Try to unlock the macaroon store with the private password.
		// Ignore ErrAlreadyUnlocked since it could be unlocked by the
		// wallet unlocker.
		err = macaroonService.CreateUnlock(&privateWalletPw)
		if err != nil && err != macaroons.ErrAlreadyUnlocked {
			err := fmt.Errorf("unable to unlock macaroons: %w", err)
			d.logger.Error(err)
			return nil, nil, nil, err
		}

		// If we have a macaroon root key from the init wallet params,
		// set the root key before baking any macaroons.
		if len(walletInitParams.MacRootKey) > 0 {
			err := macaroonService.SetRootKey(
				walletInitParams.MacRootKey,
			)
			if err != nil {
				return nil, nil, nil, err
			}
		}

		// Send an admin macaroon to all our listeners that requested
		// one by setting a non-nil macaroon channel.
		adminMacBytes, err := bakeMacaroon(
			ctx, macaroonService, adminPermissions(),
		)
		if err != nil {
			return nil, nil, nil, err
		}
		for _, lis := range grpcListeners {
			if lis.MacChan != nil {
				lis.MacChan <- adminMacBytes
			}
		}

		// In case we actually needed to unlock the wallet, we now need
		// to create an instance of the admin macaroon and send it to
		// the unlocker so it can forward it to the user. In no seed
		// backup mode, there's nobody listening on the channel and we'd
		// block here forever.
		if !d.cfg.NoSeedBackup {
			// The channel is buffered by one element so writing
			// should not block here.
			walletInitParams.MacResponseChan <- adminMacBytes
		}

		// If the user requested a stateless initialization, no macaroon
		// files should be created.
		if !walletInitParams.StatelessInit {
			// Create default macaroon files for lncli to use if
			// they don't exist.
			err = genDefaultMacaroons(
				ctx, macaroonService, d.cfg.AdminMacPath,
				d.cfg.ReadMacPath, d.cfg.InvoiceMacPath,
			)
			if err != nil {
				err := fmt.Errorf("unable to create macaroons "+
					"%v", err)
				d.logger.Error(err)
				return nil, nil, nil, err
			}
		}

		// As a security service to the user, if they requested
		// stateless initialization and there are macaroon files on disk
		// we log a warning.
		if walletInitParams.StatelessInit {
			msg := "Found %s macaroon on disk (%s) even though " +
				"--stateless_init was requested. Unencrypted " +
				"state is accessible by the host system. You " +
				"should change the password and use " +
				"--new_mac_root_key with --stateless_init to " +
				"clean up and invalidate old macaroons."

			if lnrpc.FileExists(d.cfg.AdminMacPath) {
				d.logger.Warnf(msg, "admin", d.cfg.AdminMacPath)
			}
			if lnrpc.FileExists(d.cfg.ReadMacPath) {
				d.logger.Warnf(msg, "readonly", d.cfg.ReadMacPath)
			}
			if lnrpc.FileExists(d.cfg.InvoiceMacPath) {
				d.logger.Warnf(msg, "invoice", d.cfg.InvoiceMacPath)
			}
		}

		// We add the macaroon service to our RPC interceptor. This
		// will start checking macaroons against permissions on every
		// RPC invocation.
		interceptorChain.AddMacaroonService(macaroonService)
	}

	// Now that the wallet password has been provided, transition the RPC
	// state into Unlocked.
	interceptorChain.SetWalletUnlocked()

	// Since calls to the WalletUnlocker service wait for a response on the
	// macaroon channel, we close it here to make sure they return in case
	// we did not return the admin macaroon above. This will be the case if
	// --no-macaroons is used.
	close(walletInitParams.MacResponseChan)

	// We'll also close all the macaroon channels since lnd is done sending
	// macaroon data over it.
	for _, lis := range grpcListeners {
		if lis.MacChan != nil {
			close(lis.MacChan)
		}
	}

	// With the information parsed from the configuration, create valid
	// instances of the pertinent interfaces required to operate the
	// Lightning Network Daemon.
	//
	// When we create the chain control, we need storage for the height
	// hints and also the wallet itself, for these two we want them to be
	// replicated, so we'll pass in the remote channel DB instance.
	chainControlCfg := &chainreg.Config{
		Bitcoin:                     d.cfg.Bitcoin,
		HeightHintCacheQueryDisable: d.cfg.HeightHintCacheQueryDisable,
		NeutrinoMode:                d.cfg.NeutrinoMode,
		BitcoindMode:                d.cfg.BitcoindMode,
		BtcdMode:                    d.cfg.BtcdMode,
		HeightHintDB:                dbs.HeightHintDB,
		ChanStateDB:                 dbs.ChanStateDB.ChannelStateDB(),
		NeutrinoCS:                  neutrinoCS,
		AuxLeafStore:                aux.AuxLeafStore,
		AuxSigner:                   aux.AuxSigner,
		ActiveNetParams:             d.cfg.ActiveNetParams,
		FeeURL:                      d.cfg.FeeURL,
		Fee: &lncfg.Fee{
			URL:              d.cfg.Fee.URL,
			MinUpdateTimeout: d.cfg.Fee.MinUpdateTimeout,
			MaxUpdateTimeout: d.cfg.Fee.MaxUpdateTimeout,
		},
		Dialer: func(addr string) (net.Conn, error) {
			return d.cfg.net.Dial(
				"tcp", addr, d.cfg.ConnectionTimeout,
			)
		},
		BlockCache:         blockCache,
		WalletUnlockParams: &walletInitParams,
	}

	// Let's go ahead and create the partial chain control now that is only
	// dependent on our configuration and doesn't require any wallet
	// specific information.
	partialChainControl, pccCleanup, err := chainreg.NewPartialChainControl(
		chainControlCfg,
	)
	cleanUpTasks = append(cleanUpTasks, pccCleanup)
	if err != nil {
		err := fmt.Errorf("unable to create partial chain control: %w",
			err)
		d.logger.Error(err)
		return nil, nil, nil, err
	}

	walletConfig := &btcwallet.Config{
		PrivatePass:      privateWalletPw,
		PublicPass:       publicWalletPw,
		Birthday:         walletInitParams.Birthday,
		RecoveryWindow:   walletInitParams.RecoveryWindow,
		NetParams:        d.cfg.ActiveNetParams.Params,
		CoinType:         d.cfg.ActiveNetParams.CoinType,
		Wallet:           walletInitParams.Wallet,
		LoaderOptions:    []btcwallet.LoaderOption{dbs.WalletDB},
		ChainSource:      partialChainControl.ChainSource,
		WatchOnly:        d.watchOnly,
		MigrateWatchOnly: d.migrateWatchOnly,
	}

	// Parse coin selection strategy.
	switch d.cfg.CoinSelectionStrategy {
	case "largest":
		walletConfig.CoinSelectionStrategy = wallet.CoinSelectionLargest

	case "random":
		walletConfig.CoinSelectionStrategy = wallet.CoinSelectionRandom

	default:
		return nil, nil, nil, fmt.Errorf("unknown coin selection "+
			"strategy %v", d.cfg.CoinSelectionStrategy)
	}

	earlyExit = false
	return partialChainControl, walletConfig, cleanUp, nil
}

// proxyBlockEpoch proxies a block epoch subsections to the underlying neutrino
// rebroadcaster client.
func proxyBlockEpoch(
	notifier chainntnfs.ChainNotifier) func() (*blockntfns.Subscription,
	error) {

	return func() (*blockntfns.Subscription, error) {
		blockEpoch, err := notifier.RegisterBlockEpochNtfn(
			nil,
		)
		if err != nil {
			return nil, err
		}

		sub := blockntfns.Subscription{
			Notifications: make(chan blockntfns.BlockNtfn, 6),
			Cancel:        blockEpoch.Cancel,
		}
		go func() {
			for blk := range blockEpoch.Epochs {
				ntfn := blockntfns.NewBlockConnected(
					*blk.BlockHeader,
					uint32(blk.Height),
				)

				sub.Notifications <- ntfn
			}
		}()

		return &sub, nil
	}
}

// walletReBroadcaster is a simple wrapper around the pushtx.Broadcaster
// interface to adhere to the expanded lnwallet.Rebroadcaster interface.
type walletReBroadcaster struct {
	started atomic.Bool

	*pushtx.Broadcaster
}

// newWalletReBroadcaster creates a new instance of the walletReBroadcaster.
func newWalletReBroadcaster(
	broadcaster *pushtx.Broadcaster) *walletReBroadcaster {

	return &walletReBroadcaster{
		Broadcaster: broadcaster,
	}
}

// Start launches all goroutines the rebroadcaster needs to operate.
func (w *walletReBroadcaster) Start() error {
	defer w.started.Store(true)

	return w.Broadcaster.Start()
}

// Started returns true if the broadcaster is already active.
func (w *walletReBroadcaster) Started() bool {
	return w.started.Load()
}

// BuildChainControl is responsible for creating a fully populated chain
// control instance from a wallet.
//
// NOTE: This is part of the ChainControlBuilder interface.
func (d *DefaultWalletImpl) BuildChainControl(
	partialChainControl *chainreg.PartialChainControl,
	walletConfig *btcwallet.Config) (*chainreg.ChainControl, func(), error) {

	walletController, err := btcwallet.New(
		*walletConfig, partialChainControl.Cfg.BlockCache,
	)
	if err != nil {
		err := fmt.Errorf("unable to create wallet controller: %w", err)
		d.logger.Error(err)
		return nil, nil, err
	}

	keyRing := keychain.NewBtcWalletKeyRing(
		walletController.InternalWallet(), walletConfig.CoinType,
	)

	// Create, and start the lnwallet, which handles the core payment
	// channel logic, and exposes control via proxy state machines.
	lnWalletConfig := lnwallet.Config{
		Database:              partialChainControl.Cfg.ChanStateDB,
		Notifier:              partialChainControl.ChainNotifier,
		WalletController:      walletController,
		Signer:                walletController,
		FeeEstimator:          partialChainControl.FeeEstimator,
		SecretKeyRing:         keyRing,
		ChainIO:               walletController,
		NetParams:             *walletConfig.NetParams,
		CoinSelectionStrategy: walletConfig.CoinSelectionStrategy,
		AuxLeafStore:          partialChainControl.Cfg.AuxLeafStore,
		AuxSigner:             partialChainControl.Cfg.AuxSigner,
	}

	// The broadcast is already always active for neutrino nodes, so we
	// don't want to create a rebroadcast loop.
	if partialChainControl.Cfg.NeutrinoCS == nil {
		cs := partialChainControl.ChainSource
		broadcastCfg := pushtx.Config{
			Broadcast: func(tx *wire.MsgTx) error {
				_, err := cs.SendRawTransaction(
					tx, true,
				)

				return err
			},
			SubscribeBlocks: proxyBlockEpoch(
				partialChainControl.ChainNotifier,
			),
			RebroadcastInterval: pushtx.DefaultRebroadcastInterval,
			// In case the backend is different from neutrino we
			// make sure that broadcast backend errors are mapped
			// to the neutrino broadcastErr.
			MapCustomBroadcastError: func(err error) error {
				rpcErr := cs.MapRPCErr(err)
				return broadcastErrorMapper(rpcErr)
			},
		}

		lnWalletConfig.Rebroadcaster = newWalletReBroadcaster(
			pushtx.NewBroadcaster(&broadcastCfg),
		)
	}

	// We've created the wallet configuration now, so we can finish
	// initializing the main chain control.
	activeChainControl, cleanUp, err := chainreg.NewChainControl(
		lnWalletConfig, walletController, partialChainControl,
	)
	if err != nil {
		err := fmt.Errorf("unable to create chain control: %w", err)
		d.logger.Error(err)
		return nil, nil, err
	}

	return activeChainControl, cleanUp, nil
}

// RPCSignerWalletImpl is a wallet implementation that uses a remote signer over
// an RPC interface.
type RPCSignerWalletImpl struct {
	// DefaultWalletImpl is the embedded instance of the default
	// implementation that the remote signer uses as its watch-only wallet
	// for keeping track of addresses and UTXOs.
	*DefaultWalletImpl
}

// NewRPCSignerWalletImpl creates a new instance of the remote signing wallet
// implementation.
func NewRPCSignerWalletImpl(cfg *Config, logger btclog.Logger,
	interceptor signal.Interceptor,
	migrateWatchOnly bool) *RPCSignerWalletImpl {

	return &RPCSignerWalletImpl{
		DefaultWalletImpl: &DefaultWalletImpl{
			cfg:              cfg,
			logger:           logger,
			interceptor:      interceptor,
			watchOnly:        true,
			migrateWatchOnly: migrateWatchOnly,
			pwService:        createWalletUnlockerService(cfg),
		},
	}
}

// BuildChainControl is responsible for creating or unlocking and then fully
// initializing a wallet and returning it as part of a fully populated chain
// control instance.
//
// NOTE: This is part of the ChainControlBuilder interface.
func (d *RPCSignerWalletImpl) BuildChainControl(
	partialChainControl *chainreg.PartialChainControl,
	walletConfig *btcwallet.Config) (*chainreg.ChainControl, func(), error) {

	walletController, err := btcwallet.New(
		*walletConfig, partialChainControl.Cfg.BlockCache,
	)
	if err != nil {
		err := fmt.Errorf("unable to create wallet controller: %w", err)
		d.logger.Error(err)
		return nil, nil, err
	}

	baseKeyRing := keychain.NewBtcWalletKeyRing(
		walletController.InternalWallet(), walletConfig.CoinType,
	)

	rpcKeyRing, err := rpcwallet.NewRPCKeyRing(
		baseKeyRing, walletController,
		d.DefaultWalletImpl.cfg.RemoteSigner, walletConfig.NetParams,
	)
	if err != nil {
		err := fmt.Errorf("unable to create RPC remote signing wallet "+
			"%v", err)
		d.logger.Error(err)
		return nil, nil, err
	}

	// Create, and start the lnwallet, which handles the core payment
	// channel logic, and exposes control via proxy state machines.
	lnWalletConfig := lnwallet.Config{
		Database:              partialChainControl.Cfg.ChanStateDB,
		Notifier:              partialChainControl.ChainNotifier,
		WalletController:      rpcKeyRing,
		Signer:                rpcKeyRing,
		FeeEstimator:          partialChainControl.FeeEstimator,
		SecretKeyRing:         rpcKeyRing,
		ChainIO:               walletController,
		NetParams:             *walletConfig.NetParams,
		CoinSelectionStrategy: walletConfig.CoinSelectionStrategy,
	}

	// We've created the wallet configuration now, so we can finish
	// initializing the main chain control.
	activeChainControl, cleanUp, err := chainreg.NewChainControl(
		lnWalletConfig, rpcKeyRing, partialChainControl,
	)
	if err != nil {
		err := fmt.Errorf("unable to create chain control: %w", err)
		d.logger.Error(err)
		return nil, nil, err
	}

	return activeChainControl, cleanUp, nil
}

// DatabaseInstances is a struct that holds all instances to the actual
// databases that are used in lnd.
type DatabaseInstances struct {
	// GraphDB is the database that stores the channel graph used for path
	// finding.
	GraphDB *graphdb.ChannelGraph

	// ChanStateDB is the database that stores all of our node's channel
	// state.
	ChanStateDB *channeldb.DB

	// HeightHintDB is the database that stores height hints for spends.
	HeightHintDB kvdb.Backend

	// InvoiceDB is the database that stores information about invoices.
	InvoiceDB invoices.InvoiceDB

	// PaymentsDB is the database that stores all payment related
	// information.
	PaymentsDB paymentsdb.DB

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

	// NativeSQLStore holds a reference to the native SQL store that can
	// be used for native SQL queries for tables that already support it.
	// This may be nil if the use-native-sql flag was not set.
	NativeSQLStore sqldb.DB
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
			cfg.Watchtower.TowerDir, BitcoinChainName,
			lncfg.NormalizeNetwork(cfg.ActiveNetParams.Name),
		), cfg.WtClient.Active, cfg.Watchtower.Active, d.logger,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("unable to obtain database "+
			"backends: %v", err)
	}

	// With the full remote mode we made sure both the graph and channel
	// state DB point to the same local or remote DB and the same namespace
	// within that DB.
	dbs := &DatabaseInstances{
		HeightHintDB:   databaseBackends.HeightHintDB,
		MacaroonDB:     databaseBackends.MacaroonDB,
		DecayedLogDB:   databaseBackends.DecayedLogDB,
		WalletDB:       databaseBackends.WalletDB,
		NativeSQLStore: databaseBackends.NativeSQLStore,
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

	graphDBOptions := []graphdb.StoreOptionModifier{
		graphdb.WithRejectCacheSize(cfg.Caches.RejectCacheSize),
		graphdb.WithChannelCacheSize(cfg.Caches.ChannelCacheSize),
		graphdb.WithBatchCommitInterval(cfg.DB.BatchCommitInterval),
	}

	chanGraphOpts := []graphdb.ChanGraphOption{
		graphdb.WithUseGraphCache(!cfg.DB.NoGraphCache),
	}

	// We want to pre-allocate the channel graph cache according to what we
	// expect for mainnet to speed up memory allocation.
	if cfg.ActiveNetParams.Name == chaincfg.MainNetParams.Name {
		chanGraphOpts = append(
			chanGraphOpts, graphdb.WithPreAllocCacheNumNodes(
				graphdb.DefaultPreAllocCacheNumNodes,
			),
		)
	}

	dbOptions := []channeldb.OptionModifier{
		channeldb.OptionDryRunMigration(cfg.DryRunMigration),
		channeldb.OptionStoreFinalHtlcResolutions(
			cfg.StoreFinalHtlcResolutions,
		),
		channeldb.OptionPruneRevocationLog(cfg.DB.PruneRevocation),
		channeldb.OptionNoRevLogAmtData(cfg.DB.NoRevLogAmtData),
		channeldb.OptionGcDecayedLog(cfg.DB.NoGcDecayedLog),
		channeldb.OptionWithDecayedLogDB(dbs.DecayedLogDB),
	}

	// Otherwise, we'll open two instances, one for the state we only need
	// locally, and the other for things we want to ensure are replicated.
	dbs.ChanStateDB, err = channeldb.CreateWithBackend(
		databaseBackends.ChanStateDB, dbOptions...,
	)
	switch {
	// Give the DB a chance to dry run the migration. Since we know that
	// both the channel state and graph DBs are still always behind the same
	// backend, we know this would be applied to both of those DBs.
	case err == channeldb.ErrDryRunMigrationOK:
		d.logger.Infof("Channel DB dry run migration successful")
		return nil, nil, err

	case err != nil:
		cleanUp()

		err = fmt.Errorf("unable to open graph DB: %w", err)
		d.logger.Error(err)
		return nil, nil, err
	}

	// The graph store implementation we will use depends on whether
	// native SQL is enabled or not.
	var graphStore graphdb.V1Store

	// Instantiate a native SQL store if the flag is set.
	if d.cfg.DB.UseNativeSQL {
		migrations := sqldb.GetMigrations()

		queryCfg := &d.cfg.DB.Sqlite.QueryConfig
		if d.cfg.DB.Backend == lncfg.PostgresBackend {
			queryCfg = &d.cfg.DB.Postgres.QueryConfig
		}

		// If the user has not explicitly disabled the SQL invoice
		// migration, attach the custom migration function to invoice
		// migration (version 7). Even if this custom migration is
		// disabled, the regular native SQL store migrations will still
		// run. If the database version is already above this custom
		// migration's version (7), it will be skipped permanently,
		// regardless of the flag.
		if !d.cfg.DB.SkipNativeSQLMigration {
			invoiceMig := func(tx *sqlc.Queries) error {
				err := invoices.MigrateInvoicesToSQL(
					ctx, dbs.ChanStateDB.Backend,
					dbs.ChanStateDB, tx,
					invoiceMigrationBatchSize,
				)
				if err != nil {
					return fmt.Errorf("failed to migrate "+
						"invoices to SQL: %w", err)
				}

				// Set the invoice bucket tombstone to indicate
				// that the migration has been completed.
				d.logger.Debugf("Setting invoice bucket " +
					"tombstone")

				//nolint:ll
				return dbs.ChanStateDB.SetInvoiceBucketTombstone()
			}

			graphMig := func(tx *sqlc.Queries) error {
				cfg := &graphdbmig1.SQLStoreConfig{
					//nolint:ll
					ChainHash: *d.cfg.ActiveNetParams.GenesisHash,
					QueryCfg:  queryCfg,
				}
				err := graphdbmig1.MigrateGraphToSQL(
					ctx, cfg, dbs.ChanStateDB.Backend,
					graphmig1sqlc.New(tx.GetTx()),
				)
				if err != nil {
					return fmt.Errorf("failed to migrate "+
						"graph to SQL: %w", err)
				}

				return nil
			}

			// Make sure we attach the custom migration function to
			// the correct migration version.
			for i := 0; i < len(migrations); i++ {
				version := migrations[i].Version
				switch version {
				case invoiceMigration:
					migrations[i].MigrationFn = invoiceMig

					continue
				case graphMigration:
					migrations[i].MigrationFn = graphMig

					continue

				default:
				}

				migFn, ok := d.getSQLMigration(
					ctx, version, dbs.ChanStateDB.Backend,
				)
				if !ok {
					continue
				}

				migrations[i].MigrationFn = migFn
			}
		}

		// We need to apply all migrations to the native SQL store
		// before we can use it.
		err = dbs.NativeSQLStore.ApplyAllMigrations(ctx, migrations)
		if err != nil {
			cleanUp()
			err = fmt.Errorf("faild to run migrations for the "+
				"native SQL store: %w", err)
			d.logger.Error(err)

			return nil, nil, err
		}

		// With the DB ready and migrations applied, we can now create
		// the base DB and transaction executor for the native SQL
		// invoice store.
		baseDB := dbs.NativeSQLStore.GetBaseDB()
		invoiceExecutor := sqldb.NewTransactionExecutor(
			baseDB, func(tx *sql.Tx) invoices.SQLInvoiceQueries {
				return baseDB.WithTx(tx)
			},
		)

		sqlInvoiceDB := invoices.NewSQLStore(
			invoiceExecutor, clock.NewDefaultClock(),
		)

		dbs.InvoiceDB = sqlInvoiceDB

		graphExecutor := sqldb.NewTransactionExecutor(
			baseDB, func(tx *sql.Tx) graphdb.SQLQueries {
				return baseDB.WithTx(tx)
			},
		)

		graphStore, err = graphdb.NewSQLStore(
			&graphdb.SQLStoreConfig{
				ChainHash: *d.cfg.ActiveNetParams.GenesisHash,
				QueryCfg:  queryCfg,
			},
			graphExecutor, graphDBOptions...,
		)
		if err != nil {
			err = fmt.Errorf("unable to get graph store: %w", err)
			d.logger.Error(err)

			return nil, nil, err
		}

		// Create the payments DB.
		//
		// NOTE:  In the regular build, this will construct a kvdb
		// backed payments backend. With the test_native_sql tag, it
		// will build a SQL payments backend.
		sqlPaymentsDB, err := d.getPaymentsStore(
			baseDB, dbs.ChanStateDB.Backend,
		)
		if err != nil {
			err = fmt.Errorf("unable to get payments store: %w",
				err)

			return nil, nil, err
		}

		dbs.PaymentsDB = sqlPaymentsDB
	} else {
		// Check if the invoice bucket tombstone is set. If it is, we
		// need to return and ask the user switch back to using the
		// native SQL store.
		ripInvoices, err := dbs.ChanStateDB.GetInvoiceBucketTombstone()
		if err != nil {
			err = fmt.Errorf("unable to check invoice bucket "+
				"tombstone: %w", err)
			d.logger.Error(err)

			return nil, nil, err
		}
		if ripInvoices {
			err = fmt.Errorf("invoices bucket tombstoned, please " +
				"switch back to native SQL")
			d.logger.Error(err)

			return nil, nil, err
		}

		dbs.InvoiceDB = dbs.ChanStateDB

		graphStore, err = graphdb.NewKVStore(
			databaseBackends.GraphDB, graphDBOptions...,
		)
		if err != nil {
			return nil, nil, err
		}

		// Create the payments DB.
		kvPaymentsDB, err := paymentsdb.NewKVStore(
			dbs.ChanStateDB,
		)
		if err != nil {
			cleanUp()

			err = fmt.Errorf("unable to open payments DB: %w", err)
			d.logger.Error(err)

			return nil, nil, err
		}

		dbs.PaymentsDB = kvPaymentsDB
	}

	dbs.GraphDB, err = graphdb.NewChannelGraph(graphStore, chanGraphOpts...)
	if err != nil {
		cleanUp()

		err = fmt.Errorf("unable to open channel graph DB: %w", err)
		d.logger.Error(err)

		return nil, nil, err
	}

	// Wrap the watchtower client DB and make sure we clean up.
	if cfg.WtClient.Active {
		dbs.TowerClientDB, err = wtdb.OpenClientDB(
			databaseBackends.TowerClientDB,
		)
		if err != nil {
			cleanUp()

			err = fmt.Errorf("unable to open %s database: %w",
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

			err = fmt.Errorf("unable to open %s database: %w",
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
		watchOnlyAccounts := initMsg.WatchOnlyAccounts
		recoveryWindow := initMsg.RecoveryWindow

		// Before we proceed, we'll check the internal version of the
		// seed. If it's greater than the current key derivation
		// version, then we'll return an error as we don't understand
		// this.
		if cipherSeed != nil &&
			!keychain.IsKnownVersion(cipherSeed.InternalVersion) {

			return nil, fmt.Errorf("invalid internal "+
				"seed version %v, current max version is %v",
				cipherSeed.InternalVersion,
				keychain.CurrentKeyDerivationVersion)
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

		// Neither seed nor extended private key was given, so maybe the
		// third option was chosen, the watch-only initialization. In
		// this case we need to import each of the xpubs individually.
		case watchOnlyAccounts != nil:
			if !cfg.RemoteSigner.Enable {
				return nil, fmt.Errorf("cannot initialize " +
					"watch only wallet with remote " +
					"signer config disabled")
			}

			birthday = initMsg.WatchOnlyBirthday
			newWallet, err = loader.CreateNewWatchingOnlyWallet(
				password, birthday,
			)
			if err != nil {
				break
			}

			err = importWatchOnlyAccounts(newWallet, initMsg)

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
			MacRootKey:      initMsg.MacRootKey,
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

// importWatchOnlyAccounts imports all individual account xpubs into our wallet
// which we created as watch-only.
func importWatchOnlyAccounts(wallet *wallet.Wallet,
	initMsg *walletunlocker.WalletInitMsg) error {

	scopes := make([]waddrmgr.ScopedIndex, 0, len(initMsg.WatchOnlyAccounts))
	for scope := range initMsg.WatchOnlyAccounts {
		scopes = append(scopes, scope)
	}

	// We need to import the accounts in the correct order, otherwise the
	// indices will be incorrect.
	sort.Slice(scopes, func(i, j int) bool {
		return scopes[i].Scope.Purpose < scopes[j].Scope.Purpose ||
			scopes[i].Index < scopes[j].Index
	})

	for _, scope := range scopes {
		addrSchema := waddrmgr.ScopeAddrMap[waddrmgr.KeyScopeBIP0084]

		// We want witness pubkey hash by default, except for BIP49
		// where we want mixed and BIP86 where we want taproot address
		// formats.
		switch scope.Scope.Purpose {
		case waddrmgr.KeyScopeBIP0049Plus.Purpose,
			waddrmgr.KeyScopeBIP0086.Purpose:

			addrSchema = waddrmgr.ScopeAddrMap[scope.Scope]
		}

		// We want a human-readable account name. But for the default
		// on-chain wallet we actually need to call it "default" to make
		// sure everything works correctly.
		name := fmt.Sprintf("%s/%d'", scope.Scope.String(), scope.Index)
		if scope.Index == 0 {
			name = "default"
		}

		_, err := wallet.ImportAccountWithScope(
			name, initMsg.WatchOnlyAccounts[scope],
			initMsg.WatchOnlyMasterFingerprint, scope.Scope,
			addrSchema,
		)
		if err != nil {
			return fmt.Errorf("could not import account %v: %w",
				name, err)
		}
	}

	return nil
}

// handleNeutrinoPostgresDBMigration handles the migration of the neutrino db
// to postgres. Initially we kept the neutrino db in the bolt db when running
// with kvdb postgres backend. Now now move it to postgres as well. However we
// need to make a distinction whether the user migrated the neutrino db to
// postgres via lndinit or not. Currently if the db is not migrated we start
// with a fresh db in postgres.
//
// TODO(ziggie): Also migrate the db to postgres in case it is still not
// migrated ?
func handleNeutrinoPostgresDBMigration(dbName, dbPath string,
	cfg *Config) error {

	if !lnrpc.FileExists(dbName) {
		return nil
	}

	// Open bolt db to check if it is tombstoned. If it is we assume that
	// the neutrino db was successfully migrated to postgres. We open it
	// in read-only mode to avoid long db open times.
	boltDB, err := kvdb.Open(
		kvdb.BoltBackendName, dbName, true,
		cfg.DB.Bolt.DBTimeout, true,
	)
	if err != nil {
		return fmt.Errorf("failed to open bolt db: %w", err)
	}
	defer boltDB.Close()

	isTombstoned := false
	err = boltDB.View(func(tx kvdb.RTx) error {
		_, err = channeldb.CheckMarkerPresent(
			tx, channeldb.TombstoneKey,
		)

		return err
	}, func() {})
	if err == nil {
		isTombstoned = true
	}

	if isTombstoned {
		ltndLog.Infof("Neutrino Bolt DB is tombstoned, assuming " +
			"database was successfully migrated to postgres")

		return nil
	}

	// If the db is not tombstoned, we remove the files and start fresh with
	// postgres. This is the case when a user was running lnd with the
	// postgres backend from the beginning without migrating from bolt.
	ltndLog.Infof("Neutrino Bolt DB found but NOT tombstoned, removing " +
		"it and starting fresh with postgres")

	filesToRemove := []string{
		filepath.Join(dbPath, "block_headers.bin"),
		filepath.Join(dbPath, "reg_filter_headers.bin"),
		dbName,
	}

	for _, file := range filesToRemove {
		if err := os.Remove(file); err != nil {
			ltndLog.Warnf("Could not remove %s: %v", file, err)
		}
	}

	return nil
}

// initNeutrinoBackend inits a new instance of the neutrino light client
// backend given a target chain directory to store the chain state.
func initNeutrinoBackend(ctx context.Context, cfg *Config, chainDir string,
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

	var (
		db  walletdb.DB
		err error
	)
	switch {
	case cfg.DB.Backend == kvdb.SqliteBackendName:
		sqliteConfig := lncfg.GetSqliteConfigKVDB(cfg.DB.Sqlite)
		db, err = kvdb.Open(
			kvdb.SqliteBackendName, ctx, sqliteConfig, dbPath,
			lncfg.SqliteNeutrinoDBName, lncfg.NSNeutrinoDB,
		)

	case cfg.DB.Backend == kvdb.PostgresBackendName:
		dbName := filepath.Join(dbPath, lncfg.NeutrinoDBName)

		// This code needs to be in place because we did not start
		// the postgres backend for neutrino at the beginning. Now we
		// are also moving it into the postgres backend so we can phase
		// out the bolt backend.
		err = handleNeutrinoPostgresDBMigration(dbName, dbPath, cfg)
		if err != nil {
			return nil, nil, err
		}

		postgresConfig := lncfg.GetPostgresConfigKVDB(cfg.DB.Postgres)
		db, err = kvdb.Open(
			kvdb.PostgresBackendName, ctx, postgresConfig,
			lncfg.NSNeutrinoDB,
		)

	default:
		dbName := filepath.Join(dbPath, lncfg.NeutrinoDBName)
		db, err = walletdb.Create(
			kvdb.BoltBackendName, dbName, !cfg.SyncFreelist,
			cfg.DB.Bolt.DBTimeout, false,
		)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("unable to create "+
			"neutrino database: %v", err)
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
			return cfg.net.Dial(
				addr.Network(), addr.String(),
				cfg.ConnectionTimeout,
			)
		},
		NameResolver: func(host string) ([]net.IP, error) {
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

	if cfg.NeutrinoMode.MaxPeers <= 0 {
		return nil, nil, fmt.Errorf("a non-zero number must be set " +
			"for neutrino max peers")
	}
	neutrino.MaxPeers = cfg.NeutrinoMode.MaxPeers
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
			ltndLog.Infof("Unable to stop neutrino light client: "+
				"%v", err)
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
		return nil, fmt.Errorf("invalid filter header height: %w", err)
	}

	hash, err := chainhash.NewHashFromStr(split[1])
	if err != nil {
		return nil, fmt.Errorf("invalid filter header hash: %w", err)
	}

	return &headerfs.FilterHeader{
		Height:     uint32(height),
		FilterHash: *hash,
	}, nil
}

// broadcastErrorMapper maps errors from bitcoin backends other than neutrino to
// the neutrino BroadcastError which allows the Rebroadcaster which currently
// resides in the neutrino package to use all of its functionalities.
func broadcastErrorMapper(err error) error {
	var returnErr error

	// We only filter for specific backend errors which are relevant for the
	// Rebroadcaster.
	switch {
	// This makes sure the tx is removed from the rebroadcaster once it is
	// confirmed.
	case errors.Is(err, chain.ErrTxAlreadyKnown),
		errors.Is(err, chain.ErrTxAlreadyConfirmed):

		returnErr = &pushtx.BroadcastError{
			Code:   pushtx.Confirmed,
			Reason: err.Error(),
		}

	// Transactions which are still in mempool but might fall out because
	// of low fees are rebroadcasted despite of their backend error.
	case errors.Is(err, chain.ErrTxAlreadyInMempool):
		returnErr = &pushtx.BroadcastError{
			Code:   pushtx.Mempool,
			Reason: err.Error(),
		}

	// Transactions which are not accepted into mempool because of low fees
	// in the first place are rebroadcasted despite of their backend error.
	// Mempool conditions change over time so it makes sense to retry
	// publishing the transaction. Moreover we log the detailed error so the
	// user can intervene and increase the size of his mempool or increase
	// his min relay fee configuration.
	case errors.Is(err, chain.ErrMempoolMinFeeNotMet),
		errors.Is(err, chain.ErrMinRelayFeeNotMet):

		ltndLog.Warnf("Error while broadcasting transaction: %v", err)

		returnErr = &pushtx.BroadcastError{
			Code:   pushtx.Mempool,
			Reason: err.Error(),
		}
	}

	return returnErr
}
