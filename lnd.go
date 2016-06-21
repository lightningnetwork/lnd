package main

import (
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"path/filepath"
	"runtime"
	"strconv"

	"google.golang.org/grpc"
	"google.golang.org/grpc/grpclog"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
)

var (
	cfg             *config
	shutdownChannel = make(chan struct{})
)

func main() {
	// Use all processor cores.
	runtime.GOMAXPROCS(runtime.NumCPU())

	// Load the configuration, and parse any command line options. This
	// function will also set up logging properly.
	loadedConfig, err := loadConfig()
	if err != nil {
		fmt.Printf("unable to load config: %v\n", err)
		os.Exit(1)
	}
	cfg = loadedConfig
	defer backendLog.Flush()

	// Show version at startup.
	ltndLog.Infof("Version %s", version())

	if loadedConfig.SPVMode == true {
		shell(loadedConfig.SPVHostAdr, activeNetParams)
		return
	}

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
	// network related meta-data.
	chanDB, err := channeldb.Open(loadedConfig.DataDir, activeNetParams)
	if err != nil {
		fmt.Println("unable to open channeldb: ", err)
		os.Exit(1)
	}
	defer chanDB.Close()

	// Read btcd's for lnwallet's convenience.
	f, err := os.Open(loadedConfig.RPCCert)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	cert, err := ioutil.ReadAll(f)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer f.Close()

	// Create, and start the lnwallet, which handles the core payment channel
	// logic, and exposes control via proxy state machines.
	config := &lnwallet.Config{
		PrivatePass: []byte("hello"),
		DataDir:     filepath.Join(loadedConfig.DataDir, "lnwallet"),
		RpcHost:     loadedConfig.RPCHost,
		RpcUser:     loadedConfig.RPCUser,
		RpcPass:     loadedConfig.RPCPass,
		CACert:      cert,
		NetParams:   activeNetParams,
	}
	wallet, err := lnwallet.NewLightningWallet(config, chanDB)
	if err != nil {
		fmt.Printf("unable to create wallet: %v\n", err)
		os.Exit(1)
	}
	if err := wallet.Startup(); err != nil {
		fmt.Printf("unable to start wallet: %v\n", err)
		os.Exit(1)
	}
	ltndLog.Info("LightningWallet opened")

	ec := &lnwallet.WaddrmgrEncryptorDecryptor{wallet.Manager}
	chanDB.RegisterCryptoSystem(ec)

	// Set up the core server which will listen for incoming peer
	// connections.
	defaultListenAddrs := []string{
		net.JoinHostPort("", strconv.Itoa(loadedConfig.PeerPort)),
	}
	server, err := newServer(defaultListenAddrs, wallet, chanDB)
	if err != nil {
		srvrLog.Errorf("unable to create server: %v\n", err)
		os.Exit(1)
	}
	server.Start()

	addInterruptHandler(func() {
		ltndLog.Infof("Gracefully shutting down the server...")
		server.Stop()
		server.WaitForShutdown()
	})

	// Initialize, and register our implementation of the gRPC server.
	var opts []grpc.ServerOption
	grpcServer := grpc.NewServer(opts...)
	lnrpc.RegisterLightningServer(grpcServer, server.rpcServer)

	// Finally, start the grpc server listening for HTTP/2 connections.
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", loadedConfig.RPCPort))
	if err != nil {
		grpclog.Fatalf("failed to listen: %v", err)
		fmt.Printf("failed to listen: %v", err)
		os.Exit(1)
	}
	go func() {
		rpcsLog.Infof("RPC server listening on %s", lis.Addr())
		grpcServer.Serve(lis)
	}()

	// Wait for shutdown signal from either a graceful server stop or from
	// the interrupt handler.
	<-shutdownChannel
	ltndLog.Info("Shutdown complete")
}
