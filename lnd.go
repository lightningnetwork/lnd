// Copyright (c) 2013-2017 The btcsuite developers
// Copyright (c) 2015-2016 The Decred developers
// Copyright (C) 2015-2022 The Lightning Network Developers

package lnd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"runtime"
	runtimePprof "runtime/pprof"
	"strings"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	proxy "github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/lightningnetwork/lnd/autopilot"
	"github.com/lightningnetwork/lnd/build"
	"github.com/lightningnetwork/lnd/chanacceptor"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/cluster"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/macaroons"
	"github.com/lightningnetwork/lnd/monitoring"
	"github.com/lightningnetwork/lnd/rpcperms"
	"github.com/lightningnetwork/lnd/signal"
	"github.com/lightningnetwork/lnd/tor"
	"github.com/lightningnetwork/lnd/walletunlocker"
	"github.com/lightningnetwork/lnd/watchtower"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"gopkg.in/macaroon-bakery.v2/bakery"
	"gopkg.in/macaroon.v2"
)

const (
	// adminMacaroonFilePermissions is the file permission that is used for
	// creating the admin macaroon file.
	//
	// Why 640 is safe:
	// Assuming a reasonably secure Linux system, it will have a
	// separate group for each user. E.g. a new user lnd gets assigned group
	// lnd which nothing else belongs to. A system that does not do this is
	// inherently broken already.
	//
	// Since there is no other user in the group, no other user can read
	// admin macaroon unless the administrator explicitly allowed it. Thus
	// there's no harm allowing group read.
	adminMacaroonFilePermissions = 0640

	// leaderResignTimeout is the timeout used when resigning from the
	// leader role. This is kept short so LND can shut down quickly in case
	// of a system failure or network partition making the cluster
	// unresponsive. The cluster itself should ensure that the leader is not
	// elected again until the previous leader has resigned or the leader
	// election timeout has passed.
	leaderResignTimeout = 5 * time.Second
)

// AdminAuthOptions returns a list of DialOptions that can be used to
// authenticate with the RPC server with admin capabilities.
// skipMacaroons=true should be set if we don't want to include macaroons with
// the auth options. This is needed for instance for the WalletUnlocker
// service, which must be usable also before macaroons are created.
//
// NOTE: This should only be called after the RPCListener has signaled it is
// ready.
func AdminAuthOptions(cfg *Config, skipMacaroons bool) ([]grpc.DialOption,
	error) {

	creds, err := credentials.NewClientTLSFromFile(cfg.TLSCertPath, "")
	if err != nil {
		return nil, fmt.Errorf("unable to read TLS cert: %w", err)
	}

	// Create a dial options array.
	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(creds),
	}

	// Get the admin macaroon if macaroons are active.
	if !skipMacaroons && !cfg.NoMacaroons {
		// Load the admin macaroon file.
		macBytes, err := os.ReadFile(cfg.AdminMacPath)
		if err != nil {
			return nil, fmt.Errorf("unable to read macaroon "+
				"path (check the network setting!): %v", err)
		}

		mac := &macaroon.Macaroon{}
		if err = mac.UnmarshalBinary(macBytes); err != nil {
			return nil, fmt.Errorf("unable to decode macaroon: %w",
				err)
		}

		// Now we append the macaroon credentials to the dial options.
		cred, err := macaroons.NewMacaroonCredential(mac)
		if err != nil {
			return nil, fmt.Errorf("error cloning mac: %w", err)
		}
		opts = append(opts, grpc.WithPerRPCCredentials(cred))
	}

	return opts, nil
}

// ListenerWithSignal is a net.Listener that has an additional Ready channel
// that will be closed when a server starts listening.
type ListenerWithSignal struct {
	net.Listener

	// Ready will be closed by the server listening on Listener.
	Ready chan struct{}

	// MacChan is an optional way to pass the admin macaroon to the program
	// that started lnd. The channel should be buffered to avoid lnd being
	// blocked on sending to the channel.
	MacChan chan []byte
}

// ListenerCfg is a wrapper around custom listeners that can be passed to lnd
// when calling its main method.
type ListenerCfg struct {
	// RPCListeners can be set to the listeners to use for the RPC server.
	// If empty a regular network listener will be created.
	RPCListeners []*ListenerWithSignal
}

var errStreamIsolationWithProxySkip = errors.New(
	"while stream isolation is enabled, the TOR proxy may not be skipped",
)

// Main is the true entry point for lnd. It accepts a fully populated and
// validated main configuration struct and an optional listener config struct.
// This function starts all main system components then blocks until a signal
// is received on the shutdownChan at which point everything is shut down again.
func Main(cfg *Config, lisCfg ListenerCfg, implCfg *ImplementationCfg,
	interceptor signal.Interceptor) error {

	defer func() {
		ltndLog.Info("Shutdown complete")
		err := cfg.LogRotator.Close()
		if err != nil {
			ltndLog.Errorf("Could not close log rotator: %v", err)
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ctx, err := build.WithBuildInfo(ctx, cfg.LogConfig)
	if err != nil {
		return fmt.Errorf("unable to add build info to context: %w",
			err)
	}

	mkErr := func(msg string, err error, attrs ...any) error {
		ltndLog.ErrorS(ctx, "Shutting down due to error in main "+
			"method", err, attrs...)

		var (
			params = []any{err}
			fmtStr = msg + ": %w"
		)
		for _, attr := range attrs {
			fmtStr += " %s"

			params = append(params, attr)
		}

		return fmt.Errorf(fmtStr, params...)
	}

	// Show version at startup.
	ltndLog.InfoS(ctx, "Version Info",
		slog.String("version", build.Version()),
		slog.String("commit", build.Commit),
		slog.Any("debuglevel", build.Deployment),
		slog.String("logging", cfg.DebugLevel))

	var network string
	switch {
	case cfg.Bitcoin.TestNet3:
		network = "testnet"

	case cfg.Bitcoin.TestNet4:
		network = "testnet4"

	case cfg.Bitcoin.MainNet:
		network = "mainnet"

	case cfg.Bitcoin.SimNet:
		network = "simnet"

	case cfg.Bitcoin.RegTest:
		network = "regtest"

	case cfg.Bitcoin.SigNet:
		network = "signet"
	}

	ltndLog.InfoS(ctx, "Network Info",
		"active_chain", strings.Title(BitcoinChainName),
		"network", network)

	// Enable http profiling server if requested.
	if cfg.Pprof.Profile != "" {
		// Create the http handler.
		pprofMux := http.NewServeMux()
		pprofMux.HandleFunc("/debug/pprof/", pprof.Index)
		pprofMux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		pprofMux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		pprofMux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		pprofMux.HandleFunc("/debug/pprof/trace", pprof.Trace)

		if cfg.Pprof.BlockingProfile != 0 {
			runtime.SetBlockProfileRate(cfg.Pprof.BlockingProfile)
		}
		if cfg.Pprof.MutexProfile != 0 {
			runtime.SetMutexProfileFraction(cfg.Pprof.MutexProfile)
		}

		// Redirect all requests to the pprof handler, thus visiting
		// `127.0.0.1:6060` will be redirected to
		// `127.0.0.1:6060/debug/pprof`.
		pprofMux.Handle("/", http.RedirectHandler(
			"/debug/pprof/", http.StatusSeeOther,
		))

		ltndLog.InfoS(ctx, "Pprof listening", "addr", cfg.Pprof.Profile)

		// Create the pprof server.
		pprofServer := &http.Server{
			Addr:              cfg.Pprof.Profile,
			Handler:           pprofMux,
			ReadHeaderTimeout: cfg.HTTPHeaderTimeout,
		}

		// Shut the server down when lnd is shutting down.
		defer func() {
			ltndLog.InfoS(ctx, "Stopping pprof server...")
			err := pprofServer.Shutdown(ctx)
			if err != nil {
				ltndLog.ErrorS(ctx, "Stop pprof server", err)
			}
		}()

		// Start the pprof server.
		go func() {
			err := pprofServer.ListenAndServe()
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				ltndLog.ErrorS(ctx, "Could not serve pprof "+
					"server", err)
			}
		}()
	}

	// Write cpu profile if requested.
	if cfg.Pprof.CPUProfile != "" {
		f, err := os.Create(cfg.Pprof.CPUProfile)
		if err != nil {
			return mkErr("unable to create CPU profile", err)
		}
		_ = runtimePprof.StartCPUProfile(f)
		defer func() {
			_ = f.Close()
		}()
		defer runtimePprof.StopCPUProfile()
	}

	// Run configuration dependent DB pre-initialization. Note that this
	// needs to be done early and once during the startup process, before
	// any DB access.
	if err := cfg.DB.Init(ctx, cfg.graphDatabaseDir()); err != nil {
		return mkErr("error initializing DBs", err)
	}

	tlsManagerCfg := &TLSManagerCfg{
		TLSCertPath:        cfg.TLSCertPath,
		TLSKeyPath:         cfg.TLSKeyPath,
		TLSEncryptKey:      cfg.TLSEncryptKey,
		TLSExtraIPs:        cfg.TLSExtraIPs,
		TLSExtraDomains:    cfg.TLSExtraDomains,
		TLSAutoRefresh:     cfg.TLSAutoRefresh,
		TLSDisableAutofill: cfg.TLSDisableAutofill,
		TLSCertDuration:    cfg.TLSCertDuration,

		LetsEncryptDir:    cfg.LetsEncryptDir,
		LetsEncryptDomain: cfg.LetsEncryptDomain,
		LetsEncryptListen: cfg.LetsEncryptListen,

		DisableRestTLS: cfg.DisableRestTLS,

		HTTPHeaderTimeout: cfg.HTTPHeaderTimeout,
	}
	tlsManager := NewTLSManager(tlsManagerCfg)
	serverOpts, restDialOpts, restListen, cleanUp,
		err := tlsManager.SetCertificateBeforeUnlock()
	if err != nil {
		return mkErr("error setting cert before unlock", err)
	}
	if cleanUp != nil {
		defer cleanUp()
	}

	// If we have chosen to start with a dedicated listener for the
	// rpc server, we set it directly.
	grpcListeners := append([]*ListenerWithSignal{}, lisCfg.RPCListeners...)
	if len(grpcListeners) == 0 {
		// Otherwise we create listeners from the RPCListeners defined
		// in the config.
		for _, grpcEndpoint := range cfg.RPCListeners {
			// Start a gRPC server listening for HTTP/2
			// connections.
			lis, err := lncfg.ListenOnAddress(grpcEndpoint)
			if err != nil {
				return mkErr("unable to listen on grpc "+
					"endpoint", err,
					slog.String(
						"endpoint",
						grpcEndpoint.String(),
					))
			}
			defer lis.Close()

			grpcListeners = append(
				grpcListeners, &ListenerWithSignal{
					Listener: lis,
					Ready:    make(chan struct{}),
				},
			)
		}
	}

	// Create a new RPC interceptor that we'll add to the GRPC server. This
	// will be used to log the API calls invoked on the GRPC server.
	interceptorChain := rpcperms.NewInterceptorChain(
		rpcsLog, cfg.NoMacaroons, cfg.RPCMiddleware.Mandatory,
	)
	if err := interceptorChain.Start(); err != nil {
		return mkErr("error starting interceptor chain", err)
	}
	defer func() {
		err := interceptorChain.Stop()
		if err != nil {
			ltndLog.Warnf("error stopping RPC interceptor "+
				"chain: %v", err)
		}
	}()

	// Allow the user to overwrite some defaults of the gRPC library related
	// to connection keepalive (server side and client side pings).
	serverKeepalive := keepalive.ServerParameters{
		Time:    cfg.GRPC.ServerPingTime,
		Timeout: cfg.GRPC.ServerPingTimeout,
	}
	clientKeepalive := keepalive.EnforcementPolicy{
		MinTime:             cfg.GRPC.ClientPingMinWait,
		PermitWithoutStream: cfg.GRPC.ClientAllowPingWithoutStream,
	}

	rpcServerOpts := interceptorChain.CreateServerOpts()
	serverOpts = append(serverOpts, rpcServerOpts...)
	serverOpts = append(
		serverOpts, grpc.MaxRecvMsgSize(lnrpc.MaxGrpcMsgSize),
		grpc.KeepaliveParams(serverKeepalive),
		grpc.KeepaliveEnforcementPolicy(clientKeepalive),
	)

	grpcServer := grpc.NewServer(serverOpts...)
	defer grpcServer.Stop()

	// We'll also register the RPC interceptor chain as the StateServer, as
	// it can be used to query for the current state of the wallet.
	lnrpc.RegisterStateServer(grpcServer, interceptorChain)

	// Initialize, and register our implementation of the gRPC interface
	// exported by the rpcServer.
	rpcServer := newRPCServer(cfg, interceptorChain, implCfg, interceptor)
	err = rpcServer.RegisterWithGrpcServer(grpcServer)
	if err != nil {
		return mkErr("error registering gRPC server", err)
	}

	// Now that both the WalletUnlocker and LightningService have been
	// registered with the GRPC server, we can start listening.
	err = startGrpcListen(cfg, grpcServer, grpcListeners)
	if err != nil {
		return mkErr("error starting gRPC listener", err)
	}

	// Now start the REST proxy for our gRPC server above. We'll ensure
	// we direct LND to connect to its loopback address rather than a
	// wildcard to prevent certificate issues when accessing the proxy
	// externally.
	stopProxy, err := startRestProxy(
		ctx, cfg, rpcServer, restDialOpts, restListen,
	)
	if err != nil {
		return mkErr("error starting REST proxy", err)
	}
	defer stopProxy()

	// Start leader election if we're running on etcd. Continuation will be
	// blocked until this instance is elected as the current leader or
	// shutting down.
	elected := false
	var leaderElector cluster.LeaderElector
	if cfg.Cluster.EnableLeaderElection {
		electionCtx, cancelElection := context.WithCancel(ctx)

		go func() {
			<-interceptor.ShutdownChannel()
			cancelElection()
		}()

		ltndLog.InfoS(ctx, "Using leader elector",
			"elector", cfg.Cluster.LeaderElector)

		leaderElector, err = cfg.Cluster.MakeLeaderElector(
			electionCtx, cfg.DB,
		)
		if err != nil {
			return err
		}

		defer func() {
			if !elected {
				return
			}

			ltndLog.InfoS(ctx, "Attempting to resign from "+
				"leader role", "cluster_id", cfg.Cluster.ID)

			// Ensure that we don't block the shutdown process if
			// the leader resigning process takes too long. The
			// cluster will ensure that the leader is not elected
			// again until the previous leader has resigned or the
			// leader election timeout has passed.
			timeoutCtx, cancel := context.WithTimeout(
				ctx, leaderResignTimeout,
			)
			defer cancel()

			if err := leaderElector.Resign(timeoutCtx); err != nil {
				ltndLog.Errorf("Leader elector failed to "+
					"resign: %v", err)
			}
		}()

		ltndLog.InfoS(ctx, "Starting leadership campaign",
			"cluster_id", cfg.Cluster.ID)

		if err := leaderElector.Campaign(electionCtx); err != nil {
			return mkErr("leadership campaign failed", err)
		}

		elected = true
		ltndLog.InfoS(ctx, "Elected as leader",
			"cluster_id", cfg.Cluster.ID)
	}

	dbs, cleanUp, err := implCfg.DatabaseBuilder.BuildDatabase(ctx)
	switch {
	case errors.Is(err, channeldb.ErrDryRunMigrationOK):
		ltndLog.InfoS(ctx, "Exiting due to BuildDatabase error",
			slog.Any("err", err))
		return nil
	case err != nil:
		return mkErr("unable to open databases", err)
	}

	defer cleanUp()

	partialChainControl, walletConfig, cleanUp, err := implCfg.BuildWalletConfig(
		ctx, dbs, &implCfg.AuxComponents, interceptorChain,
		grpcListeners,
	)
	if err != nil {
		return mkErr("error creating wallet config", err)
	}

	defer cleanUp()

	activeChainControl, cleanUp, err := implCfg.BuildChainControl(
		partialChainControl, walletConfig,
	)
	if err != nil {
		return mkErr("error loading chain control", err)
	}

	defer cleanUp()

	// TODO(roasbeef): add rotation
	idKeyDesc, err := activeChainControl.KeyRing.DeriveKey(
		keychain.KeyLocator{
			Family: keychain.KeyFamilyNodeKey,
			Index:  0,
		},
	)
	if err != nil {
		return mkErr("error deriving node key", err)
	}

	if cfg.Tor.StreamIsolation && cfg.Tor.SkipProxyForClearNetTargets {
		return errStreamIsolationWithProxySkip
	}

	if cfg.Tor.Active {
		if cfg.Tor.SkipProxyForClearNetTargets {
			srvrLog.InfoS(ctx, "Onion services are accessible "+
				"via Tor! NOTE: Traffic to clearnet services "+
				"is not routed via Tor.")
		} else {
			srvrLog.InfoS(ctx, "Proxying all network traffic "+
				"via Tor! NOTE: Ensure the backend node is "+
				"proxying over Tor as well",
				"stream_isolation", cfg.Tor.StreamIsolation)
		}
	}

	// If tor is active and either v2 or v3 onion services have been
	// specified, make a tor controller and pass it into both the watchtower
	// server and the regular lnd server.
	var torController *tor.Controller
	if cfg.Tor.Active && (cfg.Tor.V2 || cfg.Tor.V3) {
		torController = tor.NewController(
			cfg.Tor.Control, cfg.Tor.TargetIPAddress,
			cfg.Tor.Password,
		)

		// Start the tor controller before giving it to any other
		// subsystems.
		if err := torController.Start(); err != nil {
			return mkErr("unable to initialize tor controller",
				err)
		}
		defer func() {
			if err := torController.Stop(); err != nil {
				ltndLog.ErrorS(ctx, "Error stopping tor "+
					"controller", err)
			}
		}()
	}

	var tower *watchtower.Standalone
	if cfg.Watchtower.Active {
		towerKeyDesc, err := activeChainControl.KeyRing.DeriveKey(
			keychain.KeyLocator{
				Family: keychain.KeyFamilyTowerID,
				Index:  0,
			},
		)
		if err != nil {
			return mkErr("error deriving tower key", err)
		}

		wtCfg := &watchtower.Config{
			BlockFetcher:   activeChainControl.ChainIO,
			DB:             dbs.TowerServerDB,
			EpochRegistrar: activeChainControl.ChainNotifier,
			Net:            cfg.net,
			NewAddress: func() (btcutil.Address, error) {
				return activeChainControl.Wallet.NewAddress(
					lnwallet.TaprootPubkey, false,
					lnwallet.DefaultAccountName,
				)
			},
			NodeKeyECDH: keychain.NewPubKeyECDH(
				towerKeyDesc, activeChainControl.KeyRing,
			),
			PublishTx: activeChainControl.Wallet.PublishTransaction,
			ChainHash: *cfg.ActiveNetParams.GenesisHash,
		}

		// If there is a tor controller (user wants auto hidden
		// services), then store a pointer in the watchtower config.
		if torController != nil {
			wtCfg.TorController = torController
			wtCfg.WatchtowerKeyPath = cfg.Tor.WatchtowerKeyPath
			wtCfg.EncryptKey = cfg.Tor.EncryptKey
			wtCfg.KeyRing = activeChainControl.KeyRing

			switch {
			case cfg.Tor.V2:
				wtCfg.Type = tor.V2
			case cfg.Tor.V3:
				wtCfg.Type = tor.V3
			}
		}

		wtConfig, err := cfg.Watchtower.Apply(
			wtCfg, lncfg.NormalizeAddresses,
		)
		if err != nil {
			return mkErr("unable to configure watchtower", err)
		}

		tower, err = watchtower.New(wtConfig)
		if err != nil {
			return mkErr("unable to create watchtower", err)
		}
	}

	// Initialize the MultiplexAcceptor. If lnd was started with the
	// zero-conf feature bit, then this will be a ZeroConfAcceptor.
	// Otherwise, this will be a ChainedAcceptor.
	var multiAcceptor chanacceptor.MultiplexAcceptor
	if cfg.ProtocolOptions.ZeroConf() {
		multiAcceptor = chanacceptor.NewZeroConfAcceptor()
	} else {
		multiAcceptor = chanacceptor.NewChainedAcceptor()
	}

	// Set up the core server which will listen for incoming peer
	// connections.
	server, err := newServer(
		ctx, cfg, cfg.Listeners, dbs, activeChainControl, &idKeyDesc,
		activeChainControl.Cfg.WalletUnlockParams.ChansToRestore,
		multiAcceptor, torController, tlsManager, leaderElector,
		implCfg,
	)
	if err != nil {
		return mkErr("unable to create server", err)
	}

	// Set up an autopilot manager from the current config. This will be
	// used to manage the underlying autopilot agent, starting and stopping
	// it at will.
	atplCfg, err := initAutoPilot(
		server, cfg.Autopilot, activeChainControl.MinHtlcIn,
		cfg.ActiveNetParams,
	)
	if err != nil {
		return mkErr("unable to initialize autopilot", err)
	}

	atplManager, err := autopilot.NewManager(atplCfg)
	if err != nil {
		return mkErr("unable to create autopilot manager", err)
	}
	if err := atplManager.Start(); err != nil {
		return mkErr("unable to start autopilot manager", err)
	}
	defer atplManager.Stop()

	err = tlsManager.LoadPermanentCertificate(activeChainControl.KeyRing)
	if err != nil {
		return mkErr("unable to load permanent TLS certificate", err)
	}

	// Now we have created all dependencies necessary to populate and
	// start the RPC server.
	err = rpcServer.addDeps(
		ctx, server, interceptorChain.MacaroonService(),
		cfg.SubRPCServers, atplManager, server.invoices, tower,
		multiAcceptor, server.invoiceHtlcModifier,
	)
	if err != nil {
		return mkErr("unable to add deps to RPC server", err)
	}
	if err := rpcServer.Start(); err != nil {
		return mkErr("unable to start RPC server", err)
	}
	defer rpcServer.Stop()

	// We transition the RPC state to Active, as the RPC server is up.
	interceptorChain.SetRPCActive()

	if err := interceptor.Notifier.NotifyReady(true); err != nil {
		return mkErr("error notifying ready", err)
	}

	// We'll wait until we're fully synced to continue the start up of the
	// remainder of the daemon. This ensures that we don't accept any
	// possibly invalid state transitions, or accept channels with spent
	// funds.
	_, bestHeight, err := activeChainControl.ChainIO.GetBestBlock()
	if err != nil {
		return mkErr("unable to determine chain tip", err)
	}

	ltndLog.InfoS(ctx, "Waiting for chain backend to finish sync",
		slog.Int64("start_height", int64(bestHeight)))

	type syncResult struct {
		synced        bool
		bestBlockTime int64
		err           error
	}

	var syncedResChan = make(chan syncResult, 1)

	for {
		// We check if the wallet is synced in a separate goroutine as
		// the call is blocking, and we want to be able to interrupt it
		// if the daemon is shutting down.
		go func() {
			synced, bestBlockTime, err := activeChainControl.Wallet.
				IsSynced()
			syncedResChan <- syncResult{synced, bestBlockTime, err}
		}()

		select {
		case <-interceptor.ShutdownChannel():
			return nil

		case res := <-syncedResChan:
			if res.err != nil {
				return mkErr("unable to determine if wallet "+
					"is synced", res.err)
			}

			ltndLog.DebugS(ctx, "Syncing to block chain",
				"best_block_time", time.Unix(res.bestBlockTime, 0),
				"is_synced", res.synced)

			if res.synced {
				break
			}

			// If we're not yet synced, we'll wait for a second
			// before checking again.
			select {
			case <-interceptor.ShutdownChannel():
				return nil

			case <-time.After(time.Second):
				continue
			}
		}

		break
	}

	_, bestHeight, err = activeChainControl.ChainIO.GetBestBlock()
	if err != nil {
		return mkErr("unable to determine chain tip", err)
	}

	ltndLog.InfoS(ctx, "Chain backend is fully synced!",
		"end_height", bestHeight)

	// With all the relevant chains initialized, we can finally start the
	// server itself. We start the server in an asynchronous goroutine so
	// that we are able to interrupt and shutdown the daemon gracefully in
	// case the startup of the subservers do not behave as expected.
	errChan := make(chan error)
	go func() {
		errChan <- server.Start(ctx)
	}()

	defer func() {
		err := server.Stop()
		if err != nil {
			ltndLog.WarnS(ctx, "Stopping the server including all "+
				"its subsystems failed with", err)
		}
	}()

	select {
	case err := <-errChan:
		if err == nil {
			break
		}

		return mkErr("unable to start server", err)

	case <-interceptor.ShutdownChannel():
		return nil
	}

	// We transition the server state to Active, as the server is up.
	interceptorChain.SetServerActive()

	// Now that the server has started, if the autopilot mode is currently
	// active, then we'll start the autopilot agent immediately. It will be
	// stopped together with the autopilot service.
	if cfg.Autopilot.Active {
		if err := atplManager.StartAgent(); err != nil {
			return mkErr("unable to start autopilot agent", err)
		}
	}

	if cfg.Watchtower.Active {
		if err := tower.Start(); err != nil {
			return mkErr("unable to start watchtower", err)
		}
		defer tower.Stop()
	}

	// Wait for shutdown signal from either a graceful server stop or from
	// the interrupt handler.
	<-interceptor.ShutdownChannel()
	return nil
}

// bakeMacaroon creates a new macaroon with newest version and the given
// permissions then returns it binary serialized.
func bakeMacaroon(ctx context.Context, svc *macaroons.Service,
	permissions []bakery.Op) ([]byte, error) {

	mac, err := svc.NewMacaroon(
		ctx, macaroons.DefaultRootKeyID, permissions...,
	)
	if err != nil {
		return nil, err
	}

	return mac.M().MarshalBinary()
}

// saveMacaroon bakes a macaroon with the specified macaroon permissions and
// writes it to a file with the given filename and file permissions.
func saveMacaroon(ctx context.Context, svc *macaroons.Service, filename string,
	macaroonPermissions []bakery.Op, filePermissions os.FileMode) error {

	macaroonBytes, err := bakeMacaroon(ctx, svc, macaroonPermissions)
	if err != nil {
		return err
	}
	err = os.WriteFile(filename, macaroonBytes, filePermissions)
	if err != nil {
		_ = os.Remove(filename)
		return err
	}

	return nil
}

// genDefaultMacaroons checks for three default macaroon files and generates
// them if they do not exist; one admin-level, one for invoice access and one
// read-only. Each macaroon is checked and created independently to ensure all
// three exist. The admin macaroon can also be used to generate more granular
// macaroons.
func genDefaultMacaroons(ctx context.Context, svc *macaroons.Service,
	admFile, roFile, invoiceFile string) error {

	// First, we'll generate a macaroon that only allows the caller to
	// access invoice related calls. This is useful for merchants and other
	// services to allow an isolated instance that can only query and
	// modify invoices.
	if !lnrpc.FileExists(invoiceFile) {
		err := saveMacaroon(
			ctx, svc, invoiceFile, invoicePermissions, 0644,
		)
		if err != nil {
			return err
		}
	}

	// Generate the read-only macaroon and write it to a file.
	if !lnrpc.FileExists(roFile) {
		err := saveMacaroon(
			ctx, svc, roFile, readPermissions, 0644,
		)
		if err != nil {
			return err
		}
	}

	// Generate the admin macaroon and write it to a file.
	if !lnrpc.FileExists(admFile) {
		err := saveMacaroon(
			ctx, svc, admFile, adminPermissions(),
			adminMacaroonFilePermissions,
		)
		if err != nil {
			return err
		}
	}

	return nil
}

// adminPermissions returns a list of all permissions in a safe way that doesn't
// modify any of the source lists.
func adminPermissions() []bakery.Op {
	admin := make([]bakery.Op, len(readPermissions)+len(writePermissions))
	copy(admin[:len(readPermissions)], readPermissions)
	copy(admin[len(readPermissions):], writePermissions)
	return admin
}

// createWalletUnlockerService creates a WalletUnlockerService from the passed
// config.
func createWalletUnlockerService(cfg *Config) *walletunlocker.UnlockerService {
	// The macaroonFiles are passed to the wallet unlocker so they can be
	// deleted and recreated in case the root macaroon key is also changed
	// during the change password operation.
	macaroonFiles := []string{
		cfg.AdminMacPath, cfg.ReadMacPath, cfg.InvoiceMacPath,
	}

	return walletunlocker.New(
		cfg.ActiveNetParams.Params, macaroonFiles,
		cfg.ResetWalletTransactions, nil,
	)
}

// startGrpcListen starts the GRPC server on the passed listeners.
func startGrpcListen(cfg *Config, grpcServer *grpc.Server,
	listeners []*ListenerWithSignal) error {

	// Use a WaitGroup so we can be sure the instructions on how to input the
	// password is the last thing to be printed to the console.
	var wg sync.WaitGroup

	for _, lis := range listeners {
		wg.Add(1)
		go func(lis *ListenerWithSignal) {
			rpcsLog.Infof("RPC server listening on %s", lis.Addr())

			// Close the ready chan to indicate we are listening.
			close(lis.Ready)

			wg.Done()
			_ = grpcServer.Serve(lis)
		}(lis)
	}

	// If Prometheus monitoring is enabled, start the Prometheus exporter.
	if cfg.Prometheus.Enabled() {
		err := monitoring.ExportPrometheusMetrics(
			grpcServer, cfg.Prometheus,
		)
		if err != nil {
			return err
		}
	}

	// Wait for gRPC servers to be up running.
	wg.Wait()

	return nil
}

// startRestProxy starts the given REST proxy on the listeners found in the
// config.
func startRestProxy(ctx context.Context, cfg *Config, rpcServer *rpcServer,
	restDialOpts []grpc.DialOption,
	restListen func(net.Addr) (net.Listener, error)) (func(), error) {

	// We use the first RPC listener as the destination for our REST proxy.
	// If the listener is set to listen on all interfaces, we replace it
	// with localhost, as we cannot dial it directly.
	restProxyDest := cfg.RPCListeners[0].String()
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

	var shutdownFuncs []func()
	shutdown := func() {
		for _, shutdownFn := range shutdownFuncs {
			shutdownFn()
		}
	}

	// Start a REST proxy for our gRPC server.
	ctx, cancel := context.WithCancel(ctx)
	shutdownFuncs = append(shutdownFuncs, cancel)

	// We'll set up a proxy that will forward REST calls to the GRPC
	// server.
	//
	// The default JSON marshaler of the REST proxy only sets OrigName to
	// true, which instructs it to use the same field names as specified in
	// the proto file and not switch to camel case. What we also want is
	// that the marshaler prints all values, even if they are falsey.
	customMarshalerOption := proxy.WithMarshalerOption(
		proxy.MIMEWildcard, &proxy.JSONPb{
			MarshalOptions:   *lnrpc.RESTJsonMarshalOpts,
			UnmarshalOptions: *lnrpc.RESTJsonUnmarshalOpts,
		},
	)
	mux := proxy.NewServeMux(
		customMarshalerOption,

		// Don't allow falling back to other HTTP methods, we want exact
		// matches only. The actual method to be used can be overwritten
		// by setting X-HTTP-Method-Override so there should be no
		// reason for not specifying the correct method in the first
		// place.
		proxy.WithDisablePathLengthFallback(),
	)

	// Register our services with the REST proxy.
	err := rpcServer.RegisterWithRestProxy(
		ctx, mux, restDialOpts, restProxyDest,
	)
	if err != nil {
		return nil, err
	}

	// Wrap the default grpc-gateway handler with the WebSocket handler.
	restHandler := lnrpc.NewWebSocketProxy(
		mux, rpcsLog, cfg.WSPingInterval, cfg.WSPongWait,
		lnrpc.LndClientStreamingURIs,
	)

	// Use a WaitGroup so we can be sure the instructions on how to input the
	// password is the last thing to be printed to the console.
	var wg sync.WaitGroup

	// Now spin up a network listener for each requested port and start a
	// goroutine that serves REST with the created mux there.
	for _, restEndpoint := range cfg.RESTListeners {
		lis, err := restListen(restEndpoint)
		if err != nil {
			ltndLog.Errorf("gRPC proxy unable to listen on %s",
				restEndpoint)
			return nil, err
		}

		shutdownFuncs = append(shutdownFuncs, func() {
			err := lis.Close()
			if err != nil {
				rpcsLog.Errorf("Error closing listener: %v",
					err)
			}
		})

		wg.Add(1)
		go func() {
			rpcsLog.Infof("gRPC proxy started at %s", lis.Addr())

			// Create our proxy chain now. A request will pass
			// through the following chain:
			// req ---> CORS handler --> WS proxy --->
			//   REST proxy --> gRPC endpoint
			corsHandler := allowCORS(restHandler, cfg.RestCORS)

			wg.Done()
			err := http.Serve(lis, corsHandler)
			if err != nil && !lnrpc.IsClosedConnError(err) {
				rpcsLog.Error(err)
			}
		}()
	}

	// Wait for REST servers to be up running.
	wg.Wait()

	return shutdown, nil
}
