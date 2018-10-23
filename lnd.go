// Copyright (c) 2013-2017 The btcsuite developers
// Copyright (c) 2015-2016 The Decred developers
// Copyright (C) 2015-2017 The Lightning Network Developers

package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
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
	"path/filepath"
	"runtime/pprof"
	"strings"
	"sync"
	"time"

	"gopkg.in/macaroon-bakery.v2/bakery"

	"golang.org/x/net/context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcwallet/wallet"
	proxy "github.com/grpc-ecosystem/grpc-gateway/runtime"
	flags "github.com/jessevdk/go-flags"

	"github.com/lightningnetwork/lnd/build"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/btcwallet"
	"github.com/lightningnetwork/lnd/macaroons"
	"github.com/lightningnetwork/lnd/signal"
	"github.com/lightningnetwork/lnd/walletunlocker"
)

const (
	// Make certificate valid for 14 months.
	autogenCertValidity = 14 /*months*/ * 30 /*days*/ * 24 * time.Hour
)

var (
	cfg              *config
	registeredChains = newChainRegistry()

	// networkDir is the path to the directory of the currently active
	// network. This path will hold the files related to each different
	// network.
	networkDir string

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
	**/
	tlsCipherSuites = []uint16{
		tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA256,
		tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
		tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
		tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305,
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
			ltndLog.Info("Shutdown complete")
			logRotator.Close()
		}
	}()

	// Show version at startup.
	ltndLog.Infof("Version: %s, build=%s, logging=%s",
		build.Version(), build.Deployment, build.LoggingType)

	var network string
	switch {
	case cfg.Bitcoin.TestNet3 || cfg.Litecoin.TestNet3:
		network = "testnet"

	case cfg.Bitcoin.MainNet || cfg.Litecoin.MainNet:
		network = "mainnet"

	case cfg.Bitcoin.SimNet:
		network = "simnet"

	case cfg.Bitcoin.RegTest:
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
			ltndLog.Errorf("Unable to create cpu profile: %v", err)
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

	// Open the channeldb, which is dedicated to storing channel, and
	// network related metadata.
	chanDB, err := channeldb.Open(graphDir)
	if err != nil {
		ltndLog.Errorf("unable to open channeldb: %v", err)
		return err
	}
	defer chanDB.Close()

	// Only process macaroons if --no-macaroons isn't set.
	ctx := context.Background()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

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
	cCreds, err := credentials.NewClientTLSFromFile(cfg.TLSCertPath, "")
	if err != nil {
		return err
	}
	proxyOpts := []grpc.DialOption{grpc.WithTransportCredentials(cCreds)}

	var (
		privateWalletPw = lnwallet.DefaultPrivatePassphrase
		publicWalletPw  = lnwallet.DefaultPublicPassphrase
		birthday        = time.Now()
		recoveryWindow  uint32
		unlockedWallet  *wallet.Wallet
	)

	// We wait until the user provides a password over RPC. In case lnd is
	// started with the --noseedbackup flag, we use the default password
	// for wallet encryption.
	if !cfg.NoSeedBackup {
		walletInitParams, err := waitForWalletPassword(
			cfg.RPCListeners, cfg.RESTListeners, serverOpts,
			proxyOpts, tlsConf,
		)
		if err != nil {
			return err
		}

		privateWalletPw = walletInitParams.Password
		publicWalletPw = walletInitParams.Password
		birthday = walletInitParams.Birthday
		recoveryWindow = walletInitParams.RecoveryWindow
		unlockedWallet = walletInitParams.Wallet

		if recoveryWindow > 0 {
			ltndLog.Infof("Wallet recovery mode enabled with "+
				"address lookahead of %d addresses",
				recoveryWindow)
		}
	}

	var macaroonService *macaroons.Service
	if !cfg.NoMacaroons {
		// Create the macaroon authentication/authorization service.
		macaroonService, err = macaroons.NewService(
			networkDir, macaroons.IPLockChecker,
		)
		if err != nil {
			srvrLog.Errorf("unable to create macaroon service: %v", err)
			return err
		}
		defer macaroonService.Close()

		// Try to unlock the macaroon store with the private password.
		err = macaroonService.CreateUnlock(&privateWalletPw)
		if err != nil {
			srvrLog.Error(err)
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
				ltndLog.Errorf("unable to create macaroon "+
					"files: %v", err)
				return err
			}
		}
	}

	// With the information parsed from the configuration, create valid
	// instances of the pertinent interfaces required to operate the
	// Lightning Network Daemon.
	activeChainControl, chainCleanUp, err := newChainControlFromConfig(
		cfg, chanDB, privateWalletPw, publicWalletPw, birthday,
		recoveryWindow, unlockedWallet,
	)
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

	// TODO(roasbeef): add rotation
	idPrivKey, err := activeChainControl.wallet.DerivePrivKey(keychain.KeyDescriptor{
		KeyLocator: keychain.KeyLocator{
			Family: keychain.KeyFamilyNodeKey,
			Index:  0,
		},
	})
	if err != nil {
		return err
	}
	idPrivKey.Curve = btcec.S256()

	if cfg.Tor.Active {
		srvrLog.Infof("Proxying all network traffic via Tor "+
			"(stream_isolation=%v)! NOTE: Ensure the backend node "+
			"is proxying over Tor as well", cfg.Tor.StreamIsolation)
	}

	// Set up the core server which will listen for incoming peer
	// connections.
	server, err := newServer(
		cfg.Listeners, chanDB, activeChainControl, idPrivKey,
	)
	if err != nil {
		srvrLog.Errorf("unable to create server: %v\n", err)
		return err
	}

	// Initialize, and register our implementation of the gRPC interface
	// exported by the rpcServer.
	rpcServer, err := newRPCServer(
		server, macaroonService, cfg.SubRpcServers, serverOpts,
		proxyOpts, tlsConf,
	)
	if err != nil {
		srvrLog.Errorf("unable to start RPC server: %v", err)
		return err
	}
	if err := rpcServer.Start(); err != nil {
		return err
	}
	defer rpcServer.Stop()

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
			if !signal.Alive() {
				return nil
			}

			synced, _, err := activeChainControl.wallet.IsSynced()
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
		srvrLog.Errorf("unable to start server: %v\n", err)
		return err
	}
	defer server.Stop()

	// Now that the server has started, if the autopilot mode is currently
	// active, then we'll initialize a fresh instance of it and start it.
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
		defer pilot.Stop()
	}

	// Wait for shutdown signal from either a graceful server stop or from
	// the interrupt handler.
	<-signal.ShutdownChannel()
	return nil
}

func main() {
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

	// Add extra IP to the slice.
	ipAddr := net.ParseIP(cfg.TLSExtraIP)
	if ipAddr != nil {
		addIP(ipAddr)
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
	if cfg.TLSExtraDomain != "" {
		dnsNames = append(dnsNames, cfg.TLSExtraDomain)
	}

	// Also add fake hostnames for unix sockets, otherwise hostname
	// verification will fail in the client.
	dnsNames = append(dnsNames, "unix", "unixpacket")

	// Generate a private key for the certificate.
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
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
		IsCA:                  true, // so can sign self.
		BasicConstraintsValid: true,

		DNSNames:    dnsNames,
		IPAddresses: ipAddresses,
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

	keybytes, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return fmt.Errorf("unable to encode privkey: %v", err)
	}
	keyBuf := &bytes.Buffer{}
	err = pem.Encode(keyBuf, &pem.Block{Type: "EC PRIVATE KEY",
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
}

// waitForWalletPassword will spin up gRPC and REST endpoints for the
// WalletUnlocker server, and block until a password is provided by
// the user to this RPC server.
func waitForWalletPassword(grpcEndpoints, restEndpoints []net.Addr,
	serverOpts []grpc.ServerOption, proxyOpts []grpc.DialOption,
	tlsConf *tls.Config) (*WalletUnlockParams, error) {

	// Set up a new PasswordService, which will listen for passwords
	// provided over RPC.
	grpcServer := grpc.NewServer(serverOpts...)

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
		chainConfig.ChainDir, activeNetParams.Params, macaroonFiles,
	)
	lnrpc.RegisterWalletUnlockerServer(grpcServer, pwService)

	// Use a WaitGroup so we can be sure the instructions on how to input the
	// password is the last thing to be printed to the console.
	var wg sync.WaitGroup

	for _, grpcEndpoint := range grpcEndpoints {
		// Start a gRPC server listening for HTTP/2 connections, solely
		// used for getting the encryption password from the client.
		lis, err := lncfg.ListenOnAddress(grpcEndpoint)
		if err != nil {
			ltndLog.Errorf(
				"password RPC server unable to listen on %s",
				grpcEndpoint,
			)
			return nil, err
		}
		defer lis.Close()

		wg.Add(1)
		go func() {
			rpcsLog.Infof(
				"password RPC server listening on %s",
				lis.Addr(),
			)
			wg.Done()
			grpcServer.Serve(lis)
		}()
	}

	// Start a REST proxy for our gRPC server above.
	ctx := context.Background()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	mux := proxy.NewServeMux()

	err := lnrpc.RegisterWalletUnlockerHandlerFromEndpoint(
		ctx, mux, grpcEndpoints[0].String(), proxyOpts,
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
			activeNetParams.Params, netDir, uint32(recoveryWindow),
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

		walletInitParams := &WalletUnlockParams{
			Password:       password,
			Birthday:       birthday,
			RecoveryWindow: recoveryWindow,
			Wallet:         newWallet,
		}

		return walletInitParams, nil

	// The wallet has already been created in the past, and is simply being
	// unlocked. So we'll just return these passphrases.
	case unlockMsg := <-pwService.UnlockMsgs:
		walletInitParams := &WalletUnlockParams{
			Password:       unlockMsg.Passphrase,
			RecoveryWindow: unlockMsg.RecoveryWindow,
			Wallet:         unlockMsg.Wallet,
		}
		return walletInitParams, nil

	case <-signal.ShutdownChannel():
		return nil, fmt.Errorf("shutting down")
	}
}
