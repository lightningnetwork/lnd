package main

import (
	"crypto/rand"
	"fmt"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"runtime"
	"strconv"
	"time"

	"golang.org/x/net/context"

	"google.golang.org/grpc"

	"github.com/btcsuite/btcd/btcec"
	flags "github.com/btcsuite/go-flags"
	proxy "github.com/grpc-ecosystem/grpc-gateway/runtime"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
)

var (
	cfg              *config
	shutdownChannel  = make(chan struct{})
	registeredChains = newChainRegistry()
)

// lndMain is the true entry point for lnd. This function is required since
// defers created in the top-level scope of a main method aren't executed if
// os.Exit() is called.
func lndMain() error {
	// Load the configuration, and parse any command line options. This
	// function will also set up logging properly.
	loadedConfig, err := loadConfig()
	if err != nil {
		return err
	}
	cfg = loadedConfig
	defer backendLog.Flush()

	// Show version at startup.
	ltndLog.Infof("Version %s", version())

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

	// Open the channeldb, which is dedicated to storing channel, and
	// network related metadata.
	chanDB, err := channeldb.Open(cfg.DataDir)
	if err != nil {
		ltndLog.Errorf("unable to open channeldb: %v", err)
		return err
	}
	defer chanDB.Close()

	// With the information parsed from the configuration, create valid
	// instances of the paertinent interfaces required to operate the
	// Lightning Network Daemon.
	activeChainControl, chainCleanUp, err := newChainControlFromConfig(cfg, chanDB)
	if err != nil {
		fmt.Printf("unable to create chain control: %v\n", err)
		return err
	}
	if chainCleanUp != nil {
		defer chainCleanUp()
	}

	// Finally before we start the server, we'll register the "holy
	// trinity" of interface for our current "home chain" with the active
	// chainRegistry interface.
	primaryChain := registeredChains.PrimaryChain()
	registeredChains.RegisterChain(primaryChain, activeChainControl)

	idPrivKey, err := activeChainControl.wallet.GetIdentitykey()
	if err != nil {
		return err
	}
	idPrivKey.Curve = btcec.S256()

	// Set up the core server which will listen for incoming peer
	// connections.
	defaultListenAddrs := []string{
		net.JoinHostPort("", strconv.Itoa(cfg.PeerPort)),
	}
	server, err := newServer(defaultListenAddrs, chanDB, activeChainControl,
		idPrivKey)
	if err != nil {
		srvrLog.Errorf("unable to create server: %v\n", err)
		return err
	}

	nodeSigner := newNodeSigner(idPrivKey)
	var chanIDSeed [32]byte
	if _, err := rand.Read(chanIDSeed[:]); err != nil {
		return err
	}
	fundingMgr, err := newFundingManager(fundingConfig{
		IDKey:        idPrivKey.PubKey(),
		Wallet:       activeChainControl.wallet,
		Notifier:     activeChainControl.chainNotifier,
		FeeEstimator: activeChainControl.feeEstimator,
		SignMessage: func(pubKey *btcec.PublicKey,
			msg []byte) (*btcec.Signature, error) {

			if pubKey.IsEqual(idPrivKey.PubKey()) {
				return nodeSigner.SignMessage(pubKey, msg)
			}

			return activeChainControl.msgSigner.SignMessage(
				pubKey, msg,
			)
		},
		SendAnnouncement: func(msg lnwire.Message) error {
			server.discoverSrv.ProcessLocalAnnouncement(msg,
				idPrivKey.PubKey())
			return nil
		},
		ArbiterChan:    server.breachArbiter.newContracts,
		SendToPeer:     server.sendToPeer,
		FindPeer:       server.findPeer,
		TempChanIDSeed: chanIDSeed,
		FindChannel: func(chanID lnwire.ChannelID) (*lnwallet.LightningChannel, error) {
			dbChannels, err := chanDB.FetchAllChannels()
			if err != nil {
				return nil, err
			}

			for _, channel := range dbChannels {
				if chanID.IsChanPoint(channel.ChanID) {
					return lnwallet.NewLightningChannel(
						activeChainControl.signer,
						activeChainControl.chainNotifier,
						activeChainControl.feeEstimator,
						channel)
				}
			}

			return nil, fmt.Errorf("unable to find channel")
		},
	})
	if err != nil {
		return err
	}
	if err := fundingMgr.Start(); err != nil {
		return err
	}
	server.fundingMgr = fundingMgr

	// Initialize, and register our implementation of the gRPC server.
	var opts []grpc.ServerOption
	rpcServer := newRPCServer(server)
	if err := rpcServer.Start(); err != nil {
		return err
	}
	grpcServer := grpc.NewServer(opts...)
	lnrpc.RegisterLightningServer(grpcServer, rpcServer)

	// Next, Start the grpc server listening for HTTP/2 connections.
	grpcEndpoint := fmt.Sprintf("localhost:%d", loadedConfig.RPCPort)
	lis, err := net.Listen("tcp", grpcEndpoint)
	if err != nil {
		fmt.Printf("failed to listen: %v", err)
		return err
	}
	defer lis.Close()
	go func() {
		rpcsLog.Infof("RPC server listening on %s", lis.Addr())
		grpcServer.Serve(lis)
	}()

	// Finally, start the REST proxy for our gRPC server above.
	ctx := context.Background()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	mux := proxy.NewServeMux()
	proxyOpts := []grpc.DialOption{grpc.WithInsecure()}
	err = lnrpc.RegisterLightningHandlerFromEndpoint(ctx, mux, grpcEndpoint,
		proxyOpts)
	if err != nil {
		return err
	}
	go func() {
		rpcsLog.Infof("gRPC proxy started")
		http.ListenAndServe(":8080", mux)
	}()

	// If we're not in simnet mode, We'll wait until we're fully synced to
	// continue the start up of the remainder of the daemon. This ensures
	// that we don't accept any possibly invalid state transitions, or
	// accept channels with spent funds.
	if !(cfg.Bitcoin.SimNet || cfg.Litecoin.SimNet) {
		_, bestHeight, err := activeChainControl.chainIO.GetBestBlock()
		if err != nil {
			return err
		}

		ltndLog.Infof("Waiting for chain backend to finish sync, "+
			"start_height=%v", bestHeight)

		for {
			synced, err := activeChainControl.wallet.IsSynced()
			if err != nil {
				return err
			}

			if synced {
				break
			}

			time.Sleep(time.Second * 1)
		}

		_, bestHeight, err = activeChainControl.chainIO.GetBestBlock()
		if err != nil {
			return err
		}

		ltndLog.Infof("Chain backend is fully synced (end_height=%v)!",
			bestHeight)
	}

	// With all the relevant chains initialized, we can finally start the
	// server itself.
	if err := server.Start(); err != nil {
		srvrLog.Errorf("unable to create to start server: %v\n", err)
		return err
	}

	addInterruptHandler(func() {
		ltndLog.Infof("Gracefully shutting down the server...")
		rpcServer.Stop()
		fundingMgr.Stop()
		server.Stop()
		server.WaitForShutdown()
	})

	// Wait for shutdown signal from either a graceful server stop or from
	// the interrupt handler.
	<-shutdownChannel
	ltndLog.Info("Shutdown complete")
	return nil
}

func main() {
	// Use all processor cores.
	// TODO(roasbeef): remove this if required version # is > 1.6?
	runtime.GOMAXPROCS(runtime.NumCPU())

	// Call the "real" main in a nested manner so the defers will properly
	// be executed in the case of a graceful shutdown.
	if err := lndMain(); err != nil {
		if e, ok := err.(*flags.Error); ok && e.Type == flags.ErrHelp {
		} else {
			fmt.Fprintln(os.Stderr, err)
		}
		os.Exit(1)
	}
}
