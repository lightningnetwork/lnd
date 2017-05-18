package main

import (
	"fmt"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"runtime"
	"strconv"

	"golang.org/x/net/context"

	"google.golang.org/grpc"

	flags "github.com/btcsuite/go-flags"
	proxy "github.com/grpc-ecosystem/grpc-gateway/runtime"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnrpc"
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
	activeChainControl, err := newChainControlFromConfig(cfg, chanDB)
	if err != nil {
		fmt.Printf("unable to create chain control: %v\n", err)
		return err
	}

	// Finally before we start the server, we'll register the "holy
	// trinity" of interface for our current "home chain" with the active
	// chainRegistry interface.
	primaryChain := registeredChains.PrimaryChain()
	registeredChains.RegisterChain(primaryChain, activeChainControl)

	// Set up the core server which will listen for incoming peer
	// connections.
	defaultListenAddrs := []string{
		net.JoinHostPort("", strconv.Itoa(cfg.PeerPort)),
	}

	// With all the relevant chains initialized, we can finally start the
	// server itself.
	server, err := newServer(defaultListenAddrs, chanDB, activeChainControl)
	if err != nil {
		srvrLog.Errorf("unable to create server: %v\n", err)
		return err
	}
	if err := server.Start(); err != nil {
		srvrLog.Errorf("unable to create to start server: %v\n", err)
		return err
	}

	addInterruptHandler(func() {
		ltndLog.Infof("Gracefully shutting down the server...")
		server.Stop()
		server.WaitForShutdown()
	})

	// Initialize, and register our implementation of the gRPC server.
	var opts []grpc.ServerOption
	grpcServer := grpc.NewServer(opts...)
	lnrpc.RegisterLightningServer(grpcServer, server.rpcServer)

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
