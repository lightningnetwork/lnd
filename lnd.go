package main

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io/ioutil"
	"math/big"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"runtime"
	"runtime/pprof"
	"strconv"
	"time"

	"gopkg.in/macaroon-bakery.v1/bakery"

	"golang.org/x/net/context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	flags "github.com/btcsuite/go-flags"
	proxy "github.com/grpc-ecosystem/grpc-gateway/runtime"
	"github.com/lightningnetwork/lnd/autopilot"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/macaroons"
	"github.com/lightningnetwork/lnd/walletunlocker"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcutil"
)

const (
	// Make certificate valid for 14 months.
	autogenCertValidity = 14 /*months*/ * 30 /*days*/ * 24 * time.Hour
)

var (
	cfg              *config
	shutdownChannel  = make(chan struct{})
	registeredChains = newChainRegistry()

	macaroonDatabaseDir string

	// End of ASN.1 time.
	endOfTime = time.Date(2049, 12, 31, 23, 59, 59, 0, time.UTC)

	// Max serial number.
	serialNumberLimit = new(big.Int).Lsh(big.NewInt(1), 128)

	/*
	 * These cipher suites fit the following criteria:
	 * - Don't use outdated algorithms like SHA-1 and 3DES
	 * - Don't use ECB mode or other insecure symmetric methods
	 * - Included in the TLS v1.2 suite
	 * - Are available in the Go 1.7.6 standard library (more are
	 *   available in 1.8.3 and will be added after lnd no longer
	 *   supports 1.7, including suites that support CBC mode)
	 *
	 * The cipher suites are ordered from strongest to weakest
	 * primitives, but the client's preference order has more
	 * effect during negotiation.
	**/
	tlsCipherSuites = []uint16{
		tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
		tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
		tls.TLS_RSA_WITH_AES_256_GCM_SHA384,
		tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA256,
		tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA256,
		tls.TLS_RSA_WITH_AES_128_CBC_SHA256,
		tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
		tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
		tls.TLS_RSA_WITH_AES_128_GCM_SHA256,
		tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305,
		tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305,
	}
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
	defer func() {
		if logRotator != nil {
			logRotator.Close()
		}
	}()

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

	// Write cpu profile if requested.
	if cfg.CPUProfile != "" {
		f, err := os.Create(cfg.CPUProfile)
		if err != nil {
			ltndLog.Errorf("Unable to create cpu profile: %v", err)
			return err
		}
		pprof.StartCPUProfile(f)
		defer f.Close()
		defer pprof.StopCPUProfile()
	}

	// Open the channeldb, which is dedicated to storing channel, and
	// network related metadata.
	chanDB, err := channeldb.Open(cfg.DataDir)
	if err != nil {
		ltndLog.Errorf("unable to open channeldb: %v", err)
		return err
	}
	defer chanDB.Close()

	// Only process macaroons if --no-macaroons isn't set.
	var macaroonService *bakery.Service
	if !cfg.NoMacaroons {
		// Create the macaroon authentication/authorization service.
		macaroonService, err = macaroons.NewService(macaroonDatabaseDir)
		if err != nil {
			srvrLog.Errorf("unable to create macaroon service: %v", err)
			return err
		}

		// Create macaroon files for lncli to use if they don't exist.
		if !fileExists(cfg.AdminMacPath) && !fileExists(cfg.ReadMacPath) {
			err = genMacaroons(macaroonService, cfg.AdminMacPath,
				cfg.ReadMacPath)
			if err != nil {
				ltndLog.Errorf("unable to create macaroon "+
					"files: %v", err)
				return err
			}
		}
	}

	// Ensure we create TLS key and certificate if they don't exist
	if !fileExists(cfg.TLSCertPath) && !fileExists(cfg.TLSKeyPath) {
		if err := genCertPair(cfg.TLSCertPath, cfg.TLSKeyPath); err != nil {
			return err
		}
	}

	cert, err := tls.LoadX509KeyPair(cfg.TLSCertPath, cfg.TLSKeyPath)
	if err != nil {
		return err
	}
	tlsConf := &tls.Config{
		Certificates: []tls.Certificate{cert},
		CipherSuites: tlsCipherSuites,
		MinVersion:   tls.VersionTLS12,
	}
	sCreds := credentials.NewTLS(tlsConf)
	serverOpts := []grpc.ServerOption{grpc.Creds(sCreds)}
	grpcEndpoint := fmt.Sprintf("localhost:%d", loadedConfig.RPCPort)
	restEndpoint := fmt.Sprintf(":%d", loadedConfig.RESTPort)
	cCreds, err := credentials.NewClientTLSFromFile(cfg.TLSCertPath,
		"")
	if err != nil {
		return err
	}
	proxyOpts := []grpc.DialOption{grpc.WithTransportCredentials(cCreds)}

	// We wait until the user provides a password over RPC. In case lnd is
	// started with the --noencryptwallet flag, we use the default password
	// "hello" for wallet encryption.
	walletPw := []byte("hello")
	if !cfg.NoEncryptWallet {
		walletPw, err = waitForWalletPassword(grpcEndpoint, restEndpoint,
			serverOpts, proxyOpts, tlsConf, macaroonService)
		if err != nil {
			return err
		}
	}

	// With the information parsed from the configuration, create valid
	// instances of the pertinent interfaces required to operate the
	// Lightning Network Daemon.
	activeChainControl, chainCleanUp, err := newChainControlFromConfig(cfg,
		chanDB, walletPw)
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

	// Next, we'll initialize the funding manager itself so it can answer
	// queries while the wallet+chain are still syncing.
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
		CurrentNodeAnnouncement: func() (lnwire.NodeAnnouncement, error) {
			return server.genNodeAnnouncement(true)
		},
		SendAnnouncement: func(msg lnwire.Message) error {
			errChan := server.authGossiper.ProcessLocalAnnouncement(msg,
				idPrivKey.PubKey())
			return <-errChan
		},
		ArbiterChan:      server.breachArbiter.newContracts,
		SendToPeer:       server.SendToPeer,
		NotifyWhenOnline: server.NotifyWhenOnline,
		FindPeer:         server.FindPeer,
		TempChanIDSeed:   chanIDSeed,
		FindChannel: func(chanID lnwire.ChannelID) (*lnwallet.LightningChannel, error) {
			dbChannels, err := chanDB.FetchAllChannels()
			if err != nil {
				return nil, err
			}

			for _, channel := range dbChannels {
				if chanID.IsChanPoint(&channel.FundingOutpoint) {
					return lnwallet.NewLightningChannel(
						activeChainControl.signer,
						activeChainControl.chainNotifier,
						activeChainControl.feeEstimator,
						channel)
				}
			}

			return nil, fmt.Errorf("unable to find channel")
		},
		DefaultRoutingPolicy: activeChainControl.routingPolicy,
		NumRequiredConfs: func(chanAmt btcutil.Amount, pushAmt lnwire.MilliSatoshi) uint16 {
			// TODO(roasbeef): add configurable mapping
			//  * simple switch initially
			//  * assign coefficient, etc
			return uint16(cfg.DefaultNumChanConfs)
		},
		RequiredRemoteDelay: func(chanAmt btcutil.Amount) uint16 {
			// TODO(roasbeef): add additional hooks for
			// configuration
			return 4
		},
	})
	if err != nil {
		return err
	}
	if err := fundingMgr.Start(); err != nil {
		return err
	}
	server.fundingMgr = fundingMgr

	// Initialize, and register our implementation of the gRPC interface
	// exported by the rpcServer.
	rpcServer := newRPCServer(server, macaroonService)
	if err := rpcServer.Start(); err != nil {
		return err
	}

	grpcServer := grpc.NewServer(serverOpts...)
	lnrpc.RegisterLightningServer(grpcServer, rpcServer)

	// Next, Start the gRPC server listening for HTTP/2 connections.
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
	err = lnrpc.RegisterLightningHandlerFromEndpoint(ctx, mux, grpcEndpoint,
		proxyOpts)
	if err != nil {
		return err
	}
	go func() {
		listener, err := tls.Listen("tcp", restEndpoint, tlsConf)
		if err != nil {
			ltndLog.Errorf("gRPC proxy unable to listen on "+
				"localhost%s", restEndpoint)
			return
		}
		rpcsLog.Infof("gRPC proxy started at localhost%s", restEndpoint)
		http.Serve(listener, mux)
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

	// Now that the server has started, if the autopilot mode is currently
	// active, then we'll initialize a fresh instance of it and start it.
	var pilot *autopilot.Agent
	if cfg.Autopilot.Active {
		pilot, err := initAutoPilot(server, cfg.Autopilot)
		if err != nil {
			ltndLog.Errorf("unable to create autopilot agent: %v",
				err)
			return err
		}
		if err := pilot.Start(); err != nil {
			ltndLog.Errorf("unable to start autopilot agent: %v",
				err)
			return err
		}
	}

	addInterruptHandler(func() {
		ltndLog.Infof("Gracefully shutting down the server...")
		rpcServer.Stop()
		fundingMgr.Stop()
		server.Stop()

		if pilot != nil {
			pilot.Stop()
		}

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

// genCertPair generates a key/cert pair to the paths provided. The
// auto-generated certificates should *not* be used in production for public
// access as they're self-signed and don't necessarily contain all of the
// desired hostnames for the service. For production/public use, consider a
// real PKI.
//
// This function is adapted from https://github.com/btcsuite/btcd and
// https://github.com/btcsuite/btcutil
func genCertPair(certFile, keyFile string) error {
	rpcsLog.Infof("Generating TLS certificates...")

	org := "lnd autogenerated cert"
	now := time.Now()
	validUntil := now.Add(autogenCertValidity)

	// Check that the certificate validity isn't past the ASN.1 end of time.
	if validUntil.After(endOfTime) {
		validUntil = endOfTime
	}

	// Generate a serial number that's below the serialNumberLimit.
	serialNumber, err := rand.Int(rand.Reader, serialNumberLimit)
	if err != nil {
		return fmt.Errorf("failed to generate serial number: %s", err)
	}

	// Collect the host's IP addresses, including loopback, in a slice.
	ipAddresses := []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}

	// addIP appends an IP address only if it isn't already in the slice.
	addIP := func(ipAddr net.IP) {
		for _, ip := range ipAddresses {
			if bytes.Equal(ip, ipAddr) {
				return
			}
		}
		ipAddresses = append(ipAddresses, ipAddr)
	}

	// Add all the interface IPs that aren't already in the slice.
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return err
	}
	for _, a := range addrs {
		ipAddr, _, err := net.ParseCIDR(a.String())
		if err == nil {
			addIP(ipAddr)
		}
	}

	// Collect the host's names into a slice.
	host, err := os.Hostname()
	if err != nil {
		return err
	}
	dnsNames := []string{host}
	if host != "localhost" {
		dnsNames = append(dnsNames, "localhost")
	}

	// Generate a private key for the certificate.
	priv, err := rsa.GenerateKey(rand.Reader, 4096)
	if err != nil {
		return err
	}

	// Construct the certificate template.
	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{org},
			CommonName:   host,
		},
		NotBefore: now.Add(-time.Hour * 24),
		NotAfter:  validUntil,

		KeyUsage: x509.KeyUsageKeyEncipherment |
			x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		IsCA: true, // so can sign self.
		BasicConstraintsValid: true,

		DNSNames:    dnsNames,
		IPAddresses: ipAddresses,

		// This signature algorithm is most likely to be compatible
		// with clients using less-common TLS libraries like BoringSSL.
		SignatureAlgorithm: x509.SHA256WithRSA,
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, &template,
		&template, &priv.PublicKey, priv)
	if err != nil {
		return fmt.Errorf("failed to create certificate: %v", err)
	}

	certBuf := &bytes.Buffer{}
	err = pem.Encode(certBuf, &pem.Block{Type: "CERTIFICATE",
		Bytes: derBytes})
	if err != nil {
		return fmt.Errorf("failed to encode certificate: %v", err)
	}

	keybytes := x509.MarshalPKCS1PrivateKey(priv)
	keyBuf := &bytes.Buffer{}
	err = pem.Encode(keyBuf, &pem.Block{Type: "RSA PRIVATE KEY",
		Bytes: keybytes})
	if err != nil {
		return fmt.Errorf("failed to encode private key: %v", err)
	}

	// Write cert and key files.
	if err = ioutil.WriteFile(certFile, certBuf.Bytes(), 0644); err != nil {
		return err
	}
	if err = ioutil.WriteFile(keyFile, keyBuf.Bytes(), 0600); err != nil {
		os.Remove(certFile)
		return err
	}

	rpcsLog.Infof("Done generating TLS certificates")
	return nil
}

// genMacaroons generates a pair of macaroon files; one admin-level and one
// read-only. These can also be used to generate more granular macaroons.
func genMacaroons(svc *bakery.Service, admFile, roFile string) error {
	// Generate the admin macaroon and write it to a file.
	admMacaroon, err := svc.NewMacaroon("", nil, nil)
	if err != nil {
		return err
	}
	admBytes, err := admMacaroon.MarshalBinary()
	if err != nil {
		return err
	}
	if err = ioutil.WriteFile(admFile, admBytes, 0600); err != nil {
		return err
	}

	// Generate the read-only macaroon and write it to a file.
	roMacaroon, err := macaroons.AddConstraints(admMacaroon,
		macaroons.AllowConstraint(roPermissions...))
	if err != nil {
		return err
	}
	roBytes, err := roMacaroon.MarshalBinary()
	if err != nil {
		return err
	}
	if err = ioutil.WriteFile(roFile, roBytes, 0644); err != nil {
		os.Remove(admFile)
		return err
	}

	return nil
}

// waitForWalletPassword will spin up gRPC and REST endpoints for the
// WalletUnlocker server, and block until a password is provided by
// the user to this RPC server.
func waitForWalletPassword(grpcEndpoint, restEndpoint string,
	serverOpts []grpc.ServerOption, proxyOpts []grpc.DialOption,
	tlsConf *tls.Config, macaroonService *bakery.Service) ([]byte, error) {
	// Set up a new PasswordService, which will listen
	// for passwords provided over RPC.
	grpcServer := grpc.NewServer(serverOpts...)

	chainConfig := cfg.Bitcoin
	if registeredChains.PrimaryChain() == litecoinChain {
		chainConfig = cfg.Litecoin
	}
	pwService := walletunlocker.New(macaroonService,
		chainConfig.ChainDir, activeNetParams.Params)
	lnrpc.RegisterWalletUnlockerServer(grpcServer, pwService)

	// Start a gRPC server listening for HTTP/2 connections, solely
	// used for getting the encryption password from the client.
	lis, err := net.Listen("tcp", grpcEndpoint)
	if err != nil {
		fmt.Printf("failed to listen: %v", err)
		return nil, err
	}
	defer lis.Close()

	// Use a two channels to synchronize on, so we can be sure the
	// instructions on how to input the password is the last
	// thing to be printed to the console.
	grpcServing := make(chan struct{})
	restServing := make(chan struct{})

	go func(c chan struct{}) {
		rpcsLog.Infof("password RPC server listening on %s",
			lis.Addr())
		close(c)
		grpcServer.Serve(lis)
	}(grpcServing)

	// Start a REST proxy for our gRPC server above.
	ctx := context.Background()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	mux := proxy.NewServeMux()
	err = lnrpc.RegisterWalletUnlockerHandlerFromEndpoint(ctx, mux,
		grpcEndpoint, proxyOpts)
	if err != nil {
		return nil, err
	}
	srv := &http.Server{Handler: mux}
	defer func() {
		// We must shut down this server, since we'll let
		// the regular rpcServer listen on the same address.
		if err := srv.Shutdown(ctx); err != nil {
			ltndLog.Errorf("unable to shutdown gPRC proxy: %v", err)
		}
	}()

	go func(c chan struct{}) {
		listener, err := tls.Listen("tcp", restEndpoint,
			tlsConf)
		if err != nil {
			ltndLog.Errorf("gRPC proxy unable to listen "+
				"on localhost%s", restEndpoint)
			return
		}
		rpcsLog.Infof("password gRPC proxy started at "+
			"localhost%s", restEndpoint)
		close(c)
		srv.Serve(listener)
	}(restServing)

	// Wait for gRPC and REST server to be up running.
	<-grpcServing
	<-restServing

	// Wait for user to provide the password.
	ltndLog.Infof("Waiting for wallet encryption password. " +
		"Use `lncli create` to create wallet, or " +
		"`lncli unlock` to unlock already created wallet.")

	// We currently don't distinguish between getting a password to
	// be used for creation or unlocking, as a new wallet db will be
	// created if none exists when creating the chain control.
	select {
	case walletPw := <-pwService.CreatePasswords:
		return walletPw, nil
	case walletPw := <-pwService.UnlockPasswords:
		return walletPw, nil
	case <-shutdownChannel:
		return nil, fmt.Errorf("shutting down")
	}
}
