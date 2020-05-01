// Copyright (c) 2013-2017 The btcsuite developers
// Copyright (c) 2015-2016 The Decred developers
// Copyright (C) 2015-2017 The Lightning Network Developers

package lnd

import (
	"context"
	"crypto/tls"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime/pprof"
	"strings"
	"sync"
	"time"

	// Blank import to set up profiling HTTP handlers.
	_ "net/http/pprof"

	"gopkg.in/macaroon-bakery.v2/bakery"
	"gopkg.in/macaroon.v2"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcutil"
	"github.com/btcsuite/btcwallet/wallet"
	proxy "github.com/grpc-ecosystem/grpc-gateway/runtime"
	"github.com/lightninglabs/neutrino"

	"github.com/lightningnetwork/lnd/autopilot"
	"github.com/lightningnetwork/lnd/build"
	"github.com/lightningnetwork/lnd/cert"
	"github.com/lightningnetwork/lnd/chanacceptor"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/btcwallet"
	"github.com/lightningnetwork/lnd/macaroons"
	"github.com/lightningnetwork/lnd/signal"
	"github.com/lightningnetwork/lnd/tor"
	"github.com/lightningnetwork/lnd/walletunlocker"
	"github.com/lightningnetwork/lnd/watchtower"
	"github.com/lightningnetwork/lnd/watchtower/wtdb"
)

var (
	cfg              *config
	registeredChains = newChainRegistry()

	// networkDir is the path to the directory of the currently active
	// network. This path will hold the files related to each different
	// network.
	networkDir string
)

// WalletUnlockerAuthOptions returns a list of DialOptions that can be used to
// authenticate with the wallet unlocker service.
//
// NOTE: This should only be called after the WalletUnlocker listener has
// signaled it is ready.
func WalletUnlockerAuthOptions() ([]grpc.DialOption, error) {
	creds, err := credentials.NewClientTLSFromFile(cfg.TLSCertPath, "")
	if err != nil {
		return nil, fmt.Errorf("unable to read TLS cert: %v", err)
	}

	// Create a dial options array with the TLS credentials.
	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(creds),
	}

	return opts, nil
}

// AdminAuthOptions returns a list of DialOptions that can be used to
// authenticate with the RPC server with admin capabilities.
//
// NOTE: This should only be called after the RPCListener has signaled it is
// ready.
func AdminAuthOptions() ([]grpc.DialOption, error) {
	creds, err := credentials.NewClientTLSFromFile(cfg.TLSCertPath, "")
	if err != nil {
		return nil, fmt.Errorf("unable to read TLS cert: %v", err)
	}

	// Create a dial options array.
	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(creds),
	}

	// Get the admin macaroon if macaroons are active.
	if !cfg.NoMacaroons {
		// Load the adming macaroon file.
		macBytes, err := ioutil.ReadFile(cfg.AdminMacPath)
		if err != nil {
			return nil, fmt.Errorf("unable to read macaroon "+
				"path (check the network setting!): %v", err)
		}

		mac := &macaroon.Macaroon{}
		if err = mac.UnmarshalBinary(macBytes); err != nil {
			return nil, fmt.Errorf("unable to decode macaroon: %v",
				err)
		}

		// Now we append the macaroon credentials to the dial options.
		cred := macaroons.NewMacaroonCredential(mac)
		opts = append(opts, grpc.WithPerRPCCredentials(cred))
	}

	return opts, nil
}

// ListenerWithSignal is a net.Listener that has an additional Ready channel that
// will be closed when a server starts listening.
type ListenerWithSignal struct {
	net.Listener

	// Ready will be closed by the server listening on Listener.
	Ready chan struct{}
}

// ListenerCfg is a wrapper around custom listeners that can be passed to lnd
// when calling its main method.
type ListenerCfg struct {
	// WalletUnlocker can be set to the listener to use for the wallet
	// unlocker. If nil a regular network listener will be created.
	WalletUnlocker *ListenerWithSignal

	// RPCListener can be set to the listener to use for the RPC server. If
	// nil a regular network listener will be created.
	RPCListener *ListenerWithSignal
}

// rpcListeners is a function type used for closures that fetches a set of RPC
// listeners for the current configuration. If no custom listeners are present,
// this should return normal listeners from the RPC endpoints defined in the
// config. The second return value us a closure that will close the fetched
// listeners.
type rpcListeners func() ([]*ListenerWithSignal, func(), error)

// Main is the true entry point for lnd. This function is required since defers
// created in the top-level scope of a main method aren't executed if os.Exit()
// is called.
func Main(lisCfg ListenerCfg) error {
	// Hook interceptor for os signals.
	signal.Intercept()

	// Load the configuration, and parse any command line options. This
	// function will also set up logging properly.
	loadedConfig, err := loadConfig()
	if err != nil {
		return err
	}
	cfg = loadedConfig
	defer func() {
		ltndLog.Info("Shutdown complete")
		err := logWriter.Close()
		if err != nil {
			ltndLog.Errorf("Could not close log rotator: %v", err)
		}
	}()

	// Show version at startup.
	ltndLog.Infof("Version: %s commit=%s, build=%s, logging=%s",
		build.Version(), build.Commit, build.Deployment,
		build.LoggingType)

	var network string
	switch {
	case cfg.Bitcoin.TestNet3 || cfg.Litecoin.TestNet3:
		network = "testnet"

	case cfg.Bitcoin.MainNet || cfg.Litecoin.MainNet:
		network = "mainnet"

	case cfg.Bitcoin.SimNet || cfg.Litecoin.SimNet:
		network = "simnet"

	case cfg.Bitcoin.RegTest || cfg.Litecoin.RegTest:
		network = "regtest"
	}

	ltndLog.Infof("Active chain: %v (network=%v)",
		strings.Title(registeredChains.PrimaryChain().String()),
		network,
	)

	// Enable http profiling server if requested.
	if cfg.Profile != "" {
		go func() {
			listenAddr := net.JoinHostPort("", cfg.Profile)
			profileRedirect := http.RedirectHandler("/debug/pprof",
				http.StatusSeeOther)
			http.Handle("/", profileRedirect)
			fmt.Println(http.ListenAndServe(listenAddr, nil))
		}()
	}

	// Write cpu profile if requested.
	if cfg.CPUProfile != "" {
		f, err := os.Create(cfg.CPUProfile)
		if err != nil {
			err := fmt.Errorf("unable to create CPU profile: %v",
				err)
			ltndLog.Error(err)
			return err
		}
		pprof.StartCPUProfile(f)
		defer f.Close()
		defer pprof.StopCPUProfile()
	}

	// Create the network-segmented directory for the channel database.
	graphDir := filepath.Join(cfg.DataDir,
		defaultGraphSubDirname,
		normalizeNetwork(activeNetParams.Name))

	ltndLog.Infof("Opening the main database, this might take a few " +
		"minutes...")

	// Open the channeldb, which is dedicated to storing channel, and
	// network related metadata.
	startOpenTime := time.Now()
	chanDB, err := channeldb.Open(
		graphDir,
		channeldb.OptionSetRejectCacheSize(cfg.Caches.RejectCacheSize),
		channeldb.OptionSetChannelCacheSize(cfg.Caches.ChannelCacheSize),
		channeldb.OptionSetSyncFreelist(cfg.SyncFreelist),
	)
	if err != nil {
		err := fmt.Errorf("unable to open channeldb: %v", err)
		ltndLog.Error(err)
		return err
	}
	defer chanDB.Close()

	openTime := time.Since(startOpenTime)
	ltndLog.Infof("Database now open (time_to_open=%v)!", openTime)

	// Only process macaroons if --no-macaroons isn't set.
	ctx := context.Background()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	tlsCfg, restCreds, restProxyDest, err := getTLSConfig(
		cfg.TLSCertPath, cfg.TLSKeyPath, cfg.TLSExtraIPs,
		cfg.TLSExtraDomains, cfg.RPCListeners,
	)
	if err != nil {
		err := fmt.Errorf("unable to load TLS credentials: %v", err)
		ltndLog.Error(err)
		return err
	}

	serverCreds := credentials.NewTLS(tlsCfg)
	serverOpts := []grpc.ServerOption{grpc.Creds(serverCreds)}

	// For our REST dial options, we'll still use TLS, but also increase
	// the max message size that we'll decode to allow clients to hit
	// endpoints which return more data such as the DescribeGraph call.
	// We set this to 200MiB atm. Should be the same value as maxMsgRecvSize
	// in cmd/lncli/main.go.
	restDialOpts := []grpc.DialOption{
		grpc.WithTransportCredentials(*restCreds),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(1 * 1024 * 1024 * 200),
		),
	}

	// Before starting the wallet, we'll create and start our Neutrino
	// light client instance, if enabled, in order to allow it to sync
	// while the rest of the daemon continues startup.
	mainChain := cfg.Bitcoin
	if registeredChains.PrimaryChain() == litecoinChain {
		mainChain = cfg.Litecoin
	}
	var neutrinoCS *neutrino.ChainService
	if mainChain.Node == "neutrino" {
		neutrinoBackend, neutrinoCleanUp, err := initNeutrinoBackend(
			mainChain.ChainDir,
		)
		if err != nil {
			err := fmt.Errorf("unable to initialize neutrino "+
				"backend: %v", err)
			ltndLog.Error(err)
			return err
		}
		defer neutrinoCleanUp()
		neutrinoCS = neutrinoBackend
	}

	var (
		walletInitParams WalletUnlockParams
		privateWalletPw  = lnwallet.DefaultPrivatePassphrase
		publicWalletPw   = lnwallet.DefaultPublicPassphrase
	)

	// If the user didn't request a seed, then we'll manually assume a
	// wallet birthday of now, as otherwise the seed would've specified
	// this information.
	walletInitParams.Birthday = time.Now()

	// getListeners is a closure that creates listeners from the
	// RPCListeners defined in the config. It also returns a cleanup
	// closure and the server options to use for the GRPC server.
	getListeners := func() ([]*ListenerWithSignal, func(), error) {
		var grpcListeners []*ListenerWithSignal
		for _, grpcEndpoint := range cfg.RPCListeners {
			// Start a gRPC server listening for HTTP/2
			// connections.
			lis, err := lncfg.ListenOnAddress(grpcEndpoint)
			if err != nil {
				ltndLog.Errorf("unable to listen on %s",
					grpcEndpoint)
				return nil, nil, err
			}
			grpcListeners = append(
				grpcListeners, &ListenerWithSignal{
					Listener: lis,
					Ready:    make(chan struct{}),
				})
		}

		cleanup := func() {
			for _, lis := range grpcListeners {
				lis.Close()
			}
		}
		return grpcListeners, cleanup, nil
	}

	// walletUnlockerListeners is a closure we'll hand to the wallet
	// unlocker, that will be called when it needs listeners for its GPRC
	// server.
	walletUnlockerListeners := func() ([]*ListenerWithSignal, func(),
		error) {

		// If we have chosen to start with a dedicated listener for the
		// wallet unlocker, we return it directly.
		if lisCfg.WalletUnlocker != nil {
			return []*ListenerWithSignal{lisCfg.WalletUnlocker},
				func() {}, nil
		}

		// Otherwise we'll return the regular listeners.
		return getListeners()
	}

	// We wait until the user provides a password over RPC. In case lnd is
	// started with the --noseedbackup flag, we use the default password
	// for wallet encryption.
	if !cfg.NoSeedBackup {
		params, err := waitForWalletPassword(
			cfg.RESTListeners, serverOpts, restDialOpts,
			restProxyDest, tlsCfg, walletUnlockerListeners,
		)
		if err != nil {
			err := fmt.Errorf("unable to set up wallet password "+
				"listeners: %v", err)
			ltndLog.Error(err)
			return err
		}

		walletInitParams = *params
		privateWalletPw = walletInitParams.Password
		publicWalletPw = walletInitParams.Password

		if walletInitParams.RecoveryWindow > 0 {
			ltndLog.Infof("Wallet recovery mode enabled with "+
				"address lookahead of %d addresses",
				walletInitParams.RecoveryWindow)
		}
	}

	var macaroonService *macaroons.Service
	if !cfg.NoMacaroons {
		// Create the macaroon authentication/authorization service.
		macaroonService, err = macaroons.NewService(
			networkDir, macaroons.IPLockChecker,
		)
		if err != nil {
			err := fmt.Errorf("unable to set up macaroon "+
				"authentication: %v", err)
			ltndLog.Error(err)
			return err
		}
		defer macaroonService.Close()

		// Try to unlock the macaroon store with the private password.
		err = macaroonService.CreateUnlock(&privateWalletPw)
		if err != nil {
			err := fmt.Errorf("unable to unlock macaroons: %v", err)
			ltndLog.Error(err)
			return err
		}

		// Create macaroon files for lncli to use if they don't exist.
		if !fileExists(cfg.AdminMacPath) && !fileExists(cfg.ReadMacPath) &&
			!fileExists(cfg.InvoiceMacPath) {

			err = genMacaroons(
				ctx, macaroonService, cfg.AdminMacPath,
				cfg.ReadMacPath, cfg.InvoiceMacPath,
			)
			if err != nil {
				err := fmt.Errorf("unable to create macaroons "+
					"%v", err)
				ltndLog.Error(err)
				return err
			}
		}
	}

	// With the information parsed from the configuration, create valid
	// instances of the pertinent interfaces required to operate the
	// Lightning Network Daemon.
	activeChainControl, err := newChainControlFromConfig(
		cfg, chanDB, privateWalletPw, publicWalletPw,
		walletInitParams.Birthday, walletInitParams.RecoveryWindow,
		walletInitParams.Wallet, neutrinoCS,
	)
	if err != nil {
		err := fmt.Errorf("unable to create chain control: %v", err)
		ltndLog.Error(err)
		return err
	}

	// Finally before we start the server, we'll register the "holy
	// trinity" of interface for our current "home chain" with the active
	// chainRegistry interface.
	primaryChain := registeredChains.PrimaryChain()
	registeredChains.RegisterChain(primaryChain, activeChainControl)

	// TODO(roasbeef): add rotation
	idPrivKey, err := activeChainControl.wallet.DerivePrivKey(keychain.KeyDescriptor{
		KeyLocator: keychain.KeyLocator{
			Family: keychain.KeyFamilyNodeKey,
			Index:  0,
		},
	})
	if err != nil {
		err := fmt.Errorf("unable to derive node private key: %v", err)
		ltndLog.Error(err)
		return err
	}
	idPrivKey.Curve = btcec.S256()

	if cfg.Tor.Active {
		srvrLog.Infof("Proxying all network traffic via Tor "+
			"(stream_isolation=%v)! NOTE: Ensure the backend node "+
			"is proxying over Tor as well", cfg.Tor.StreamIsolation)
	}

	// If the watchtower client should be active, open the client database.
	// This is done here so that Close always executes when lndMain returns.
	var towerClientDB *wtdb.ClientDB
	if cfg.WtClient.Active {
		var err error
		towerClientDB, err = wtdb.OpenClientDB(graphDir)
		if err != nil {
			err := fmt.Errorf("unable to open watchtower client "+
				"database: %v", err)
			ltndLog.Error(err)
			return err
		}
		defer towerClientDB.Close()
	}

	// If tor is active and either v2 or v3 onion services have been specified,
	// make a tor controller and pass it into both the watchtower server and
	// the regular lnd server.
	var torController *tor.Controller
	if cfg.Tor.Active && (cfg.Tor.V2 || cfg.Tor.V3) {
		torController = tor.NewController(
			cfg.Tor.Control, cfg.Tor.TargetIPAddress, cfg.Tor.Password,
		)

		// Start the tor controller before giving it to any other subsystems.
		if err := torController.Start(); err != nil {
			err := fmt.Errorf("unable to initialize tor controller: %v", err)
			ltndLog.Error(err)
			return err
		}
		defer func() {
			if err := torController.Stop(); err != nil {
				ltndLog.Errorf("error stopping tor controller: %v", err)
			}
		}()
	}

	var tower *watchtower.Standalone
	if cfg.Watchtower.Active {
		// Segment the watchtower directory by chain and network.
		towerDBDir := filepath.Join(
			cfg.Watchtower.TowerDir,
			registeredChains.PrimaryChain().String(),
			normalizeNetwork(activeNetParams.Name),
		)

		towerDB, err := wtdb.OpenTowerDB(towerDBDir)
		if err != nil {
			err := fmt.Errorf("unable to open watchtower "+
				"database: %v", err)
			ltndLog.Error(err)
			return err
		}
		defer towerDB.Close()

		towerPrivKey, err := activeChainControl.wallet.DerivePrivKey(
			keychain.KeyDescriptor{
				KeyLocator: keychain.KeyLocator{
					Family: keychain.KeyFamilyTowerID,
					Index:  0,
				},
			},
		)
		if err != nil {
			err := fmt.Errorf("unable to derive watchtower "+
				"private key: %v", err)
			ltndLog.Error(err)
			return err
		}

		wtCfg := &watchtower.Config{
			BlockFetcher:   activeChainControl.chainIO,
			DB:             towerDB,
			EpochRegistrar: activeChainControl.chainNotifier,
			Net:            cfg.net,
			NewAddress: func() (btcutil.Address, error) {
				return activeChainControl.wallet.NewAddress(
					lnwallet.WitnessPubKey, false,
				)
			},
			NodePrivKey: towerPrivKey,
			PublishTx:   activeChainControl.wallet.PublishTransaction,
			ChainHash:   *activeNetParams.GenesisHash,
		}

		// If there is a tor controller (user wants auto hidden services), then
		// store a pointer in the watchtower config.
		if torController != nil {
			wtCfg.TorController = torController
			wtCfg.WatchtowerKeyPath = cfg.Tor.WatchtowerKeyPath

			switch {
			case cfg.Tor.V2:
				wtCfg.Type = tor.V2
			case cfg.Tor.V3:
				wtCfg.Type = tor.V3
			}
		}

		wtConfig, err := cfg.Watchtower.Apply(wtCfg, lncfg.NormalizeAddresses)
		if err != nil {
			err := fmt.Errorf("unable to configure watchtower: %v",
				err)
			ltndLog.Error(err)
			return err
		}

		tower, err = watchtower.New(wtConfig)
		if err != nil {
			err := fmt.Errorf("unable to create watchtower: %v", err)
			ltndLog.Error(err)
			return err
		}
	}

	// Initialize the ChainedAcceptor.
	chainedAcceptor := chanacceptor.NewChainedAcceptor()

	// Set up the core server which will listen for incoming peer
	// connections.
	server, err := newServer(
		cfg.Listeners, chanDB, towerClientDB, activeChainControl,
		idPrivKey, walletInitParams.ChansToRestore, chainedAcceptor,
		torController,
	)
	if err != nil {
		err := fmt.Errorf("unable to create server: %v", err)
		ltndLog.Error(err)
		return err
	}

	// Set up an autopilot manager from the current config. This will be
	// used to manage the underlying autopilot agent, starting and stopping
	// it at will.
	atplCfg, err := initAutoPilot(server, cfg.Autopilot, mainChain)
	if err != nil {
		err := fmt.Errorf("unable to initialize autopilot: %v", err)
		ltndLog.Error(err)
		return err
	}

	atplManager, err := autopilot.NewManager(atplCfg)
	if err != nil {
		err := fmt.Errorf("unable to create autopilot manager: %v", err)
		ltndLog.Error(err)
		return err
	}
	if err := atplManager.Start(); err != nil {
		err := fmt.Errorf("unable to start autopilot manager: %v", err)
		ltndLog.Error(err)
		return err
	}
	defer atplManager.Stop()

	// rpcListeners is a closure we'll hand to the rpc server, that will be
	// called when it needs listeners for its GPRC server.
	rpcListeners := func() ([]*ListenerWithSignal, func(), error) {
		// If we have chosen to start with a dedicated listener for the
		// rpc server, we return it directly.
		if lisCfg.RPCListener != nil {
			return []*ListenerWithSignal{lisCfg.RPCListener},
				func() {}, nil
		}

		// Otherwise we'll return the regular listeners.
		return getListeners()
	}

	// Initialize, and register our implementation of the gRPC interface
	// exported by the rpcServer.
	rpcServer, err := newRPCServer(
		server, macaroonService, cfg.SubRPCServers, serverOpts,
		restDialOpts, restProxyDest, atplManager, server.invoices,
		tower, tlsCfg, rpcListeners, chainedAcceptor,
	)
	if err != nil {
		err := fmt.Errorf("unable to create RPC server: %v", err)
		ltndLog.Error(err)
		return err
	}
	if err := rpcServer.Start(); err != nil {
		err := fmt.Errorf("unable to start RPC server: %v", err)
		ltndLog.Error(err)
		return err
	}
	defer rpcServer.Stop()

	// If we're not in regtest or simnet mode, We'll wait until we're fully
	// synced to continue the start up of the remainder of the daemon. This
	// ensures that we don't accept any possibly invalid state transitions, or
	// accept channels with spent funds.
	if !(cfg.Bitcoin.RegTest || cfg.Bitcoin.SimNet ||
		cfg.Litecoin.RegTest || cfg.Litecoin.SimNet) {

		_, bestHeight, err := activeChainControl.chainIO.GetBestBlock()
		if err != nil {
			err := fmt.Errorf("unable to determine chain tip: %v",
				err)
			ltndLog.Error(err)
			return err
		}

		ltndLog.Infof("Waiting for chain backend to finish sync, "+
			"start_height=%v", bestHeight)

		for {
			if !signal.Alive() {
				return nil
			}

			synced, _, err := activeChainControl.wallet.IsSynced()
			if err != nil {
				err := fmt.Errorf("unable to determine if "+
					"wallet is synced: %v", err)
				ltndLog.Error(err)
				return err
			}

			if synced {
				break
			}

			time.Sleep(time.Second * 1)
		}

		_, bestHeight, err = activeChainControl.chainIO.GetBestBlock()
		if err != nil {
			err := fmt.Errorf("unable to determine chain tip: %v",
				err)
			ltndLog.Error(err)
			return err
		}

		ltndLog.Infof("Chain backend is fully synced (end_height=%v)!",
			bestHeight)
	}

	// With all the relevant chains initialized, we can finally start the
	// server itself.
	if err := server.Start(); err != nil {
		err := fmt.Errorf("unable to start server: %v", err)
		ltndLog.Error(err)
		return err
	}
	defer server.Stop()

	// Now that the server has started, if the autopilot mode is currently
	// active, then we'll start the autopilot agent immediately. It will be
	// stopped together with the autopilot service.
	if cfg.Autopilot.Active {
		if err := atplManager.StartAgent(); err != nil {
			err := fmt.Errorf("unable to start autopilot agent: %v",
				err)
			ltndLog.Error(err)
			return err
		}
	}

	if cfg.Watchtower.Active {
		if err := tower.Start(); err != nil {
			err := fmt.Errorf("unable to start watchtower: %v", err)
			ltndLog.Error(err)
			return err
		}
		defer tower.Stop()
	}

	// Wait for shutdown signal from either a graceful server stop or from
	// the interrupt handler.
	<-signal.ShutdownChannel()
	return nil
}

// getTLSConfig returns a TLS configuration for the gRPC server and credentials
// and a proxy destination for the REST reverse proxy.
func getTLSConfig(tlsCertPath string, tlsKeyPath string, tlsExtraIPs,
	tlsExtraDomains []string, rpcListeners []net.Addr) (*tls.Config,
	*credentials.TransportCredentials, string, error) {

	// Ensure we create TLS key and certificate if they don't exist.
	if !fileExists(tlsCertPath) && !fileExists(tlsKeyPath) {
		rpcsLog.Infof("Generating TLS certificates...")
		err := cert.GenCertPair(
			"lnd autogenerated cert", tlsCertPath, tlsKeyPath,
			tlsExtraIPs, tlsExtraDomains,
			cert.DefaultAutogenValidity,
		)
		if err != nil {
			return nil, nil, "", err
		}
		rpcsLog.Infof("Done generating TLS certificates")
	}

	certData, parsedCert, err := cert.LoadCert(tlsCertPath, tlsKeyPath)
	if err != nil {
		return nil, nil, "", err
	}

	// We check whether the certifcate we have on disk match the IPs and
	// domains specified by the config. If the extra IPs or domains have
	// changed from when the certificate was created, we will refresh the
	// certificate if auto refresh is active.
	refresh := false
	if cfg.TLSAutoRefresh {
		refresh, err = cert.IsOutdated(
			parsedCert, tlsExtraIPs, tlsExtraDomains,
		)
		if err != nil {
			return nil, nil, "", err
		}
	}

	// If the certificate expired or it was outdated, delete it and the TLS
	// key and generate a new pair.
	if time.Now().After(parsedCert.NotAfter) || refresh {
		ltndLog.Info("TLS certificate is expired or outdated, " +
			"generating a new one")

		err := os.Remove(tlsCertPath)
		if err != nil {
			return nil, nil, "", err
		}

		err = os.Remove(tlsKeyPath)
		if err != nil {
			return nil, nil, "", err
		}

		rpcsLog.Infof("Renewing TLS certificates...")
		err = cert.GenCertPair(
			"lnd autogenerated cert", tlsCertPath, tlsKeyPath,
			tlsExtraIPs, tlsExtraDomains,
			cert.DefaultAutogenValidity,
		)
		if err != nil {
			return nil, nil, "", err
		}
		rpcsLog.Infof("Done renewing TLS certificates")

		// Reload the certificate data.
		certData, _, err = cert.LoadCert(tlsCertPath, tlsKeyPath)
		if err != nil {
			return nil, nil, "", err
		}
	}

	tlsCfg := cert.TLSConfFromCert(certData)
	restCreds, err := credentials.NewClientTLSFromFile(tlsCertPath, "")
	if err != nil {
		return nil, nil, "", err
	}

	restProxyDest := rpcListeners[0].String()
	switch {
	case strings.Contains(restProxyDest, "0.0.0.0"):
		restProxyDest = strings.Replace(
			restProxyDest, "0.0.0.0", "127.0.0.1", 1,
		)

	case strings.Contains(restProxyDest, "[::]"):
		restProxyDest = strings.Replace(
			restProxyDest, "[::]", "[::1]", 1,
		)
	}

	return tlsCfg, &restCreds, restProxyDest, nil
}

// fileExists reports whether the named file or directory exists.
// This function is taken from https://github.com/btcsuite/btcd
func fileExists(name string) bool {
	if _, err := os.Stat(name); err != nil {
		if os.IsNotExist(err) {
			return false
		}
	}
	return true
}

// genMacaroons generates three macaroon files; one admin-level, one for
// invoice access and one read-only. These can also be used to generate more
// granular macaroons.
func genMacaroons(ctx context.Context, svc *macaroons.Service,
	admFile, roFile, invoiceFile string) error {

	// First, we'll generate a macaroon that only allows the caller to
	// access invoice related calls. This is useful for merchants and other
	// services to allow an isolated instance that can only query and
	// modify invoices.
	invoiceMac, err := svc.Oven.NewMacaroon(
		ctx, bakery.LatestVersion, nil, invoicePermissions...,
	)
	if err != nil {
		return err
	}
	invoiceMacBytes, err := invoiceMac.M().MarshalBinary()
	if err != nil {
		return err
	}
	err = ioutil.WriteFile(invoiceFile, invoiceMacBytes, 0644)
	if err != nil {
		os.Remove(invoiceFile)
		return err
	}

	// Generate the read-only macaroon and write it to a file.
	roMacaroon, err := svc.Oven.NewMacaroon(
		ctx, bakery.LatestVersion, nil, readPermissions...,
	)
	if err != nil {
		return err
	}
	roBytes, err := roMacaroon.M().MarshalBinary()
	if err != nil {
		return err
	}
	if err = ioutil.WriteFile(roFile, roBytes, 0644); err != nil {
		os.Remove(admFile)
		return err
	}

	// Generate the admin macaroon and write it to a file.
	adminPermissions := append(readPermissions, writePermissions...)
	admMacaroon, err := svc.Oven.NewMacaroon(
		ctx, bakery.LatestVersion, nil, adminPermissions...,
	)
	if err != nil {
		return err
	}
	admBytes, err := admMacaroon.M().MarshalBinary()
	if err != nil {
		return err
	}
	if err = ioutil.WriteFile(admFile, admBytes, 0600); err != nil {
		return err
	}

	return nil
}

// WalletUnlockParams holds the variables used to parameterize the unlocking of
// lnd's wallet after it has already been created.
type WalletUnlockParams struct {
	// Password is the public and private wallet passphrase.
	Password []byte

	// Birthday specifies the approximate time that this wallet was created.
	// This is used to bound any rescans on startup.
	Birthday time.Time

	// RecoveryWindow specifies the address lookahead when entering recovery
	// mode. A recovery will be attempted if this value is non-zero.
	RecoveryWindow uint32

	// Wallet is the loaded and unlocked Wallet. This is returned
	// from the unlocker service to avoid it being unlocked twice (once in
	// the unlocker service to check if the password is correct and again
	// later when lnd actually uses it). Because unlocking involves scrypt
	// which is resource intensive, we want to avoid doing it twice.
	Wallet *wallet.Wallet

	// ChansToRestore a set of static channel backups that should be
	// restored before the main server instance starts up.
	ChansToRestore walletunlocker.ChannelsToRecover
}

// waitForWalletPassword will spin up gRPC and REST endpoints for the
// WalletUnlocker server, and block until a password is provided by
// the user to this RPC server.
func waitForWalletPassword(restEndpoints []net.Addr,
	serverOpts []grpc.ServerOption, restDialOpts []grpc.DialOption,
	restProxyDest string, tlsConf *tls.Config,
	getListeners rpcListeners) (*WalletUnlockParams, error) {

	// Start a gRPC server listening for HTTP/2 connections, solely used
	// for getting the encryption password from the client.
	listeners, cleanup, err := getListeners()
	if err != nil {
		return nil, err
	}
	defer cleanup()

	// Set up a new PasswordService, which will listen for passwords
	// provided over RPC.
	grpcServer := grpc.NewServer(serverOpts...)
	defer grpcServer.GracefulStop()

	chainConfig := cfg.Bitcoin
	if registeredChains.PrimaryChain() == litecoinChain {
		chainConfig = cfg.Litecoin
	}

	// The macaroon files are passed to the wallet unlocker since they are
	// also encrypted with the wallet's password. These files will be
	// deleted within it and recreated when successfully changing the
	// wallet's password.
	macaroonFiles := []string{
		filepath.Join(networkDir, macaroons.DBFilename),
		cfg.AdminMacPath, cfg.ReadMacPath, cfg.InvoiceMacPath,
	}
	pwService := walletunlocker.New(
		chainConfig.ChainDir, activeNetParams.Params, !cfg.SyncFreelist,
		macaroonFiles,
	)
	lnrpc.RegisterWalletUnlockerServer(grpcServer, pwService)

	// Use a WaitGroup so we can be sure the instructions on how to input the
	// password is the last thing to be printed to the console.
	var wg sync.WaitGroup

	for _, lis := range listeners {
		wg.Add(1)
		go func(lis *ListenerWithSignal) {
			rpcsLog.Infof("password RPC server listening on %s",
				lis.Addr())

			// Close the ready chan to indicate we are listening.
			close(lis.Ready)

			wg.Done()
			grpcServer.Serve(lis)
		}(lis)
	}

	// Start a REST proxy for our gRPC server above.
	ctx := context.Background()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	mux := proxy.NewServeMux()

	err = lnrpc.RegisterWalletUnlockerHandlerFromEndpoint(
		ctx, mux, restProxyDest, restDialOpts,
	)
	if err != nil {
		return nil, err
	}

	srv := &http.Server{Handler: mux}

	for _, restEndpoint := range restEndpoints {
		lis, err := lncfg.TLSListenOnAddress(restEndpoint, tlsConf)
		if err != nil {
			ltndLog.Errorf(
				"password gRPC proxy unable to listen on %s",
				restEndpoint,
			)
			return nil, err
		}
		defer lis.Close()

		wg.Add(1)
		go func() {
			rpcsLog.Infof(
				"password gRPC proxy started at %s",
				lis.Addr(),
			)
			wg.Done()
			srv.Serve(lis)
		}()
	}

	// Wait for gRPC and REST servers to be up running.
	wg.Wait()

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
		recoveryWindow := initMsg.RecoveryWindow

		// Before we proceed, we'll check the internal version of the
		// seed. If it's greater than the current key derivation
		// version, then we'll return an error as we don't understand
		// this.
		if cipherSeed.InternalVersion != keychain.KeyDerivationVersion {
			return nil, fmt.Errorf("invalid internal seed version "+
				"%v, current version is %v",
				cipherSeed.InternalVersion,
				keychain.KeyDerivationVersion)
		}

		netDir := btcwallet.NetworkDir(
			chainConfig.ChainDir, activeNetParams.Params,
		)
		loader := wallet.NewLoader(
			activeNetParams.Params, netDir, !cfg.SyncFreelist,
			recoveryWindow,
		)

		// With the seed, we can now use the wallet loader to create
		// the wallet, then pass it back to avoid unlocking it again.
		birthday := cipherSeed.BirthdayTime()
		newWallet, err := loader.CreateNewWallet(
			password, password, cipherSeed.Entropy[:], birthday,
		)
		if err != nil {
			// Don't leave the file open in case the new wallet
			// could not be created for whatever reason.
			if err := loader.UnloadWallet(); err != nil {
				ltndLog.Errorf("Could not unload new "+
					"wallet: %v", err)
			}
			return nil, err
		}

		return &WalletUnlockParams{
			Password:       password,
			Birthday:       birthday,
			RecoveryWindow: recoveryWindow,
			Wallet:         newWallet,
			ChansToRestore: initMsg.ChanBackups,
		}, nil

	// The wallet has already been created in the past, and is simply being
	// unlocked. So we'll just return these passphrases.
	case unlockMsg := <-pwService.UnlockMsgs:
		return &WalletUnlockParams{
			Password:       unlockMsg.Passphrase,
			RecoveryWindow: unlockMsg.RecoveryWindow,
			Wallet:         unlockMsg.Wallet,
			ChansToRestore: unlockMsg.ChanBackups,
		}, nil

	case <-signal.ShutdownChannel():
		return nil, fmt.Errorf("shutting down")
	}
}
