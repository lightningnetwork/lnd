package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/grpclog"

	"li.lan/labs/plasma/lnrpc"
	"li.lan/labs/plasma/lnwallet"
)

//lightning == terrestrial plasma

var (
	rpcport = flag.Int("port", 10000, "The port for the rpc server")
)

func main() {
	flag.Parse()

	// Create, and start the lnwallet, which handles the core payment channel
	// logic, and exposes control via proxy state machines.
	// TODO(roasbeef): accept config via cli flags, move to real config file
	// afterwards
	config := &lnwallet.Config{PrivatePass: []byte("hello"), DataDir: "test_wal"}
	lnwallet, err := lnwallet.NewLightningWallet(config)
	if err != nil {
		fmt.Printf("unable to create wallet: %v\n", err)
		os.Exit(1)
	}
	if err := lnwallet.Startup(); err != nil {
		fmt.Printf("unable to start wallet: %v\n", err)
		os.Exit(1)
	}
	lnwallet.Unlock(cf.PrivatePass, time.Duration(0))
	fmt.Println("wallet open")

	// Initialize, and register our implementation of the gRPC server.
	var opts []grpc.ServerOption
	rpcServer := newRpcServer(lnwallet)
	// start message handler for incoming LN messages
	go OmniHandler(rpcServer)
	grpcServer := grpc.NewServer(opts...)
	lnrpc.RegisterLightningServer(grpcServer, rpcServer)

	// Finally, start the grpc server listening for HTTP/2 connections.
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", *rpcport))
	if err != nil {
		grpclog.Fatalf("failed to listen: %v", err)
		fmt.Printf("failed to listen: %v", err)
		os.Exit(1)
	}
	grpcServer.Serve(lis)
}
