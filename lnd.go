package main

import (
	"flag"
	"fmt"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"

	"google.golang.org/grpc"
	"google.golang.org/grpc/grpclog"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
)

var (
	rpcPort  = flag.Int("rpcport", 10009, "The port for the rpc server")
	peerPort = flag.String("peerport", "10011", "The port to listen on for incoming p2p connections")
	dataDir  = flag.String("datadir", "test_wal", "The directory to store lnd's data within")
	spvMode  = flag.Bool("spv", false, "assert to enter spv wallet mode")
)

func main() {
	flag.Parse()

	if *spvMode == true {
		shell()
		return
	}

	go func() {
		listenAddr := net.JoinHostPort("", "5009")
		profileRedirect := http.RedirectHandler("/debug/pprof",
			http.StatusSeeOther)
		http.Handle("/", profileRedirect)
		fmt.Println(http.ListenAndServe(listenAddr, nil))
	}()

	// Create, and start the lnwallet, which handles the core payment channel
	// logic, and exposes control via proxy state machines.
	// TODO(roasbeef): accept config via cli flags, move to real config file
	// afterwards
	config := &lnwallet.Config{PrivatePass: []byte("hello"), DataDir: *dataDir}

	lnwallet, db, err := lnwallet.NewLightningWallet(config)
	if err != nil {
		fmt.Printf("unable to create wallet: %v\n", err)
		os.Exit(1)
	}

	if err := lnwallet.Startup(); err != nil {
		fmt.Printf("unable to start wallet: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("wallet open")
	defer db.Close()

	// Set up the core server which will listen for incoming peer
	// connections.
	defaultListenAddr := []string{net.JoinHostPort("", *peerPort)}
	server, err := newServer(defaultListenAddr, &chaincfg.TestNet3Params,
		lnwallet)
	if err != nil {
		fmt.Printf("unable to create server: %v\n", err)
		os.Exit(1)
	}
	server.Start()

	// Initialize, and register our implementation of the gRPC server.
	var opts []grpc.ServerOption
	grpcServer := grpc.NewServer(opts...)
	lnrpc.RegisterLightningServer(grpcServer, server.rpcServer)

	// Finally, start the grpc server listening for HTTP/2 connections.
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", *rpcPort))
	if err != nil {
		grpclog.Fatalf("failed to listen: %v", err)
		fmt.Printf("failed to listen: %v", err)
		os.Exit(1)
	}
	grpcServer.Serve(lis)
}
