package main

import (
	"fmt"
	"google.golang.org/grpc"
	"google.golang.org/grpc/grpclog"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"strconv"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
)

func main() {

	loadedConfig, err := loadConfig()

	if err != nil {
		fmt.Printf("unable to load config: %v\n", err)
		os.Exit(1)
	}

	if loadedConfig.SPVMode == true {
		shell(loadedConfig.SPVHostAdr, loadedConfig.NetParams)
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
	config := &lnwallet.Config{
		PrivatePass: []byte("hello"),
		DataDir:     loadedConfig.DataDir,
		RpcHost:     loadedConfig.BTCDHost,
		RpcUser:     loadedConfig.BTCDUser,
		RpcPass:     loadedConfig.BTCDPass,
		RpcNoTLS:    loadedConfig.BTCDNoTLS,
		CACert:      loadedConfig.BTCDCACert,
		NetParams:   loadedConfig.NetParams,
	}
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
	defaultListenAddr := []string{net.JoinHostPort("", strconv.Itoa(loadedConfig.PeerPort))}
	server, err := newServer(defaultListenAddr, loadedConfig.NetParams,
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
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", loadedConfig.RPCPort))
	if err != nil {
		grpclog.Fatalf("failed to listen: %v", err)
		fmt.Printf("failed to listen: %v", err)
		os.Exit(1)
	}
	grpcServer.Serve(lis)
}
