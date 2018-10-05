package main

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/coreos/bbolt"
	"github.com/davecgh/go-spew/spew"
	proxy "github.com/grpc-ecosystem/grpc-gateway/runtime"
	"github.com/lightningnetwork/lnd/autopilot"
	"github.com/lightningnetwork/lnd/build"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/channelnotifier"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/macaroons"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/lightningnetwork/lnd/signal"
	"github.com/lightningnetwork/lnd/sweep"
	"github.com/lightningnetwork/lnd/zpay32"
	"github.com/tv42/zbase32"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"gopkg.in/macaroon-bakery.v2/bakery"
)

const (
	// maxBtcPaymentMSat is the maximum allowed Bitcoin payment currently
	// permitted as defined in BOLT-0002.
	maxBtcPaymentMSat = lnwire.MilliSatoshi(math.MaxUint32)

	// maxLtcPaymentMSat is the maximum allowed Litecoin payment currently
	// permitted.
	maxLtcPaymentMSat = lnwire.MilliSatoshi(math.MaxUint32) *
		btcToLtcConversionRate
)

var (
	zeroHash [32]byte

	// maxPaymentMSat is the maximum allowed payment currently permitted as
	// defined in BOLT-002. This value depends on which chain is active.
	// It is set to the value under the Bitcoin chain as default.
	maxPaymentMSat = maxBtcPaymentMSat

	defaultAccount uint32 = waddrmgr.DefaultAccountNum

	// readPermissions is a slice of all entities that allow read
	// permissions for authorization purposes, all lowercase.
	readPermissions = []bakery.Op{
		{
			Entity: "onchain",
			Action: "read",
		},
		{
			Entity: "offchain",
			Action: "read",
		},
		{
			Entity: "address",
			Action: "read",
		},
		{
			Entity: "message",
			Action: "read",
		},
		{
			Entity: "peers",
			Action: "read",
		},
		{
			Entity: "info",
			Action: "read",
		},
		{
			Entity: "invoices",
			Action: "read",
		},
	}

	// writePermissions is a slice of all entities that allow write
	// permissions for authorization purposes, all lowercase.
	writePermissions = []bakery.Op{
		{
			Entity: "onchain",
			Action: "write",
		},
		{
			Entity: "offchain",
			Action: "write",
		},
		{
			Entity: "address",
			Action: "write",
		},
		{
			Entity: "message",
			Action: "write",
		},
		{
			Entity: "peers",
			Action: "write",
		},
		{
			Entity: "info",
			Action: "write",
		},
		{
			Entity: "invoices",
			Action: "write",
		},
		{
			Entity: "signer",
			Action: "generate",
		},
	}

	// invoicePermissions is a slice of all the entities that allows a user
	// to only access calls that are related to invoices, so: streaming
	// RPCs, generating, and listening invoices.
	invoicePermissions = []bakery.Op{
		{
			Entity: "invoices",
			Action: "read",
		},
		{
			Entity: "invoices",
			Action: "write",
		},
		{
			Entity: "address",
			Action: "read",
		},
		{
			Entity: "address",
			Action: "write",
		},
	}

	// permissions maps RPC calls to the permissions they require.
	permissions = map[string][]bakery.Op{
		"/lnrpc.Lightning/SendCoins": {{
			Entity: "onchain",
			Action: "write",
		}},
		"/lnrpc.Lightning/ListUnspent": {{
			Entity: "onchain",
			Action: "read",
		}},
		"/lnrpc.Lightning/SendMany": {{
			Entity: "onchain",
			Action: "write",
		}},
		"/lnrpc.Lightning/NewAddress": {{
			Entity: "address",
			Action: "write",
		}},
		"/lnrpc.Lightning/SignMessage": {{
			Entity: "message",
			Action: "write",
		}},
		"/lnrpc.Lightning/VerifyMessage": {{
			Entity: "message",
			Action: "read",
		}},
		"/lnrpc.Lightning/ConnectPeer": {{
			Entity: "peers",
			Action: "write",
		}},
		"/lnrpc.Lightning/DisconnectPeer": {{
			Entity: "peers",
			Action: "write",
		}},
		"/lnrpc.Lightning/OpenChannel": {{
			Entity: "onchain",
			Action: "write",
		}, {
			Entity: "offchain",
			Action: "write",
		}},
		"/lnrpc.Lightning/OpenChannelSync": {{
			Entity: "onchain",
			Action: "write",
		}, {
			Entity: "offchain",
			Action: "write",
		}},
		"/lnrpc.Lightning/CloseChannel": {{
			Entity: "onchain",
			Action: "write",
		}, {
			Entity: "offchain",
			Action: "write",
		}},
		"/lnrpc.Lightning/AbandonChannel": {{
			Entity: "offchain",
			Action: "write",
		}},
		"/lnrpc.Lightning/GetInfo": {{
			Entity: "info",
			Action: "read",
		}},
		"/lnrpc.Lightning/ListPeers": {{
			Entity: "peers",
			Action: "read",
		}},
		"/lnrpc.Lightning/WalletBalance": {{
			Entity: "onchain",
			Action: "read",
		}},
		"/lnrpc.Lightning/ChannelBalance": {{
			Entity: "offchain",
			Action: "read",
		}},
		"/lnrpc.Lightning/PendingChannels": {{
			Entity: "offchain",
			Action: "read",
		}},
		"/lnrpc.Lightning/ListChannels": {{
			Entity: "offchain",
			Action: "read",
		}},
		"/lnrpc.Lightning/SubscribeChannelEvents": {{
			Entity: "offchain",
			Action: "read",
		}},
		"/lnrpc.Lightning/ClosedChannels": {{
			Entity: "offchain",
			Action: "read",
		}},
		"/lnrpc.Lightning/SendPayment": {{
			Entity: "offchain",
			Action: "write",
		}},
		"/lnrpc.Lightning/SendPaymentSync": {{
			Entity: "offchain",
			Action: "write",
		}},
		"/lnrpc.Lightning/SendToRoute": {{
			Entity: "offchain",
			Action: "write",
		}},
		"/lnrpc.Lightning/SendToRouteSync": {{
			Entity: "offchain",
			Action: "write",
		}},
		"/lnrpc.Lightning/AddInvoice": {{
			Entity: "invoices",
			Action: "write",
		}},
		"/lnrpc.Lightning/LookupInvoice": {{
			Entity: "invoices",
			Action: "read",
		}},
		"/lnrpc.Lightning/ListInvoices": {{
			Entity: "invoices",
			Action: "read",
		}},
		"/lnrpc.Lightning/SubscribeInvoices": {{
			Entity: "invoices",
			Action: "read",
		}},
		"/lnrpc.Lightning/SubscribeTransactions": {{
			Entity: "onchain",
			Action: "read",
		}},
		"/lnrpc.Lightning/GetTransactions": {{
			Entity: "onchain",
			Action: "read",
		}},
		"/lnrpc.Lightning/DescribeGraph": {{
			Entity: "info",
			Action: "read",
		}},
		"/lnrpc.Lightning/GetChanInfo": {{
			Entity: "info",
			Action: "read",
		}},
		"/lnrpc.Lightning/GetNodeInfo": {{
			Entity: "info",
			Action: "read",
		}},
		"/lnrpc.Lightning/QueryRoutes": {{
			Entity: "info",
			Action: "read",
		}},
		"/lnrpc.Lightning/GetNetworkInfo": {{
			Entity: "info",
			Action: "read",
		}},
		"/lnrpc.Lightning/StopDaemon": {{
			Entity: "info",
			Action: "write",
		}},
		"/lnrpc.Lightning/SubscribeChannelGraph": {{
			Entity: "info",
			Action: "read",
		}},
		"/lnrpc.Lightning/ListPayments": {{
			Entity: "offchain",
			Action: "read",
		}},
		"/lnrpc.Lightning/DeleteAllPayments": {{
			Entity: "offchain",
			Action: "write",
		}},
		"/lnrpc.Lightning/DebugLevel": {{
			Entity: "info",
			Action: "write",
		}},
		"/lnrpc.Lightning/DecodePayReq": {{
			Entity: "offchain",
			Action: "read",
		}},
		"/lnrpc.Lightning/FeeReport": {{
			Entity: "offchain",
			Action: "read",
		}},
		"/lnrpc.Lightning/UpdateChannelPolicy": {{
			Entity: "offchain",
			Action: "write",
		}},
		"/lnrpc.Lightning/ForwardingHistory": {{
			Entity: "offchain",
			Action: "read",
		}},
	}
)

// rpcServer is a gRPC, RPC front end to the lnd daemon.
// TODO(roasbeef): pagination support for the list-style calls
type rpcServer struct {
	started  int32 // To be used atomically.
	shutdown int32 // To be used atomically.

	server *server

	wg sync.WaitGroup

	// subServers are a set of sub-RPC servers that use the same gRPC and
	// listening sockets as the main RPC server, but which maintain their
	// own independent service. This allows us to expose a set of
	// micro-service like abstractions to the outside world for users to
	// consume.
	subServers []lnrpc.SubServer

	// grpcServer is the main gRPC server that this RPC server, and all the
	// sub-servers will use to register themselves and accept client
	// requests from.
	grpcServer *grpc.Server

	// listenerCleanUp are a set of closures functions that will allow this
	// main RPC server to clean up all the listening socket created for the
	// server.
	listenerCleanUp []func()

	// restServerOpts are a set of gRPC dial options that the REST server
	// proxy will use to connect to the main gRPC server.
	restServerOpts []grpc.DialOption

	// tlsCfg is the TLS config that allows the REST server proxy to
	// connect to the main gRPC server to proxy all incoming requests.
	tlsCfg *tls.Config

	quit chan struct{}
}

// A compile time check to ensure that rpcServer fully implements the
// LightningServer gRPC service.
var _ lnrpc.LightningServer = (*rpcServer)(nil)

// newRPCServer creates and returns a new instance of the rpcServer. The
// rpcServer will handle creating all listening sockets needed by it, and any
// of the sub-servers that it maintains. The set of serverOpts should be the
// base level options passed to the grPC server. This typically includes things
// like requiring TLS, etc.
func newRPCServer(s *server, macService *macaroons.Service,
	subServerCgs *subRPCServerConfigs, serverOpts []grpc.ServerOption,
	restServerOpts []grpc.DialOption, atpl *autopilot.Manager,
	invoiceRegistry *invoices.InvoiceRegistry,
	tlsCfg *tls.Config) (*rpcServer, error) {

	var (
		subServers     []lnrpc.SubServer
		subServerPerms []lnrpc.MacaroonPerms
	)

	// Before we create any of the sub-servers, we need to ensure that all
	// the dependencies they need are properly populated within each sub
	// server configuration struct.
	err := subServerCgs.PopulateDependencies(
		s.cc, networkDir, macService, atpl, invoiceRegistry,
		activeNetParams.Params,
	)
	if err != nil {
		return nil, err
	}

	// Now that the sub-servers have all their dependencies in place, we
	// can create each sub-server!
	registeredSubServers := lnrpc.RegisteredSubServers()
	for _, subServer := range registeredSubServers {
		subServerInstance, macPerms, err := subServer.New(subServerCgs)
		if err != nil {
			return nil, err
		}

		// We'll collect the sub-server, and also the set of
		// permissions it needs for macaroons so we can apply the
		// interceptors below.
		subServers = append(subServers, subServerInstance)
		subServerPerms = append(subServerPerms, macPerms)
	}

	// Next, we need to merge the set of sub server macaroon permissions
	// with the main RPC server permissions so we can unite them under a
	// single set of interceptors.
	for _, subServerPerm := range subServerPerms {
		for method, ops := range subServerPerm {
			// For each new method:ops combo, we also ensure that
			// non of the sub-servers try to override each other.
			if _, ok := permissions[method]; ok {
				return nil, fmt.Errorf("detected duplicate "+
					"macaroon constraints for path: %v",
					method)
			}

			permissions[method] = ops
		}
	}

	// If macaroons aren't disabled (a non-nil service), then we'll set up
	// our set of interceptors which will allow us handle the macaroon
	// authentication in a single location .
	if macService != nil {
		unaryInterceptor := grpc.UnaryInterceptor(
			macService.UnaryServerInterceptor(permissions),
		)
		streamInterceptor := grpc.StreamInterceptor(
			macService.StreamServerInterceptor(permissions),
		)

		serverOpts = append(serverOpts,
			unaryInterceptor, streamInterceptor,
		)
	}

	// Finally, with all the pre-set up complete,  we can create the main
	// gRPC server, and register the main lnrpc server along side.
	grpcServer := grpc.NewServer(serverOpts...)
	rootRPCServer := &rpcServer{
		restServerOpts: restServerOpts,
		subServers:     subServers,
		tlsCfg:         tlsCfg,
		grpcServer:     grpcServer,
		server:         s,
		quit:           make(chan struct{}, 1),
	}
	lnrpc.RegisterLightningServer(grpcServer, rootRPCServer)

	// Now the main RPC server has been registered, we'll iterate through
	// all the sub-RPC servers and register them to ensure that requests
	// are properly routed towards them.
	for _, subServer := range subServers {
		err := subServer.RegisterWithRootServer(grpcServer)
		if err != nil {
			return nil, fmt.Errorf("unable to register "+
				"sub-server %v with root: %v",
				subServer.Name(), err)
		}
	}

	return rootRPCServer, nil
}

// Start launches any helper goroutines required for the rpcServer to function.
func (r *rpcServer) Start() error {
	if atomic.AddInt32(&r.started, 1) != 1 {
		return nil
	}

	// First, we'll start all the sub-servers to ensure that they're ready
	// to take new requests in.
	//
	// TODO(roasbeef): some may require that the entire daemon be started
	// at that point
	for _, subServer := range r.subServers {
		rpcsLog.Debugf("Starting sub RPC server: %v", subServer.Name())

		if err := subServer.Start(); err != nil {
			return err
		}
	}

	// With all the sub-servers started, we'll spin up the listeners for
	// the main RPC server itself.
	for _, listener := range cfg.RPCListeners {
		lis, err := lncfg.ListenOnAddress(listener)
		if err != nil {
			ltndLog.Errorf(
				"RPC server unable to listen on %s", listener,
			)
			return err
		}

		r.listenerCleanUp = append(r.listenerCleanUp, func() {
			lis.Close()
		})

		go func() {
			rpcsLog.Infof("RPC server listening on %s", lis.Addr())
			r.grpcServer.Serve(lis)
		}()
	}

	// Finally, start the REST proxy for our gRPC server above. We'll ensure
	// we direct LND to connect to its loopback address rather than a
	// wildcard to prevent certificate issues when accessing the proxy
	// externally.
	//
	// TODO(roasbeef): eventually also allow the sub-servers to themselves
	// have a REST proxy.
	mux := proxy.NewServeMux()
	grpcEndpoint := cfg.RPCListeners[0].String()
	switch {
	case strings.Contains(grpcEndpoint, "0.0.0.0"):
		grpcEndpoint = strings.Replace(
			grpcEndpoint, "0.0.0.0", "127.0.0.1", 1,
		)
	case strings.Contains(grpcEndpoint, "[::]"):
		grpcEndpoint = strings.Replace(grpcEndpoint, "[::]", "[::1]", 1)
	}
	err := lnrpc.RegisterLightningHandlerFromEndpoint(
		context.Background(), mux, grpcEndpoint, r.restServerOpts,
	)
	if err != nil {
		return err
	}
	for _, restEndpoint := range cfg.RESTListeners {
		lis, err := lncfg.TLSListenOnAddress(restEndpoint, r.tlsCfg)
		if err != nil {
			ltndLog.Errorf(
				"gRPC proxy unable to listen on %s",
				restEndpoint,
			)
			return err
		}

		r.listenerCleanUp = append(r.listenerCleanUp, func() {
			lis.Close()
		})

		go func() {
			rpcsLog.Infof("gRPC proxy started at %s", lis.Addr())
			http.Serve(lis, mux)
		}()
	}

	return nil
}

// Stop signals any active goroutines for a graceful closure.
func (r *rpcServer) Stop() error {
	if atomic.AddInt32(&r.shutdown, 1) != 1 {
		return nil
	}

	rpcsLog.Infof("Stopping RPC Server")

	close(r.quit)

	// After we've signalled all of our active goroutines to exit, we'll
	// then do the same to signal a graceful shutdown of all the sub
	// servers.
	for _, subServer := range r.subServers {
		rpcsLog.Infof("Stopping %v Sub-RPC Server",
			subServer.Name())

		if err := subServer.Stop(); err != nil {
			rpcsLog.Errorf("unable to stop sub-server %v: %v",
				subServer.Name(), err)
			continue
		}
	}

	// Finally, we can clean up all the listening sockets to ensure that we
	// give the file descriptors back to the OS.
	for _, cleanUp := range r.listenerCleanUp {
		cleanUp()
	}

	return nil
}

// addrPairsToOutputs converts a map describing a set of outputs to be created,
// the outputs themselves. The passed map pairs up an address, to a desired
// output value amount. Each address is converted to its corresponding pkScript
// to be used within the constructed output(s).
func addrPairsToOutputs(addrPairs map[string]int64) ([]*wire.TxOut, error) {
	outputs := make([]*wire.TxOut, 0, len(addrPairs))
	for addr, amt := range addrPairs {
		addr, err := btcutil.DecodeAddress(addr, activeNetParams.Params)
		if err != nil {
			return nil, err
		}

		pkscript, err := txscript.PayToAddrScript(addr)
		if err != nil {
			return nil, err
		}

		outputs = append(outputs, wire.NewTxOut(amt, pkscript))
	}

	return outputs, nil
}

// sendCoinsOnChain makes an on-chain transaction in or to send coins to one or
// more addresses specified in the passed payment map. The payment map maps an
// address to a specified output value to be sent to that address.
func (r *rpcServer) sendCoinsOnChain(paymentMap map[string]int64,
	feeRate lnwallet.SatPerKWeight) (*chainhash.Hash, error) {

	outputs, err := addrPairsToOutputs(paymentMap)
	if err != nil {
		return nil, err
	}

	tx, err := r.server.cc.wallet.SendOutputs(outputs, feeRate)
	if err != nil {
		return nil, err
	}

	txHash := tx.TxHash()
	return &txHash, nil
}

// ListUnspent returns useful information about each unspent output owned by
// the wallet, as reported by the underlying `ListUnspentWitness`; the
// information returned is: outpoint, amount in satoshis, address, address
// type, scriptPubKey in hex and number of confirmations.  The result is
// filtered to contain outputs whose number of confirmations is between a
// minimum and maximum number of confirmations specified by the user, with 0
// meaning unconfirmed.
func (r *rpcServer) ListUnspent(ctx context.Context,
	in *lnrpc.ListUnspentRequest) (*lnrpc.ListUnspentResponse, error) {

	minConfs := in.MinConfs
	maxConfs := in.MaxConfs

	switch {
	// Ensure that the user didn't attempt to specify a negative number of
	// confirmations, as that isn't possible.
	case minConfs < 0:
		return nil, fmt.Errorf("min confirmations must be >= 0")

	// We'll also ensure that the min number of confs is strictly less than
	// or equal to the max number of confs for sanity.
	case minConfs > maxConfs:
		return nil, fmt.Errorf("max confirmations must be >= min " +
			"confirmations")
	}

	// With our arguments validated, we'll query the internal wallet for
	// the set of UTXOs that match our query.
	utxos, err := r.server.cc.wallet.ListUnspentWitness(minConfs, maxConfs)
	if err != nil {
		return nil, err
	}

	resp := &lnrpc.ListUnspentResponse{
		Utxos: make([]*lnrpc.Utxo, 0, len(utxos)),
	}

	for _, utxo := range utxos {
		// Translate lnwallet address type to the proper gRPC proto
		// address type.
		var addrType lnrpc.AddressType
		switch utxo.AddressType {

		case lnwallet.WitnessPubKey:
			addrType = lnrpc.AddressType_WITNESS_PUBKEY_HASH

		case lnwallet.NestedWitnessPubKey:
			addrType = lnrpc.AddressType_NESTED_PUBKEY_HASH

		case lnwallet.UnknownAddressType:
			rpcsLog.Warnf("[listunspent] utxo with address of "+
				"unknown type ignored: %v",
				utxo.OutPoint.String())
			continue

		default:
			return nil, fmt.Errorf("invalid utxo address type")
		}

		// Now that we know we have a proper mapping to an address,
		// we'll convert the regular outpoint to an lnrpc variant.
		outpoint := &lnrpc.OutPoint{
			TxidBytes:   utxo.OutPoint.Hash[:],
			TxidStr:     utxo.OutPoint.Hash.String(),
			OutputIndex: utxo.OutPoint.Index,
		}

		utxoResp := lnrpc.Utxo{
			Type:          addrType,
			AmountSat:     int64(utxo.Value),
			ScriptPubkey:  hex.EncodeToString(utxo.PkScript),
			Outpoint:      outpoint,
			Confirmations: utxo.Confirmations,
		}

		// Finally, we'll attempt to extract the raw address from the
		// script so we can display a human friendly address to the end
		// user.
		_, outAddresses, _, err := txscript.ExtractPkScriptAddrs(
			utxo.PkScript, activeNetParams.Params,
		)
		if err != nil {
			return nil, err
		}

		// If we can't properly locate a single address, then this was
		// an error in our mapping, and we'll return an error back to
		// the user.
		if len(outAddresses) != 1 {
			return nil, fmt.Errorf("an output was unexpectedly " +
				"multisig")
		}

		utxoResp.Address = outAddresses[0].String()

		resp.Utxos = append(resp.Utxos, &utxoResp)
	}

	maxStr := ""
	if maxConfs != math.MaxInt32 {
		maxStr = " max=" + fmt.Sprintf("%d", maxConfs)
	}

	rpcsLog.Debugf("[listunspent] min=%v%v, generated utxos: %v", minConfs,
		maxStr, utxos)

	return resp, nil
}

// SendCoins executes a request to send coins to a particular address. Unlike
// SendMany, this RPC call only allows creating a single output at a time.
func (r *rpcServer) SendCoins(ctx context.Context,
	in *lnrpc.SendCoinsRequest) (*lnrpc.SendCoinsResponse, error) {

	// Based on the passed fee related parameters, we'll determine an
	// appropriate fee rate for this transaction.
	satPerKw := lnwallet.SatPerKVByte(in.SatPerByte * 1000).FeePerKWeight()
	feePerKw, err := sweep.DetermineFeePerKw(
		r.server.cc.feeEstimator, sweep.FeePreference{
			ConfTarget: uint32(in.TargetConf),
			FeeRate:    satPerKw,
		},
	)
	if err != nil {
		return nil, err
	}

	rpcsLog.Infof("[sendcoins] addr=%v, amt=%v, sat/kw=%v, sweep_all=%v",
		in.Addr, btcutil.Amount(in.Amount), int64(feePerKw),
		in.SendAll)

	var txid *chainhash.Hash

	wallet := r.server.cc.wallet

	// If the send all flag is active, then we'll attempt to sweep all the
	// coins in the wallet in a single transaction (if possible),
	// otherwise, we'll respect the amount, and attempt a regular 2-output
	// send.
	if in.SendAll {
		// At this point, the amount shouldn't be set since we've been
		// instructed to sweep all the coins from the wallet.
		if in.Amount != 0 {
			return nil, fmt.Errorf("amount set while SendAll is " +
				"active")
		}

		// Additionally, we'll need to convert the sweep address passed
		// into a useable struct, and also query for the latest block
		// height so we can properly construct the transaction.
		sweepAddr, err := btcutil.DecodeAddress(
			in.Addr, activeNetParams.Params,
		)
		if err != nil {
			return nil, err
		}
		_, bestHeight, err := r.server.cc.chainIO.GetBestBlock()
		if err != nil {
			return nil, err
		}

		// With the sweeper instance created, we can now generate a
		// transaction that will sweep ALL outputs from the wallet in a
		// single transaction. This will be generated in a concurrent
		// safe manner, so no need to worry about locking.
		sweepTxPkg, err := sweep.CraftSweepAllTx(
			feePerKw, uint32(bestHeight), sweepAddr, wallet,
			wallet.WalletController, wallet.WalletController,
			r.server.cc.feeEstimator, r.server.cc.signer,
		)
		if err != nil {
			return nil, err
		}

		rpcsLog.Debugf("Sweeping all coins from wallet to addr=%v, "+
			"with tx=%v", in.Addr, spew.Sdump(sweepTxPkg.SweepTx))

		// As our sweep transaction was created, successfully, we'll
		// now attempt to publish it, cancelling the sweep pkg to
		// return all outputs if it fails.
		err = wallet.PublishTransaction(sweepTxPkg.SweepTx)
		if err != nil {
			sweepTxPkg.CancelSweepAttempt()

			return nil, fmt.Errorf("unable to broadcast sweep "+
				"transaction: %v", err)
		}

		sweepTXID := sweepTxPkg.SweepTx.TxHash()
		txid = &sweepTXID
	} else {

		// We'll now construct out payment map, and use the wallet's
		// coin selection synchronization method to ensure that no coin
		// selection (funding, sweep alls, other sends) can proceed
		// while we instruct the wallet to send this transaction.
		paymentMap := map[string]int64{in.Addr: in.Amount}
		err := wallet.WithCoinSelectLock(func() error {
			newTXID, err := r.sendCoinsOnChain(paymentMap, feePerKw)
			if err != nil {
				return err
			}

			txid = newTXID

			return nil
		})
		if err != nil {
			return nil, err
		}
	}

	rpcsLog.Infof("[sendcoins] spend generated txid: %v", txid.String())

	return &lnrpc.SendCoinsResponse{Txid: txid.String()}, nil
}

// SendMany handles a request for a transaction create multiple specified
// outputs in parallel.
func (r *rpcServer) SendMany(ctx context.Context,
	in *lnrpc.SendManyRequest) (*lnrpc.SendManyResponse, error) {

	// Based on the passed fee related parameters, we'll determine an
	// appropriate fee rate for this transaction.
	satPerKw := lnwallet.SatPerKVByte(in.SatPerByte * 1000).FeePerKWeight()
	feePerKw, err := sweep.DetermineFeePerKw(
		r.server.cc.feeEstimator, sweep.FeePreference{
			ConfTarget: uint32(in.TargetConf),
			FeeRate:    satPerKw,
		},
	)
	if err != nil {
		return nil, err
	}

	rpcsLog.Infof("[sendmany] outputs=%v, sat/kw=%v",
		spew.Sdump(in.AddrToAmount), int64(feePerKw))

	var txid *chainhash.Hash

	// We'll attempt to send to the target set of outputs, ensuring that we
	// synchronize with any other ongoing coin selection attempts which
	// happen to also be concurrently executing.
	wallet := r.server.cc.wallet
	err = wallet.WithCoinSelectLock(func() error {
		sendManyTXID, err := r.sendCoinsOnChain(
			in.AddrToAmount, feePerKw,
		)
		if err != nil {
			return err
		}

		txid = sendManyTXID

		return nil
	})
	if err != nil {
		return nil, err
	}

	rpcsLog.Infof("[sendmany] spend generated txid: %v", txid.String())

	return &lnrpc.SendManyResponse{Txid: txid.String()}, nil
}

// NewAddress creates a new address under control of the local wallet.
func (r *rpcServer) NewAddress(ctx context.Context,
	in *lnrpc.NewAddressRequest) (*lnrpc.NewAddressResponse, error) {

	// Translate the gRPC proto address type to the wallet controller's
	// available address types.
	var addrType lnwallet.AddressType
	switch in.Type {
	case lnrpc.AddressType_WITNESS_PUBKEY_HASH:
		addrType = lnwallet.WitnessPubKey
	case lnrpc.AddressType_NESTED_PUBKEY_HASH:
		addrType = lnwallet.NestedWitnessPubKey
	}

	addr, err := r.server.cc.wallet.NewAddress(addrType, false)
	if err != nil {
		return nil, err
	}

	rpcsLog.Infof("[newaddress] addr=%v", addr.String())
	return &lnrpc.NewAddressResponse{Address: addr.String()}, nil
}

var (
	// signedMsgPrefix is a special prefix that we'll prepend to any
	// messages we sign/verify. We do this to ensure that we don't
	// accidentally sign a sighash, or other sensitive material. By
	// prepending this fragment, we mind message signing to our particular
	// context.
	signedMsgPrefix = []byte("Lightning Signed Message:")
)

// SignMessage signs a message with the resident node's private key. The
// returned signature string is zbase32 encoded and pubkey recoverable, meaning
// that only the message digest and signature are needed for verification.
func (r *rpcServer) SignMessage(ctx context.Context,
	in *lnrpc.SignMessageRequest) (*lnrpc.SignMessageResponse, error) {

	if in.Msg == nil {
		return nil, fmt.Errorf("need a message to sign")
	}

	in.Msg = append(signedMsgPrefix, in.Msg...)
	sigBytes, err := r.server.nodeSigner.SignCompact(in.Msg)
	if err != nil {
		return nil, err
	}

	sig := zbase32.EncodeToString(sigBytes)
	return &lnrpc.SignMessageResponse{Signature: sig}, nil
}

// VerifyMessage verifies a signature over a msg. The signature must be zbase32
// encoded and signed by an active node in the resident node's channel
// database. In addition to returning the validity of the signature,
// VerifyMessage also returns the recovered pubkey from the signature.
func (r *rpcServer) VerifyMessage(ctx context.Context,
	in *lnrpc.VerifyMessageRequest) (*lnrpc.VerifyMessageResponse, error) {

	if in.Msg == nil {
		return nil, fmt.Errorf("need a message to verify")
	}

	// The signature should be zbase32 encoded
	sig, err := zbase32.DecodeString(in.Signature)
	if err != nil {
		return nil, fmt.Errorf("failed to decode signature: %v", err)
	}

	// The signature is over the double-sha256 hash of the message.
	in.Msg = append(signedMsgPrefix, in.Msg...)
	digest := chainhash.DoubleHashB(in.Msg)

	// RecoverCompact both recovers the pubkey and validates the signature.
	pubKey, _, err := btcec.RecoverCompact(btcec.S256(), sig, digest)
	if err != nil {
		return &lnrpc.VerifyMessageResponse{Valid: false}, nil
	}
	pubKeyHex := hex.EncodeToString(pubKey.SerializeCompressed())

	var pub [33]byte
	copy(pub[:], pubKey.SerializeCompressed())

	// Query the channel graph to ensure a node in the network with active
	// channels signed the message.
	//
	// TODO(phlip9): Require valid nodes to have capital in active channels.
	graph := r.server.chanDB.ChannelGraph()
	_, active, err := graph.HasLightningNode(pub)
	if err != nil {
		return nil, fmt.Errorf("failed to query graph: %v", err)
	}

	return &lnrpc.VerifyMessageResponse{
		Valid:  active,
		Pubkey: pubKeyHex,
	}, nil
}

// ConnectPeer attempts to establish a connection to a remote peer.
func (r *rpcServer) ConnectPeer(ctx context.Context,
	in *lnrpc.ConnectPeerRequest) (*lnrpc.ConnectPeerResponse, error) {

	// The server hasn't yet started, so it won't be able to service any of
	// our requests, so we'll bail early here.
	if !r.server.Started() {
		return nil, fmt.Errorf("chain backend is still syncing, server " +
			"not active yet")
	}

	if in.Addr == nil {
		return nil, fmt.Errorf("need: lnc pubkeyhash@hostname")
	}

	pubkeyHex, err := hex.DecodeString(in.Addr.Pubkey)
	if err != nil {
		return nil, err
	}
	pubKey, err := btcec.ParsePubKey(pubkeyHex, btcec.S256())
	if err != nil {
		return nil, err
	}

	// Connections to ourselves are disallowed for obvious reasons.
	if pubKey.IsEqual(r.server.identityPriv.PubKey()) {
		return nil, fmt.Errorf("cannot make connection to self")
	}

	addr, err := parseAddr(in.Addr.Host)
	if err != nil {
		return nil, err
	}

	peerAddr := &lnwire.NetAddress{
		IdentityKey: pubKey,
		Address:     addr,
		ChainNet:    activeNetParams.Net,
	}

	rpcsLog.Debugf("[connectpeer] requested connection to %x@%s",
		peerAddr.IdentityKey.SerializeCompressed(), peerAddr.Address)

	if err := r.server.ConnectToPeer(peerAddr, in.Perm); err != nil {
		rpcsLog.Errorf("[connectpeer]: error connecting to peer: %v", err)
		return nil, err
	}

	rpcsLog.Debugf("Connected to peer: %v", peerAddr.String())
	return &lnrpc.ConnectPeerResponse{}, nil
}

// DisconnectPeer attempts to disconnect one peer from another identified by a
// given pubKey. In the case that we currently have a pending or active channel
// with the target peer, this action will be disallowed.
func (r *rpcServer) DisconnectPeer(ctx context.Context,
	in *lnrpc.DisconnectPeerRequest) (*lnrpc.DisconnectPeerResponse, error) {

	rpcsLog.Debugf("[disconnectpeer] from peer(%s)", in.PubKey)

	if !r.server.Started() {
		return nil, fmt.Errorf("chain backend is still syncing, server " +
			"not active yet")
	}

	// First we'll validate the string passed in within the request to
	// ensure that it's a valid hex-string, and also a valid compressed
	// public key.
	pubKeyBytes, err := hex.DecodeString(in.PubKey)
	if err != nil {
		return nil, fmt.Errorf("unable to decode pubkey bytes: %v", err)
	}
	peerPubKey, err := btcec.ParsePubKey(pubKeyBytes, btcec.S256())
	if err != nil {
		return nil, fmt.Errorf("unable to parse pubkey: %v", err)
	}

	// Next, we'll fetch the pending/active channels we have with a
	// particular peer.
	nodeChannels, err := r.server.chanDB.FetchOpenChannels(peerPubKey)
	if err != nil {
		return nil, fmt.Errorf("unable to fetch channels for peer: %v", err)
	}

	// In order to avoid erroneously disconnecting from a peer that we have
	// an active channel with, if we have any channels active with this
	// peer, then we'll disallow disconnecting from them.
	if len(nodeChannels) > 0 && !cfg.UnsafeDisconnect {
		return nil, fmt.Errorf("cannot disconnect from peer(%x), "+
			"all active channels with the peer need to be closed "+
			"first", pubKeyBytes)
	}

	// With all initial validation complete, we'll now request that the
	// server disconnects from the peer.
	if err := r.server.DisconnectPeer(peerPubKey); err != nil {
		return nil, fmt.Errorf("unable to disconnect peer: %v", err)
	}

	return &lnrpc.DisconnectPeerResponse{}, nil
}

// extractOpenChannelMinConfs extracts the minimum number of confirmations from
// the OpenChannelRequest that each output used to fund the channel's funding
// transaction should satisfy.
func extractOpenChannelMinConfs(in *lnrpc.OpenChannelRequest) (int32, error) {
	switch {
	// Ensure that the MinConfs parameter is non-negative.
	case in.MinConfs < 0:
		return 0, errors.New("minimum number of confirmations must " +
			"be a non-negative number")

	// The funding transaction should not be funded with unconfirmed outputs
	// unless explicitly specified by SpendUnconfirmed. We do this to
	// provide sane defaults to the OpenChannel RPC, as otherwise, if the
	// MinConfs field isn't explicitly set by the caller, we'll use
	// unconfirmed outputs without the caller being aware.
	case in.MinConfs == 0 && !in.SpendUnconfirmed:
		return 1, nil

	// In the event that the caller set MinConfs > 0 and SpendUnconfirmed to
	// true, we'll return an error to indicate the conflict.
	case in.MinConfs > 0 && in.SpendUnconfirmed:
		return 0, errors.New("SpendUnconfirmed set to true with " +
			"MinConfs > 0")

	// The funding transaction of the new channel to be created can be
	// funded with unconfirmed outputs.
	case in.SpendUnconfirmed:
		return 0, nil

	// If none of the above cases matched, we'll return the value set
	// explicitly by the caller.
	default:
		return in.MinConfs, nil
	}
}

// OpenChannel attempts to open a singly funded channel specified in the
// request to a remote peer.
func (r *rpcServer) OpenChannel(in *lnrpc.OpenChannelRequest,
	updateStream lnrpc.Lightning_OpenChannelServer) error {

	rpcsLog.Tracef("[openchannel] request to NodeKey(%v) "+
		"allocation(us=%v, them=%v)", in.NodePubkeyString,
		in.LocalFundingAmount, in.PushSat)

	if !r.server.Started() {
		return fmt.Errorf("chain backend is still syncing, server " +
			"not active yet")
	}

	localFundingAmt := btcutil.Amount(in.LocalFundingAmount)
	remoteInitialBalance := btcutil.Amount(in.PushSat)
	minHtlc := lnwire.MilliSatoshi(in.MinHtlcMsat)
	remoteCsvDelay := uint16(in.RemoteCsvDelay)

	// Ensure that the initial balance of the remote party (if pushing
	// satoshis) does not exceed the amount the local party has requested
	// for funding.
	//
	// TODO(roasbeef): incorporate base fee?
	if remoteInitialBalance >= localFundingAmt {
		return fmt.Errorf("amount pushed to remote peer for initial " +
			"state must be below the local funding amount")
	}

	// Ensure that the user doesn't exceed the current soft-limit for
	// channel size. If the funding amount is above the soft-limit, then
	// we'll reject the request.
	if localFundingAmt > maxFundingAmount {
		return fmt.Errorf("funding amount is too large, the max "+
			"channel size is: %v", maxFundingAmount)
	}

	// Restrict the size of the channel we'll actually open. At a later
	// level, we'll ensure that the output we create after accounting for
	// fees that a dust output isn't created.
	if localFundingAmt < minChanFundingSize {
		return fmt.Errorf("channel is too small, the minimum channel "+
			"size is: %v SAT", int64(minChanFundingSize))
	}

	// Then, we'll extract the minimum number of confirmations that each
	// output we use to fund the channel's funding transaction should
	// satisfy.
	minConfs, err := extractOpenChannelMinConfs(in)
	if err != nil {
		return err
	}

	var (
		nodePubKey      *btcec.PublicKey
		nodePubKeyBytes []byte
	)

	// TODO(roasbeef): also return channel ID?

	// Ensure that the NodePubKey is set before attempting to use it
	if len(in.NodePubkey) == 0 {
		return fmt.Errorf("NodePubKey is not set")
	}

	// Parse the raw bytes of the node key into a pubkey object so we
	// can easily manipulate it.
	nodePubKey, err = btcec.ParsePubKey(in.NodePubkey, btcec.S256())
	if err != nil {
		return err
	}

	// Making a channel to ourselves wouldn't be of any use, so we
	// explicitly disallow them.
	if nodePubKey.IsEqual(r.server.identityPriv.PubKey()) {
		return fmt.Errorf("cannot open channel to self")
	}

	nodePubKeyBytes = nodePubKey.SerializeCompressed()

	// Based on the passed fee related parameters, we'll determine an
	// appropriate fee rate for the funding transaction.
	satPerKw := lnwallet.SatPerKVByte(in.SatPerByte * 1000).FeePerKWeight()
	feeRate, err := sweep.DetermineFeePerKw(
		r.server.cc.feeEstimator, sweep.FeePreference{
			ConfTarget: uint32(in.TargetConf),
			FeeRate:    satPerKw,
		},
	)
	if err != nil {
		return err
	}

	rpcsLog.Debugf("[openchannel]: using fee of %v sat/kw for funding tx",
		int64(feeRate))

	// Instruct the server to trigger the necessary events to attempt to
	// open a new channel. A stream is returned in place, this stream will
	// be used to consume updates of the state of the pending channel.
	req := &openChanReq{
		targetPubkey:    nodePubKey,
		chainHash:       *activeNetParams.GenesisHash,
		localFundingAmt: localFundingAmt,
		pushAmt:         lnwire.NewMSatFromSatoshis(remoteInitialBalance),
		minHtlc:         minHtlc,
		fundingFeePerKw: feeRate,
		private:         in.Private,
		remoteCsvDelay:  remoteCsvDelay,
		minConfs:        minConfs,
	}

	updateChan, errChan := r.server.OpenChannel(req)

	var outpoint wire.OutPoint
out:
	for {
		select {
		case err := <-errChan:
			rpcsLog.Errorf("unable to open channel to NodeKey(%x): %v",
				nodePubKeyBytes, err)
			return err
		case fundingUpdate := <-updateChan:
			rpcsLog.Tracef("[openchannel] sending update: %v",
				fundingUpdate)
			if err := updateStream.Send(fundingUpdate); err != nil {
				return err
			}

			// If a final channel open update is being sent, then
			// we can break out of our recv loop as we no longer
			// need to process any further updates.
			switch update := fundingUpdate.Update.(type) {
			case *lnrpc.OpenStatusUpdate_ChanOpen:
				chanPoint := update.ChanOpen.ChannelPoint
				txidHash, err := getChanPointFundingTxid(chanPoint)
				if err != nil {
					return err
				}

				h, err := chainhash.NewHash(txidHash)
				if err != nil {
					return err
				}
				outpoint = wire.OutPoint{
					Hash:  *h,
					Index: chanPoint.OutputIndex,
				}

				break out
			}
		case <-r.quit:
			return nil
		}
	}

	rpcsLog.Tracef("[openchannel] success NodeKey(%x), ChannelPoint(%v)",
		nodePubKeyBytes, outpoint)
	return nil
}

// OpenChannelSync is a synchronous version of the OpenChannel RPC call. This
// call is meant to be consumed by clients to the REST proxy. As with all other
// sync calls, all byte slices are instead to be populated as hex encoded
// strings.
func (r *rpcServer) OpenChannelSync(ctx context.Context,
	in *lnrpc.OpenChannelRequest) (*lnrpc.ChannelPoint, error) {

	rpcsLog.Tracef("[openchannel] request to NodeKey(%v) "+
		"allocation(us=%v, them=%v)", in.NodePubkeyString,
		in.LocalFundingAmount, in.PushSat)

	// We don't allow new channels to be open while the server is still
	// syncing, as otherwise we may not be able to obtain the relevant
	// notifications.
	if !r.server.Started() {
		return nil, fmt.Errorf("chain backend is still syncing, server " +
			"not active yet")
	}

	// Creation of channels before the wallet syncs up is currently
	// disallowed.
	isSynced, _, err := r.server.cc.wallet.IsSynced()
	if err != nil {
		return nil, err
	}
	if !isSynced {
		return nil, errors.New("channels cannot be created before the " +
			"wallet is fully synced")
	}

	// Decode the provided target node's public key, parsing it into a pub
	// key object. For all sync call, byte slices are expected to be
	// encoded as hex strings.
	keyBytes, err := hex.DecodeString(in.NodePubkeyString)
	if err != nil {
		return nil, err
	}
	nodepubKey, err := btcec.ParsePubKey(keyBytes, btcec.S256())
	if err != nil {
		return nil, err
	}

	localFundingAmt := btcutil.Amount(in.LocalFundingAmount)
	remoteInitialBalance := btcutil.Amount(in.PushSat)
	minHtlc := lnwire.MilliSatoshi(in.MinHtlcMsat)
	remoteCsvDelay := uint16(in.RemoteCsvDelay)

	// Ensure that the initial balance of the remote party (if pushing
	// satoshis) does not exceed the amount the local party has requested
	// for funding.
	if remoteInitialBalance >= localFundingAmt {
		return nil, fmt.Errorf("amount pushed to remote peer for " +
			"initial state must be below the local funding amount")
	}

	// Restrict the size of the channel we'll actually open. At a later
	// level, we'll ensure that the output we create after accounting for
	// fees that a dust output isn't created.
	if localFundingAmt < minChanFundingSize {
		return nil, fmt.Errorf("channel is too small, the minimum channel "+
			"size is: %v SAT", int64(minChanFundingSize))
	}

	// Then, we'll extract the minimum number of confirmations that each
	// output we use to fund the channel's funding transaction should
	// satisfy.
	minConfs, err := extractOpenChannelMinConfs(in)
	if err != nil {
		return nil, err
	}

	// Based on the passed fee related parameters, we'll determine an
	// appropriate fee rate for the funding transaction.
	satPerKw := lnwallet.SatPerKVByte(in.SatPerByte * 1000).FeePerKWeight()
	feeRate, err := sweep.DetermineFeePerKw(
		r.server.cc.feeEstimator, sweep.FeePreference{
			ConfTarget: uint32(in.TargetConf),
			FeeRate:    satPerKw,
		},
	)
	if err != nil {
		return nil, err
	}

	rpcsLog.Tracef("[openchannel] target sat/kw for funding tx: %v",
		int64(feeRate))

	req := &openChanReq{
		targetPubkey:    nodepubKey,
		chainHash:       *activeNetParams.GenesisHash,
		localFundingAmt: localFundingAmt,
		pushAmt:         lnwire.NewMSatFromSatoshis(remoteInitialBalance),
		minHtlc:         minHtlc,
		fundingFeePerKw: feeRate,
		private:         in.Private,
		remoteCsvDelay:  remoteCsvDelay,
		minConfs:        minConfs,
	}

	updateChan, errChan := r.server.OpenChannel(req)
	select {
	// If an error occurs them immediately return the error to the client.
	case err := <-errChan:
		rpcsLog.Errorf("unable to open channel to NodeKey(%x): %v",
			nodepubKey, err)
		return nil, err

	// Otherwise, wait for the first channel update. The first update sent
	// is when the funding transaction is broadcast to the network.
	case fundingUpdate := <-updateChan:
		rpcsLog.Tracef("[openchannel] sending update: %v",
			fundingUpdate)

		// Parse out the txid of the pending funding transaction. The
		// sync client can use this to poll against the list of
		// PendingChannels.
		openUpdate := fundingUpdate.Update.(*lnrpc.OpenStatusUpdate_ChanPending)
		chanUpdate := openUpdate.ChanPending

		return &lnrpc.ChannelPoint{
			FundingTxid: &lnrpc.ChannelPoint_FundingTxidBytes{
				FundingTxidBytes: chanUpdate.Txid,
			},
			OutputIndex: chanUpdate.OutputIndex,
		}, nil
	case <-r.quit:
		return nil, nil
	}
}

// getChanPointFundingTxid returns the given channel point's funding txid in
// raw bytes.
func getChanPointFundingTxid(chanPoint *lnrpc.ChannelPoint) ([]byte, error) {
	var txid []byte

	// A channel point's funding txid can be get/set as a byte slice or a
	// string. In the case it is a string, decode it.
	switch chanPoint.GetFundingTxid().(type) {
	case *lnrpc.ChannelPoint_FundingTxidBytes:
		txid = chanPoint.GetFundingTxidBytes()
	case *lnrpc.ChannelPoint_FundingTxidStr:
		s := chanPoint.GetFundingTxidStr()
		h, err := chainhash.NewHashFromStr(s)
		if err != nil {
			return nil, err
		}

		txid = h[:]
	}

	return txid, nil
}

// CloseChannel attempts to close an active channel identified by its channel
// point. The actions of this method can additionally be augmented to attempt
// a force close after a timeout period in the case of an inactive peer.
func (r *rpcServer) CloseChannel(in *lnrpc.CloseChannelRequest,
	updateStream lnrpc.Lightning_CloseChannelServer) error {

	// If the user didn't specify a channel point, then we'll reject this
	// request all together.
	if in.GetChannelPoint() == nil {
		return fmt.Errorf("must specify channel point in close channel")
	}

	force := in.Force
	index := in.ChannelPoint.OutputIndex
	txidHash, err := getChanPointFundingTxid(in.GetChannelPoint())
	if err != nil {
		rpcsLog.Errorf("[closechannel] unable to get funding txid: %v", err)
		return err
	}
	txid, err := chainhash.NewHash(txidHash)
	if err != nil {
		rpcsLog.Errorf("[closechannel] invalid txid: %v", err)
		return err
	}
	chanPoint := wire.NewOutPoint(txid, index)

	rpcsLog.Tracef("[closechannel] request for ChannelPoint(%v), force=%v",
		chanPoint, force)

	var (
		updateChan chan interface{}
		errChan    chan error
	)

	// TODO(roasbeef): if force and peer online then don't force?

	// First, we'll fetch the channel as is, as we'll need to examine it
	// regardless of if this is a force close or not.
	channel, err := r.fetchActiveChannel(*chanPoint)
	if err != nil {
		return err
	}

	// If a force closure was requested, then we'll handle all the details
	// around the creation and broadcast of the unilateral closure
	// transaction here rather than going to the switch as we don't require
	// interaction from the peer.
	if force {
		_, bestHeight, err := r.server.cc.chainIO.GetBestBlock()
		if err != nil {
			return err
		}

		// As we're force closing this channel, as a precaution, we'll
		// ensure that the switch doesn't continue to see this channel
		// as eligible for forwarding HTLC's. If the peer is online,
		// then we'll also purge all of its indexes.
		remotePub := &channel.StateSnapshot().RemoteIdentity
		if peer, err := r.server.FindPeer(remotePub); err == nil {
			// TODO(roasbeef): actually get the active channel
			// instead too?
			//  * so only need to grab from database
			peer.WipeChannel(channel.ChannelPoint())
		} else {
			chanID := lnwire.NewChanIDFromOutPoint(channel.ChannelPoint())
			r.server.htlcSwitch.RemoveLink(chanID)
		}

		// With the necessary indexes cleaned up, we'll now force close
		// the channel.
		chainArbitrator := r.server.chainArb
		closingTx, err := chainArbitrator.ForceCloseContract(
			*chanPoint,
		)
		if err != nil {
			rpcsLog.Errorf("unable to force close transaction: %v", err)
			return err
		}

		closingTxid := closingTx.TxHash()

		// With the transaction broadcast, we send our first update to
		// the client.
		updateChan = make(chan interface{}, 2)
		updateChan <- &pendingUpdate{
			Txid: closingTxid[:],
		}

		errChan = make(chan error, 1)
		notifier := r.server.cc.chainNotifier
		go waitForChanToClose(uint32(bestHeight), notifier, errChan, chanPoint,
			&closingTxid, closingTx.TxOut[0].PkScript, func() {
				// Respond to the local subsystem which
				// requested the channel closure.
				updateChan <- &channelCloseUpdate{
					ClosingTxid: closingTxid[:],
					Success:     true,
				}
			})
	} else {
		// If the link is not known by the switch, we cannot gracefully close
		// the channel.
		channelID := lnwire.NewChanIDFromOutPoint(chanPoint)
		if _, err := r.server.htlcSwitch.GetLink(channelID); err != nil {
			rpcsLog.Debugf("Trying to non-force close offline channel with "+
				"chan_point=%v", chanPoint)
			return fmt.Errorf("unable to gracefully close channel while peer "+
				"is offline (try force closing it instead): %v", err)
		}

		// Based on the passed fee related parameters, we'll determine
		// an appropriate fee rate for the cooperative closure
		// transaction.
		satPerKw := lnwallet.SatPerKVByte(
			in.SatPerByte * 1000,
		).FeePerKWeight()
		feeRate, err := sweep.DetermineFeePerKw(
			r.server.cc.feeEstimator, sweep.FeePreference{
				ConfTarget: uint32(in.TargetConf),
				FeeRate:    satPerKw,
			},
		)
		if err != nil {
			return err
		}

		rpcsLog.Debugf("Target sat/kw for closing transaction: %v",
			int64(feeRate))

		// Before we attempt the cooperative channel closure, we'll
		// examine the channel to ensure that it doesn't have a
		// lingering HTLC.
		if len(channel.ActiveHtlcs()) != 0 {
			return fmt.Errorf("cannot co-op close channel " +
				"with active htlcs")
		}

		// Otherwise, the caller has requested a regular interactive
		// cooperative channel closure. So we'll forward the request to
		// the htlc switch which will handle the negotiation and
		// broadcast details.
		updateChan, errChan = r.server.htlcSwitch.CloseLink(
			chanPoint, htlcswitch.CloseRegular, feeRate,
		)
	}
out:
	for {
		select {
		case err := <-errChan:
			rpcsLog.Errorf("[closechannel] unable to close "+
				"ChannelPoint(%v): %v", chanPoint, err)
			return err
		case closingUpdate := <-updateChan:
			rpcClosingUpdate, err := createRPCCloseUpdate(
				closingUpdate,
			)
			if err != nil {
				return err
			}

			rpcsLog.Tracef("[closechannel] sending update: %v",
				rpcClosingUpdate)

			if err := updateStream.Send(rpcClosingUpdate); err != nil {
				return err
			}

			// If a final channel closing updates is being sent,
			// then we can break out of our dispatch loop as we no
			// longer need to process any further updates.
			switch closeUpdate := closingUpdate.(type) {
			case *channelCloseUpdate:
				h, _ := chainhash.NewHash(closeUpdate.ClosingTxid)
				rpcsLog.Infof("[closechannel] close completed: "+
					"txid(%v)", h)
				break out
			}
		case <-r.quit:
			return nil
		}
	}

	return nil
}

func createRPCCloseUpdate(update interface{}) (
	*lnrpc.CloseStatusUpdate, error) {

	switch u := update.(type) {
	case *channelCloseUpdate:
		return &lnrpc.CloseStatusUpdate{
			Update: &lnrpc.CloseStatusUpdate_ChanClose{
				ChanClose: &lnrpc.ChannelCloseUpdate{
					ClosingTxid: u.ClosingTxid,
				},
			},
		}, nil
	case *pendingUpdate:
		return &lnrpc.CloseStatusUpdate{
			Update: &lnrpc.CloseStatusUpdate_ClosePending{
				ClosePending: &lnrpc.PendingUpdate{
					Txid:        u.Txid,
					OutputIndex: u.OutputIndex,
				},
			},
		}, nil
	}

	return nil, errors.New("unknown close status update")
}

// AbandonChannel removes all channel state from the database except for a
// close summary. This method can be used to get rid of permanently unusable
// channels due to bugs fixed in newer versions of lnd.
func (r *rpcServer) AbandonChannel(ctx context.Context,
	in *lnrpc.AbandonChannelRequest) (*lnrpc.AbandonChannelResponse, error) {

	// If this isn't the dev build, then we won't allow the RPC to be
	// executed, as it's an advanced feature and won't be activated in
	// regular production/release builds.
	if !build.IsDevBuild() {
		return nil, fmt.Errorf("AbandonChannel RPC call only " +
			"available in dev builds")
	}

	// We'll parse out the arguments to we can obtain the chanPoint of the
	// target channel.
	txidHash, err := getChanPointFundingTxid(in.GetChannelPoint())
	if err != nil {
		return nil, err
	}
	txid, err := chainhash.NewHash(txidHash)
	if err != nil {
		return nil, err
	}
	index := in.ChannelPoint.OutputIndex
	chanPoint := wire.NewOutPoint(txid, index)

	// With the chanPoint constructed, we'll attempt to find the target
	// channel in the database. If we can't find the channel, then we'll
	// return the error back to the caller.
	dbChan, err := r.server.chanDB.FetchChannel(*chanPoint)
	if err != nil {
		return nil, err
	}

	// Now that we've found the channel, we'll populate a close summary for
	// the channel, so we can store as much information for this abounded
	// channel as possible. We also ensure that we set Pending to false, to
	// indicate that this channel has been "fully" closed.
	_, bestHeight, err := r.server.cc.chainIO.GetBestBlock()
	if err != nil {
		return nil, err
	}
	summary := &channeldb.ChannelCloseSummary{
		CloseType:               channeldb.Abandoned,
		ChanPoint:               *chanPoint,
		ChainHash:               dbChan.ChainHash,
		CloseHeight:             uint32(bestHeight),
		RemotePub:               dbChan.IdentityPub,
		Capacity:                dbChan.Capacity,
		SettledBalance:          dbChan.LocalCommitment.LocalBalance.ToSatoshis(),
		ShortChanID:             dbChan.ShortChanID(),
		RemoteCurrentRevocation: dbChan.RemoteCurrentRevocation,
		RemoteNextRevocation:    dbChan.RemoteNextRevocation,
		LocalChanConfig:         dbChan.LocalChanCfg,
	}

	// Finally, we'll close the channel in the DB, and return back to the
	// caller.
	err = dbChan.CloseChannel(summary)
	if err != nil {
		return nil, err
	}

	return &lnrpc.AbandonChannelResponse{}, nil
}

// fetchActiveChannel attempts to locate a channel identified by its channel
// point from the database's set of all currently opened channels and
// return it as a fully populated state machine
func (r *rpcServer) fetchActiveChannel(chanPoint wire.OutPoint) (
	*lnwallet.LightningChannel, error) {

	dbChan, err := r.server.chanDB.FetchChannel(chanPoint)
	if err != nil {
		return nil, err
	}

	// If the channel is successfully fetched from the database,
	// we create a fully populated channel state machine which
	// uses the db channel as backing storage.
	return lnwallet.NewLightningChannel(
		r.server.cc.wallet.Cfg.Signer, nil, dbChan, nil,
	)
}

// GetInfo returns general information concerning the lightning node including
// its identity pubkey, alias, the chains it is connected to, and information
// concerning the number of open+pending channels.
func (r *rpcServer) GetInfo(ctx context.Context,
	in *lnrpc.GetInfoRequest) (*lnrpc.GetInfoResponse, error) {

	var activeChannels uint32
	serverPeers := r.server.Peers()
	for _, serverPeer := range serverPeers {
		activeChannels += uint32(len(serverPeer.ChannelSnapshots()))
	}

	openChannels, err := r.server.chanDB.FetchAllOpenChannels()
	if err != nil {
		return nil, err
	}
	inactiveChannels := uint32(len(openChannels)) - activeChannels

	pendingChannels, err := r.server.chanDB.FetchPendingChannels()
	if err != nil {
		return nil, fmt.Errorf("unable to get retrieve pending "+
			"channels: %v", err)
	}
	nPendingChannels := uint32(len(pendingChannels))

	idPub := r.server.identityPriv.PubKey().SerializeCompressed()
	encodedIDPub := hex.EncodeToString(idPub)

	bestHash, bestHeight, err := r.server.cc.chainIO.GetBestBlock()
	if err != nil {
		return nil, fmt.Errorf("unable to get best block info: %v", err)
	}

	isSynced, bestHeaderTimestamp, err := r.server.cc.wallet.IsSynced()
	if err != nil {
		return nil, fmt.Errorf("unable to sync PoV of the wallet "+
			"with current best block in the main chain: %v", err)
	}

	network := normalizeNetwork(activeNetParams.Name)
	activeChains := make([]*lnrpc.Chain, registeredChains.NumActiveChains())
	for i, chain := range registeredChains.ActiveChains() {
		activeChains[i] = &lnrpc.Chain{
			Chain:   chain.String(),
			Network: network,
		}

	}

	// Check if external IP addresses were provided to lnd and use them
	// to set the URIs.
	nodeAnn, err := r.server.genNodeAnnouncement(false)
	if err != nil {
		return nil, fmt.Errorf("unable to retrieve current fully signed "+
			"node announcement: %v", err)
	}
	addrs := nodeAnn.Addresses
	uris := make([]string, len(addrs))
	for i, addr := range addrs {
		uris[i] = fmt.Sprintf("%s@%s", encodedIDPub, addr.String())
	}

	// TODO(roasbeef): add synced height n stuff
	return &lnrpc.GetInfoResponse{
		IdentityPubkey:      encodedIDPub,
		NumPendingChannels:  nPendingChannels,
		NumActiveChannels:   activeChannels,
		NumInactiveChannels: inactiveChannels,
		NumPeers:            uint32(len(serverPeers)),
		BlockHeight:         uint32(bestHeight),
		BlockHash:           bestHash.String(),
		SyncedToChain:       isSynced,
		Testnet:             isTestnet(&activeNetParams),
		Chains:              activeChains,
		Uris:                uris,
		Alias:               nodeAnn.Alias.String(),
		BestHeaderTimestamp: int64(bestHeaderTimestamp),
		Version:             build.Version(),
	}, nil
}

// ListPeers returns a verbose listing of all currently active peers.
func (r *rpcServer) ListPeers(ctx context.Context,
	in *lnrpc.ListPeersRequest) (*lnrpc.ListPeersResponse, error) {

	rpcsLog.Tracef("[listpeers] request")

	serverPeers := r.server.Peers()
	resp := &lnrpc.ListPeersResponse{
		Peers: make([]*lnrpc.Peer, 0, len(serverPeers)),
	}

	for _, serverPeer := range serverPeers {
		var (
			satSent int64
			satRecv int64
		)

		// In order to display the total number of satoshis of outbound
		// (sent) and inbound (recv'd) satoshis that have been
		// transported through this peer, we'll sum up the sent/recv'd
		// values for each of the active channels we have with the
		// peer.
		chans := serverPeer.ChannelSnapshots()
		for _, c := range chans {
			satSent += int64(c.TotalMSatSent.ToSatoshis())
			satRecv += int64(c.TotalMSatReceived.ToSatoshis())
		}

		nodePub := serverPeer.addr.IdentityKey.SerializeCompressed()
		peer := &lnrpc.Peer{
			PubKey:    hex.EncodeToString(nodePub),
			Address:   serverPeer.conn.RemoteAddr().String(),
			Inbound:   serverPeer.inbound,
			BytesRecv: atomic.LoadUint64(&serverPeer.bytesReceived),
			BytesSent: atomic.LoadUint64(&serverPeer.bytesSent),
			SatSent:   satSent,
			SatRecv:   satRecv,
			PingTime:  serverPeer.PingTime(),
		}

		resp.Peers = append(resp.Peers, peer)
	}

	rpcsLog.Debugf("[listpeers] yielded %v peers", serverPeers)

	return resp, nil
}

// WalletBalance returns total unspent outputs(confirmed and unconfirmed), all
// confirmed unspent outputs and all unconfirmed unspent outputs under control
// by the wallet. This method can be modified by having the request specify
// only witness outputs should be factored into the final output sum.
// TODO(roasbeef): add async hooks into wallet balance changes
func (r *rpcServer) WalletBalance(ctx context.Context,
	in *lnrpc.WalletBalanceRequest) (*lnrpc.WalletBalanceResponse, error) {

	// Get total balance, from txs that have >= 0 confirmations.
	totalBal, err := r.server.cc.wallet.ConfirmedBalance(0)
	if err != nil {
		return nil, err
	}

	// Get confirmed balance, from txs that have >= 1 confirmations.
	confirmedBal, err := r.server.cc.wallet.ConfirmedBalance(1)
	if err != nil {
		return nil, err
	}

	// Get unconfirmed balance, from txs with 0 confirmations.
	unconfirmedBal := totalBal - confirmedBal

	rpcsLog.Debugf("[walletbalance] Total balance=%v", totalBal)

	return &lnrpc.WalletBalanceResponse{
		TotalBalance:       int64(totalBal),
		ConfirmedBalance:   int64(confirmedBal),
		UnconfirmedBalance: int64(unconfirmedBal),
	}, nil
}

// ChannelBalance returns the total available channel flow across all open
// channels in satoshis.
func (r *rpcServer) ChannelBalance(ctx context.Context,
	in *lnrpc.ChannelBalanceRequest) (*lnrpc.ChannelBalanceResponse, error) {

	openChannels, err := r.server.chanDB.FetchAllOpenChannels()
	if err != nil {
		return nil, err
	}

	var balance btcutil.Amount
	for _, channel := range openChannels {
		balance += channel.LocalCommitment.LocalBalance.ToSatoshis()
	}

	pendingChannels, err := r.server.chanDB.FetchPendingChannels()
	if err != nil {
		return nil, err
	}

	var pendingOpenBalance btcutil.Amount
	for _, channel := range pendingChannels {
		pendingOpenBalance += channel.LocalCommitment.LocalBalance.ToSatoshis()
	}

	return &lnrpc.ChannelBalanceResponse{
		Balance:            int64(balance),
		PendingOpenBalance: int64(pendingOpenBalance),
	}, nil
}

// PendingChannels returns a list of all the channels that are currently
// considered "pending". A channel is pending if it has finished the funding
// workflow and is waiting for confirmations for the funding txn, or is in the
// process of closure, either initiated cooperatively or non-cooperatively.
func (r *rpcServer) PendingChannels(ctx context.Context,
	in *lnrpc.PendingChannelsRequest) (*lnrpc.PendingChannelsResponse, error) {

	rpcsLog.Debugf("[pendingchannels]")

	resp := &lnrpc.PendingChannelsResponse{}

	// First, we'll populate the response with all the channels that are
	// soon to be opened. We can easily fetch this data from the database
	// and map the db struct to the proto response.
	pendingOpenChannels, err := r.server.chanDB.FetchPendingChannels()
	if err != nil {
		rpcsLog.Errorf("unable to fetch pending channels: %v", err)
		return nil, err
	}
	resp.PendingOpenChannels = make([]*lnrpc.PendingChannelsResponse_PendingOpenChannel,
		len(pendingOpenChannels))
	for i, pendingChan := range pendingOpenChannels {
		pub := pendingChan.IdentityPub.SerializeCompressed()

		// As this is required for display purposes, we'll calculate
		// the weight of the commitment transaction. We also add on the
		// estimated weight of the witness to calculate the weight of
		// the transaction if it were to be immediately unilaterally
		// broadcast.
		// TODO(roasbeef): query for funding tx from wallet, display
		// that also?
		localCommitment := pendingChan.LocalCommitment
		utx := btcutil.NewTx(localCommitment.CommitTx)
		commitBaseWeight := blockchain.GetTransactionWeight(utx)
		commitWeight := commitBaseWeight + input.WitnessCommitmentTxWeight

		resp.PendingOpenChannels[i] = &lnrpc.PendingChannelsResponse_PendingOpenChannel{
			Channel: &lnrpc.PendingChannelsResponse_PendingChannel{
				RemoteNodePub: hex.EncodeToString(pub),
				ChannelPoint:  pendingChan.FundingOutpoint.String(),
				Capacity:      int64(pendingChan.Capacity),
				LocalBalance:  int64(localCommitment.LocalBalance.ToSatoshis()),
				RemoteBalance: int64(localCommitment.RemoteBalance.ToSatoshis()),
			},
			CommitWeight: commitWeight,
			CommitFee:    int64(localCommitment.CommitFee),
			FeePerKw:     int64(localCommitment.FeePerKw),
			// TODO(roasbeef): need to track confirmation height
		}
	}

	_, currentHeight, err := r.server.cc.chainIO.GetBestBlock()
	if err != nil {
		return nil, err
	}

	// Next, we'll examine the channels that are soon to be closed so we
	// can populate these fields within the response.
	pendingCloseChannels, err := r.server.chanDB.FetchClosedChannels(true)
	if err != nil {
		rpcsLog.Errorf("unable to fetch closed channels: %v", err)
		return nil, err
	}
	for _, pendingClose := range pendingCloseChannels {
		// First construct the channel struct itself, this will be
		// needed regardless of how this channel was closed.
		pub := pendingClose.RemotePub.SerializeCompressed()
		chanPoint := pendingClose.ChanPoint
		channel := &lnrpc.PendingChannelsResponse_PendingChannel{
			RemoteNodePub: hex.EncodeToString(pub),
			ChannelPoint:  chanPoint.String(),
			Capacity:      int64(pendingClose.Capacity),
			LocalBalance:  int64(pendingClose.SettledBalance),
		}

		closeTXID := pendingClose.ClosingTXID.String()

		switch pendingClose.CloseType {

		// If the channel was closed cooperatively, then we'll only
		// need to tack on the closing txid.
		// TODO(halseth): remove. After recent changes, a coop closed
		// channel should never be in the "pending close" state.
		// Keeping for now to let someone that upgraded in the middle
		// of a close let their closing tx confirm.
		case channeldb.CooperativeClose:
			resp.PendingClosingChannels = append(
				resp.PendingClosingChannels,
				&lnrpc.PendingChannelsResponse_ClosedChannel{
					Channel:     channel,
					ClosingTxid: closeTXID,
				},
			)

			resp.TotalLimboBalance += channel.LocalBalance

		// If the channel was force closed, then we'll need to query
		// the utxoNursery for additional information.
		// TODO(halseth): distinguish remote and local case?
		case channeldb.LocalForceClose, channeldb.RemoteForceClose:
			forceClose := &lnrpc.PendingChannelsResponse_ForceClosedChannel{
				Channel:     channel,
				ClosingTxid: closeTXID,
			}

			// Fetch reports from both nursery and resolvers. At the
			// moment this is not an atomic snapshot. This is
			// planned to be resolved when the nursery is removed
			// and channel arbitrator will be the single source for
			// these kind of reports.
			err := r.nurseryPopulateForceCloseResp(
				&chanPoint, currentHeight, forceClose,
			)
			if err != nil {
				return nil, err
			}

			err = r.arbitratorPopulateForceCloseResp(
				&chanPoint, currentHeight, forceClose,
			)
			if err != nil {
				return nil, err
			}

			resp.TotalLimboBalance += int64(forceClose.LimboBalance)

			resp.PendingForceClosingChannels = append(
				resp.PendingForceClosingChannels,
				forceClose,
			)
		}
	}

	// We'll also fetch all channels that are open, but have had their
	// commitment broadcasted, meaning they are waiting for the closing
	// transaction to confirm.
	waitingCloseChans, err := r.server.chanDB.FetchWaitingCloseChannels()
	if err != nil {
		rpcsLog.Errorf("unable to fetch channels waiting close: %v",
			err)
		return nil, err
	}

	for _, waitingClose := range waitingCloseChans {
		pub := waitingClose.IdentityPub.SerializeCompressed()
		chanPoint := waitingClose.FundingOutpoint
		channel := &lnrpc.PendingChannelsResponse_PendingChannel{
			RemoteNodePub: hex.EncodeToString(pub),
			ChannelPoint:  chanPoint.String(),
			Capacity:      int64(waitingClose.Capacity),
			LocalBalance:  int64(waitingClose.LocalCommitment.LocalBalance.ToSatoshis()),
		}

		// A close tx has been broadcasted, all our balance will be in
		// limbo until it confirms.
		resp.WaitingCloseChannels = append(
			resp.WaitingCloseChannels,
			&lnrpc.PendingChannelsResponse_WaitingCloseChannel{
				Channel:      channel,
				LimboBalance: channel.LocalBalance,
			},
		)

		resp.TotalLimboBalance += channel.LocalBalance
	}

	return resp, nil
}

// arbitratorPopulateForceCloseResp populates the pending channels response
// message with channel resolution information from the contract resolvers.
func (r *rpcServer) arbitratorPopulateForceCloseResp(chanPoint *wire.OutPoint,
	currentHeight int32,
	forceClose *lnrpc.PendingChannelsResponse_ForceClosedChannel) error {

	// Query for contract resolvers state.
	arbitrator, err := r.server.chainArb.GetChannelArbitrator(*chanPoint)
	if err != nil {
		return err
	}
	reports := arbitrator.Report()

	for _, report := range reports {
		htlc := &lnrpc.PendingHTLC{
			Incoming:       report.Incoming,
			Amount:         int64(report.Amount),
			Outpoint:       report.Outpoint.String(),
			MaturityHeight: report.MaturityHeight,
			Stage:          report.Stage,
		}

		if htlc.MaturityHeight != 0 {
			htlc.BlocksTilMaturity =
				int32(htlc.MaturityHeight) - currentHeight
		}

		forceClose.LimboBalance += int64(report.LimboBalance)
		forceClose.RecoveredBalance += int64(report.RecoveredBalance)

		forceClose.PendingHtlcs = append(forceClose.PendingHtlcs, htlc)
	}

	return nil
}

// nurseryPopulateForceCloseResp populates the pending channels response
// message with contract resolution information from utxonursery.
func (r *rpcServer) nurseryPopulateForceCloseResp(chanPoint *wire.OutPoint,
	currentHeight int32,
	forceClose *lnrpc.PendingChannelsResponse_ForceClosedChannel) error {

	// Query for the maturity state for this force closed channel. If we
	// didn't have any time-locked outputs, then the nursery may not know of
	// the contract.
	nurseryInfo, err := r.server.utxoNursery.NurseryReport(chanPoint)
	if err == ErrContractNotFound {
		return nil
	}
	if err != nil {
		return fmt.Errorf("unable to obtain "+
			"nursery report for ChannelPoint(%v): %v",
			chanPoint, err)
	}

	// If the nursery knows of this channel, then we can populate
	// information detailing exactly how much funds are time locked and also
	// the height in which we can ultimately sweep the funds into the
	// wallet.
	forceClose.LimboBalance = int64(nurseryInfo.limboBalance)
	forceClose.RecoveredBalance = int64(nurseryInfo.recoveredBalance)
	forceClose.MaturityHeight = nurseryInfo.maturityHeight

	// If the transaction has been confirmed, then we can compute how many
	// blocks it has left.
	if forceClose.MaturityHeight != 0 {
		forceClose.BlocksTilMaturity =
			int32(forceClose.MaturityHeight) -
				currentHeight
	}

	for _, htlcReport := range nurseryInfo.htlcs {
		// TODO(conner) set incoming flag appropriately after handling
		// incoming incubation
		htlc := &lnrpc.PendingHTLC{
			Incoming:       false,
			Amount:         int64(htlcReport.amount),
			Outpoint:       htlcReport.outpoint.String(),
			MaturityHeight: htlcReport.maturityHeight,
			Stage:          htlcReport.stage,
		}

		if htlc.MaturityHeight != 0 {
			htlc.BlocksTilMaturity =
				int32(htlc.MaturityHeight) -
					currentHeight
		}

		forceClose.PendingHtlcs = append(forceClose.PendingHtlcs,
			htlc)
	}

	return nil
}

// ClosedChannels returns a list of all the channels have been closed.
// This does not include channels that are still in the process of closing.
func (r *rpcServer) ClosedChannels(ctx context.Context,
	in *lnrpc.ClosedChannelsRequest) (*lnrpc.ClosedChannelsResponse,
	error) {

	// Show all channels when no filter flags are set.
	filterResults := in.Cooperative || in.LocalForce ||
		in.RemoteForce || in.Breach || in.FundingCanceled ||
		in.Abandoned

	resp := &lnrpc.ClosedChannelsResponse{}

	dbChannels, err := r.server.chanDB.FetchClosedChannels(false)
	if err != nil {
		return nil, err
	}

	// In order to make the response easier to parse for clients, we'll
	// sort the set of closed channels by their closing height before
	// serializing the proto response.
	sort.Slice(dbChannels, func(i, j int) bool {
		return dbChannels[i].CloseHeight < dbChannels[j].CloseHeight
	})

	for _, dbChannel := range dbChannels {
		if dbChannel.IsPending {
			continue
		}

		switch dbChannel.CloseType {
		case channeldb.CooperativeClose:
			if filterResults && !in.Cooperative {
				continue
			}
		case channeldb.LocalForceClose:
			if filterResults && !in.LocalForce {
				continue
			}
		case channeldb.RemoteForceClose:
			if filterResults && !in.RemoteForce {
				continue
			}
		case channeldb.BreachClose:
			if filterResults && !in.Breach {
				continue
			}
		case channeldb.FundingCanceled:
			if filterResults && !in.FundingCanceled {
				continue
			}
		case channeldb.Abandoned:
			if filterResults && !in.Abandoned {
				continue
			}
		}

		channel := createRPCClosedChannel(dbChannel)
		resp.Channels = append(resp.Channels, channel)
	}

	return resp, nil
}

// ListChannels returns a description of all the open channels that this node
// is a participant in.
func (r *rpcServer) ListChannels(ctx context.Context,
	in *lnrpc.ListChannelsRequest) (*lnrpc.ListChannelsResponse, error) {

	if in.ActiveOnly && in.InactiveOnly {
		return nil, fmt.Errorf("either `active_only` or " +
			"`inactive_only` can be set, but not both")
	}

	if in.PublicOnly && in.PrivateOnly {
		return nil, fmt.Errorf("either `public_only` or " +
			"`private_only` can be set, but not both")
	}

	resp := &lnrpc.ListChannelsResponse{}

	graph := r.server.chanDB.ChannelGraph()

	dbChannels, err := r.server.chanDB.FetchAllOpenChannels()
	if err != nil {
		return nil, err
	}

	rpcsLog.Infof("[listchannels] fetched %v channels from DB",
		len(dbChannels))

	for _, dbChannel := range dbChannels {
		nodePub := dbChannel.IdentityPub
		chanPoint := dbChannel.FundingOutpoint

		var peerOnline bool
		if _, err := r.server.FindPeer(nodePub); err == nil {
			peerOnline = true
		}

		channelID := lnwire.NewChanIDFromOutPoint(&chanPoint)
		var linkActive bool
		if link, err := r.server.htlcSwitch.GetLink(channelID); err == nil {
			// A channel is only considered active if it is known
			// by the switch *and* able to forward
			// incoming/outgoing payments.
			linkActive = link.EligibleToForward()
		}

		// Next, we'll determine whether we should add this channel to
		// our list depending on the type of channels requested to us.
		isActive := peerOnline && linkActive
		channel := createRPCOpenChannel(r, graph, dbChannel, isActive)

		// We'll only skip returning this channel if we were requested
		// for a specific kind and this channel doesn't satisfy it.
		switch {
		case in.ActiveOnly && !isActive:
			continue
		case in.InactiveOnly && isActive:
			continue
		case in.PublicOnly && channel.Private:
			continue
		case in.PrivateOnly && !channel.Private:
			continue
		}

		resp.Channels = append(resp.Channels, channel)
	}

	return resp, nil
}

// createRPCOpenChannel creates an *lnrpc.Channel from the *channeldb.Channel.
func createRPCOpenChannel(r *rpcServer, graph *channeldb.ChannelGraph,
	dbChannel *channeldb.OpenChannel, isActive bool) *lnrpc.Channel {

	nodePub := dbChannel.IdentityPub
	nodeID := hex.EncodeToString(nodePub.SerializeCompressed())
	chanPoint := dbChannel.FundingOutpoint

	// With the channel point known, retrieve the network channel
	// ID from the database.
	var chanID uint64
	chanID, _ = graph.ChannelID(&chanPoint)

	// Next, we'll determine whether the channel is public or not.
	isPublic := dbChannel.ChannelFlags&lnwire.FFAnnounceChannel != 0

	// As this is required for display purposes, we'll calculate
	// the weight of the commitment transaction. We also add on the
	// estimated weight of the witness to calculate the weight of
	// the transaction if it were to be immediately unilaterally
	// broadcast.
	localCommit := dbChannel.LocalCommitment
	utx := btcutil.NewTx(localCommit.CommitTx)
	commitBaseWeight := blockchain.GetTransactionWeight(utx)
	commitWeight := commitBaseWeight + input.WitnessCommitmentTxWeight

	localBalance := localCommit.LocalBalance
	remoteBalance := localCommit.RemoteBalance

	// As an artifact of our usage of mSAT internally, either party
	// may end up in a state where they're holding a fractional
	// amount of satoshis which can't be expressed within the
	// actual commitment output. Since we round down when going
	// from mSAT -> SAT, we may at any point be adding an
	// additional SAT to miners fees. As a result, we display a
	// commitment fee that accounts for this externally.
	var sumOutputs btcutil.Amount
	for _, txOut := range localCommit.CommitTx.TxOut {
		sumOutputs += btcutil.Amount(txOut.Value)
	}
	externalCommitFee := dbChannel.Capacity - sumOutputs

	channel := &lnrpc.Channel{
		Active:                isActive,
		Private:               !isPublic,
		RemotePubkey:          nodeID,
		ChannelPoint:          chanPoint.String(),
		ChanId:                chanID,
		Capacity:              int64(dbChannel.Capacity),
		LocalBalance:          int64(localBalance.ToSatoshis()),
		RemoteBalance:         int64(remoteBalance.ToSatoshis()),
		CommitFee:             int64(externalCommitFee),
		CommitWeight:          commitWeight,
		FeePerKw:              int64(localCommit.FeePerKw),
		TotalSatoshisSent:     int64(dbChannel.TotalMSatSent.ToSatoshis()),
		TotalSatoshisReceived: int64(dbChannel.TotalMSatReceived.ToSatoshis()),
		NumUpdates:            localCommit.CommitHeight,
		PendingHtlcs:          make([]*lnrpc.HTLC, len(localCommit.Htlcs)),
		CsvDelay:              uint32(dbChannel.LocalChanCfg.CsvDelay),
		Initiator:             dbChannel.IsInitiator,
	}

	for i, htlc := range localCommit.Htlcs {
		var rHash [32]byte
		copy(rHash[:], htlc.RHash[:])
		channel.PendingHtlcs[i] = &lnrpc.HTLC{
			Incoming:         htlc.Incoming,
			Amount:           int64(htlc.Amt.ToSatoshis()),
			HashLock:         rHash[:],
			ExpirationHeight: htlc.RefundTimeout,
		}

		// Add the Pending Htlc Amount to UnsettledBalance field.
		channel.UnsettledBalance += channel.PendingHtlcs[i].Amount
	}

	return channel
}

// createRPCClosedChannel creates an *lnrpc.ClosedChannelSummary from a
// *channeldb.ChannelCloseSummary.
func createRPCClosedChannel(
	dbChannel *channeldb.ChannelCloseSummary) *lnrpc.ChannelCloseSummary {

	nodePub := dbChannel.RemotePub
	nodeID := hex.EncodeToString(nodePub.SerializeCompressed())

	var closeType lnrpc.ChannelCloseSummary_ClosureType
	switch dbChannel.CloseType {
	case channeldb.CooperativeClose:
		closeType = lnrpc.ChannelCloseSummary_COOPERATIVE_CLOSE
	case channeldb.LocalForceClose:
		closeType = lnrpc.ChannelCloseSummary_LOCAL_FORCE_CLOSE
	case channeldb.RemoteForceClose:
		closeType = lnrpc.ChannelCloseSummary_REMOTE_FORCE_CLOSE
	case channeldb.BreachClose:
		closeType = lnrpc.ChannelCloseSummary_BREACH_CLOSE
	case channeldb.FundingCanceled:
		closeType = lnrpc.ChannelCloseSummary_FUNDING_CANCELED
	case channeldb.Abandoned:
		closeType = lnrpc.ChannelCloseSummary_ABANDONED
	}

	return &lnrpc.ChannelCloseSummary{
		Capacity:          int64(dbChannel.Capacity),
		RemotePubkey:      nodeID,
		CloseHeight:       dbChannel.CloseHeight,
		CloseType:         closeType,
		ChannelPoint:      dbChannel.ChanPoint.String(),
		ChanId:            dbChannel.ShortChanID.ToUint64(),
		SettledBalance:    int64(dbChannel.SettledBalance),
		TimeLockedBalance: int64(dbChannel.TimeLockedBalance),
		ChainHash:         dbChannel.ChainHash.String(),
		ClosingTxHash:     dbChannel.ClosingTXID.String(),
	}
}

// SubscribeChannelEvents returns a uni-directional stream (server -> client)
// for notifying the client of newly active, inactive or closed channels.
func (r *rpcServer) SubscribeChannelEvents(req *lnrpc.ChannelEventSubscription,
	updateStream lnrpc.Lightning_SubscribeChannelEventsServer) error {

	channelEventSub, err := r.server.channelNotifier.SubscribeChannelEvents()
	if err != nil {
		return err
	}

	// Ensure that the resources for the client is cleaned up once either
	// the server, or client exits.
	defer channelEventSub.Cancel()

	graph := r.server.chanDB.ChannelGraph()

	for {
		select {
		// A new update has been sent by the channel router, we'll
		// marshal it into the form expected by the gRPC client, then
		// send it off to the client(s).
		case e := <-channelEventSub.Updates():
			var update *lnrpc.ChannelEventUpdate
			switch event := e.(type) {
			case channelnotifier.OpenChannelEvent:
				channel := createRPCOpenChannel(r, graph,
					event.Channel, true)
				update = &lnrpc.ChannelEventUpdate{
					Type: lnrpc.ChannelEventUpdate_OPEN_CHANNEL,
					Channel: &lnrpc.ChannelEventUpdate_OpenChannel{
						OpenChannel: channel,
					},
				}
			case channelnotifier.ClosedChannelEvent:
				closedChannel := createRPCClosedChannel(event.CloseSummary)
				update = &lnrpc.ChannelEventUpdate{
					Type: lnrpc.ChannelEventUpdate_CLOSED_CHANNEL,
					Channel: &lnrpc.ChannelEventUpdate_ClosedChannel{
						ClosedChannel: closedChannel,
					},
				}
			case channelnotifier.ActiveChannelEvent:
				update = &lnrpc.ChannelEventUpdate{
					Type: lnrpc.ChannelEventUpdate_ACTIVE_CHANNEL,
					Channel: &lnrpc.ChannelEventUpdate_ActiveChannel{
						ActiveChannel: &lnrpc.ChannelPoint{
							FundingTxid: &lnrpc.ChannelPoint_FundingTxidBytes{
								FundingTxidBytes: event.ChannelPoint.Hash[:],
							},
							OutputIndex: event.ChannelPoint.Index,
						},
					},
				}
			case channelnotifier.InactiveChannelEvent:
				update = &lnrpc.ChannelEventUpdate{
					Type: lnrpc.ChannelEventUpdate_INACTIVE_CHANNEL,
					Channel: &lnrpc.ChannelEventUpdate_InactiveChannel{
						InactiveChannel: &lnrpc.ChannelPoint{
							FundingTxid: &lnrpc.ChannelPoint_FundingTxidBytes{
								FundingTxidBytes: event.ChannelPoint.Hash[:],
							},
							OutputIndex: event.ChannelPoint.Index,
						},
					},
				}
			default:
				return fmt.Errorf("unexpected channel event update: %v", event)
			}

			if err := updateStream.Send(update); err != nil {
				return err
			}
		case <-r.quit:
			return nil
		}
	}
}

// savePayment saves a successfully completed payment to the database for
// historical record keeping.
func (r *rpcServer) savePayment(route *routing.Route,
	amount lnwire.MilliSatoshi, preImage []byte) error {

	paymentPath := make([][33]byte, len(route.Hops))
	for i, hop := range route.Hops {
		hopPub := hop.PubKeyBytes
		copy(paymentPath[i][:], hopPub[:])
	}

	payment := &channeldb.OutgoingPayment{
		Invoice: channeldb.Invoice{
			Terms: channeldb.ContractTerm{
				Value: amount,
			},
			CreationDate: time.Now(),
		},
		Path:           paymentPath,
		Fee:            route.TotalFees,
		TimeLockLength: route.TotalTimeLock,
	}
	copy(payment.PaymentPreimage[:], preImage)

	return r.server.chanDB.AddPayment(payment)
}

// validatePayReqExpiry checks if the passed payment request has expired. In
// the case it has expired, an error will be returned.
func validatePayReqExpiry(payReq *zpay32.Invoice) error {
	expiry := payReq.Expiry()
	validUntil := payReq.Timestamp.Add(expiry)
	if time.Now().After(validUntil) {
		return fmt.Errorf("invoice expired. Valid until %v", validUntil)
	}

	return nil
}

// paymentStream enables different types of payment streams, such as:
// lnrpc.Lightning_SendPaymentServer and lnrpc.Lightning_SendToRouteServer to
// execute sendPayment. We use this struct as a sort of bridge to enable code
// re-use between SendPayment and SendToRoute.
type paymentStream struct {
	recv func() (*rpcPaymentRequest, error)
	send func(*lnrpc.SendResponse) error
}

// rpcPaymentRequest wraps lnrpc.SendRequest so that routes from
// lnrpc.SendToRouteRequest can be passed to sendPayment.
type rpcPaymentRequest struct {
	*lnrpc.SendRequest
	routes []*routing.Route
}

// calculateFeeLimit returns the fee limit in millisatoshis. If a percentage
// based fee limit has been requested, we'll factor in the ratio provided with
// the amount of the payment.
func calculateFeeLimit(feeLimit *lnrpc.FeeLimit,
	amount lnwire.MilliSatoshi) lnwire.MilliSatoshi {

	switch feeLimit.GetLimit().(type) {
	case *lnrpc.FeeLimit_Fixed:
		return lnwire.NewMSatFromSatoshis(
			btcutil.Amount(feeLimit.GetFixed()),
		)
	case *lnrpc.FeeLimit_Percent:
		return amount * lnwire.MilliSatoshi(feeLimit.GetPercent()) / 100
	default:
		// If a fee limit was not specified, we'll use the payment's
		// amount as an upper bound in order to avoid payment attempts
		// from incurring fees higher than the payment amount itself.
		return amount
	}
}

// SendPayment dispatches a bi-directional streaming RPC for sending payments
// through the Lightning Network. A single RPC invocation creates a persistent
// bi-directional stream allowing clients to rapidly send payments through the
// Lightning Network with a single persistent connection.
func (r *rpcServer) SendPayment(stream lnrpc.Lightning_SendPaymentServer) error {
	var lock sync.Mutex

	return r.sendPayment(&paymentStream{
		recv: func() (*rpcPaymentRequest, error) {
			req, err := stream.Recv()
			if err != nil {
				return nil, err
			}

			return &rpcPaymentRequest{
				SendRequest: req,
			}, nil
		},
		send: func(r *lnrpc.SendResponse) error {
			// Calling stream.Send concurrently is not safe.
			lock.Lock()
			defer lock.Unlock()
			return stream.Send(r)
		},
	})
}

// SendToRoute dispatches a bi-directional streaming RPC for sending payments
// through the Lightning Network via predefined routes passed in. A single RPC
// invocation creates a persistent bi-directional stream allowing clients to
// rapidly send payments through the Lightning Network with a single persistent
// connection.
func (r *rpcServer) SendToRoute(stream lnrpc.Lightning_SendToRouteServer) error {
	var lock sync.Mutex

	return r.sendPayment(&paymentStream{
		recv: func() (*rpcPaymentRequest, error) {
			req, err := stream.Recv()
			if err != nil {
				return nil, err
			}

			graph := r.server.chanDB.ChannelGraph()

			return unmarshallSendToRouteRequest(req, graph)
		},
		send: func(r *lnrpc.SendResponse) error {
			// Calling stream.Send concurrently is not safe.
			lock.Lock()
			defer lock.Unlock()
			return stream.Send(r)
		},
	})
}

// unmarshallSendToRouteRequest unmarshalls an rpc sendtoroute request
func unmarshallSendToRouteRequest(req *lnrpc.SendToRouteRequest,
	graph *channeldb.ChannelGraph) (*rpcPaymentRequest, error) {

	switch {
	case len(req.Routes) == 0 && req.Route == nil:
		return nil, fmt.Errorf("unable to send, no routes provided")
	case len(req.Routes) > 0 && req.Route != nil:
		return nil, fmt.Errorf("cannot use both route and routes field")
	}

	var routes []*routing.Route
	if len(req.Routes) > 0 {
		routes = make([]*routing.Route, len(req.Routes))
		for i, rpcroute := range req.Routes {
			route, err := unmarshallRoute(rpcroute, graph)
			if err != nil {
				return nil, err
			}
			routes[i] = route
		}
	} else {
		route, err := unmarshallRoute(req.Route, graph)
		if err != nil {
			return nil, err
		}
		routes = []*routing.Route{route}
	}

	return &rpcPaymentRequest{
		SendRequest: &lnrpc.SendRequest{
			PaymentHash:       req.PaymentHash,
			PaymentHashString: req.PaymentHashString,
		},
		routes: routes,
	}, nil
}

// rpcPaymentIntent is a small wrapper struct around the of values we can
// receive from a client over RPC if they wish to send a payment. We'll either
// extract these fields from a payment request (which may include routing
// hints), or we'll get a fully populated route from the user that we'll pass
// directly to the channel router for dispatching.
type rpcPaymentIntent struct {
	msat       lnwire.MilliSatoshi
	feeLimit   lnwire.MilliSatoshi
	dest       *btcec.PublicKey
	rHash      [32]byte
	cltvDelta  uint16
	routeHints [][]routing.HopHint

	routes []*routing.Route
}

// extractPaymentIntent attempts to parse the complete details required to
// dispatch a client from the information presented by an RPC client. There are
// three ways a client can specify their payment details: a payment request,
// via manual details, or via a complete route.
func extractPaymentIntent(rpcPayReq *rpcPaymentRequest) (rpcPaymentIntent, error) {
	var err error
	payIntent := rpcPaymentIntent{}

	// If a route was specified, then we can use that directly.
	if len(rpcPayReq.routes) != 0 {
		// If the user is using the REST interface, then they'll be
		// passing the payment hash as a hex encoded string.
		if rpcPayReq.PaymentHashString != "" {
			paymentHash, err := hex.DecodeString(
				rpcPayReq.PaymentHashString,
			)
			if err != nil {
				return payIntent, err
			}

			copy(payIntent.rHash[:], paymentHash)
		} else {
			copy(payIntent.rHash[:], rpcPayReq.PaymentHash)
		}

		payIntent.routes = rpcPayReq.routes
		return payIntent, nil
	}

	// If the payment request field isn't blank, then the details of the
	// invoice are encoded entirely within the encoded payReq.  So we'll
	// attempt to decode it, populating the payment accordingly.
	if rpcPayReq.PaymentRequest != "" {
		payReq, err := zpay32.Decode(
			rpcPayReq.PaymentRequest, activeNetParams.Params,
		)
		if err != nil {
			return payIntent, err
		}

		// Next, we'll ensure that this payreq hasn't already expired.
		err = validatePayReqExpiry(payReq)
		if err != nil {
			return payIntent, err
		}

		// If the amount was not included in the invoice, then we let
		// the payee specify the amount of satoshis they wish to send.
		// We override the amount to pay with the amount provided from
		// the payment request.
		if payReq.MilliSat == nil {
			if rpcPayReq.Amt == 0 {
				return payIntent, errors.New("amount must be " +
					"specified when paying a zero amount " +
					"invoice")
			}

			payIntent.msat = lnwire.NewMSatFromSatoshis(
				btcutil.Amount(rpcPayReq.Amt),
			)
		} else {
			payIntent.msat = *payReq.MilliSat
		}

		// Calculate the fee limit that should be used for this payment.
		payIntent.feeLimit = calculateFeeLimit(
			rpcPayReq.FeeLimit, payIntent.msat,
		)

		copy(payIntent.rHash[:], payReq.PaymentHash[:])
		payIntent.dest = payReq.Destination
		payIntent.cltvDelta = uint16(payReq.MinFinalCLTVExpiry())
		payIntent.routeHints = payReq.RouteHints

		return payIntent, nil
	}

	// At this point, a destination MUST be specified, so we'll convert it
	// into the proper representation now. The destination will either be
	// encoded as raw bytes, or via a hex string.
	if len(rpcPayReq.Dest) != 0 {
		payIntent.dest, err = btcec.ParsePubKey(
			rpcPayReq.Dest, btcec.S256(),
		)
		if err != nil {
			return payIntent, err
		}

	} else {
		pubBytes, err := hex.DecodeString(rpcPayReq.DestString)
		if err != nil {
			return payIntent, err
		}
		payIntent.dest, err = btcec.ParsePubKey(pubBytes, btcec.S256())
		if err != nil {
			return payIntent, err
		}
	}

	// Otherwise, If the payment request field was not specified
	// (and a custom route wasn't specified), construct the payment
	// from the other fields.
	payIntent.msat = lnwire.NewMSatFromSatoshis(
		btcutil.Amount(rpcPayReq.Amt),
	)

	// Calculate the fee limit that should be used for this payment.
	payIntent.feeLimit = calculateFeeLimit(
		rpcPayReq.FeeLimit, payIntent.msat,
	)

	payIntent.cltvDelta = uint16(rpcPayReq.FinalCltvDelta)

	// If the user is manually specifying payment details, then the payment
	// hash may be encoded as a string.
	switch {
	case rpcPayReq.PaymentHashString != "":
		paymentHash, err := hex.DecodeString(
			rpcPayReq.PaymentHashString,
		)
		if err != nil {
			return payIntent, err
		}

		copy(payIntent.rHash[:], paymentHash)

	// If we're in debug HTLC mode, then all outgoing HTLCs will pay to the
	// same debug rHash. Otherwise, we pay to the rHash specified within
	// the RPC request.
	case cfg.DebugHTLC && bytes.Equal(payIntent.rHash[:], zeroHash[:]):
		copy(payIntent.rHash[:], invoices.DebugHash[:])

	default:
		copy(payIntent.rHash[:], rpcPayReq.PaymentHash)
	}

	// Currently, within the bootstrap phase of the network, we limit the
	// largest payment size allotted to (2^32) - 1 mSAT or 4.29 million
	// satoshis.
	if payIntent.msat > maxPaymentMSat {
		// In this case, we'll send an error to the caller, but
		// continue our loop for the next payment.
		return payIntent, fmt.Errorf("payment of %v is too large, "+
			"max payment allowed is %v", payIntent.msat,
			maxPaymentMSat)

	}

	return payIntent, nil
}

type paymentIntentResponse struct {
	Route    *routing.Route
	Preimage [32]byte
	Err      error
}

// dispatchPaymentIntent attempts to fully dispatch an RPC payment intent.
// We'll either pass the payment as a whole to the channel router, or give it a
// pre-built route. The first error this method returns denotes if we were
// unable to save the payment. The second error returned denotes if the payment
// didn't succeed.
func (r *rpcServer) dispatchPaymentIntent(
	payIntent *rpcPaymentIntent) (*paymentIntentResponse, error) {

	// Construct a payment request to send to the channel router. If the
	// payment is successful, the route chosen will be returned. Otherwise,
	// we'll get a non-nil error.
	var (
		preImage  [32]byte
		route     *routing.Route
		routerErr error
	)

	// If a route was specified, then we'll pass the route directly to the
	// router, otherwise we'll create a payment session to execute it.
	if len(payIntent.routes) == 0 {
		payment := &routing.LightningPayment{
			Target:      payIntent.dest,
			Amount:      payIntent.msat,
			FeeLimit:    payIntent.feeLimit,
			PaymentHash: payIntent.rHash,
			RouteHints:  payIntent.routeHints,
		}

		// If the final CLTV value was specified, then we'll use that
		// rather than the default.
		if payIntent.cltvDelta != 0 {
			payment.FinalCLTVDelta = &payIntent.cltvDelta
		}

		preImage, route, routerErr = r.server.chanRouter.SendPayment(
			payment,
		)
	} else {
		payment := &routing.LightningPayment{
			PaymentHash: payIntent.rHash,
		}

		preImage, route, routerErr = r.server.chanRouter.SendToRoute(
			payIntent.routes, payment,
		)
	}

	// If the route failed, then we'll return a nil save err, but a non-nil
	// routing err.
	if routerErr != nil {
		return &paymentIntentResponse{
			Err: routerErr,
		}, nil
	}

	// If a route was used to complete this payment, then we'll need to
	// compute the final amount sent
	var amt lnwire.MilliSatoshi
	if len(payIntent.routes) > 0 {
		amt = route.TotalAmount - route.TotalFees
	} else {
		amt = payIntent.msat
	}

	// Save the completed payment to the database for record keeping
	// purposes.
	err := r.savePayment(route, amt, preImage[:])
	if err != nil {
		// We weren't able to save the payment, so we return the save
		// err, but a nil routing err.
		return nil, err
	}

	return &paymentIntentResponse{
		Route:    route,
		Preimage: preImage,
	}, nil
}

// sendPayment takes a paymentStream (a source of pre-built routes or payment
// requests) and continually attempt to dispatch payment requests written to
// the write end of the stream. Responses will also be streamed back to the
// client via the write end of the stream. This method is by both SendToRoute
// and SendPayment as the logic is virtually identical.
func (r *rpcServer) sendPayment(stream *paymentStream) error {
	payChan := make(chan *rpcPaymentIntent)
	errChan := make(chan error, 1)

	// We don't allow payments to be sent while the daemon itself is still
	// syncing as we may be trying to sent a payment over a "stale"
	// channel.
	if !r.server.Started() {
		return fmt.Errorf("chain backend is still syncing, server " +
			"not active yet")
	}

	// TODO(roasbeef): check payment filter to see if already used?

	// In order to limit the level of concurrency and prevent a client from
	// attempting to OOM the server, we'll set up a semaphore to create an
	// upper ceiling on the number of outstanding payments.
	const numOutstandingPayments = 2000
	htlcSema := make(chan struct{}, numOutstandingPayments)
	for i := 0; i < numOutstandingPayments; i++ {
		htlcSema <- struct{}{}
	}

	// Launch a new goroutine to handle reading new payment requests from
	// the client. This way we can handle errors independently of blocking
	// and waiting for the next payment request to come through.
	reqQuit := make(chan struct{})
	defer func() {
		close(reqQuit)
	}()

	// TODO(joostjager): Callers expect result to come in in the same order
	// as the request were sent, but this is far from guarantueed in the
	// code below.
	go func() {
		for {
			select {
			case <-reqQuit:
				return
			case <-r.quit:
				errChan <- nil
				return
			default:
				// Receive the next pending payment within the
				// stream sent by the client. If we read the
				// EOF sentinel, then the client has closed the
				// stream, and we can exit normally.
				nextPayment, err := stream.recv()
				if err == io.EOF {
					errChan <- nil
					return
				} else if err != nil {
					select {
					case errChan <- err:
					case <-reqQuit:
						return
					}
					return
				}

				// Populate the next payment, either from the
				// payment request, or from the explicitly set
				// fields. If the payment proto wasn't well
				// formed, then we'll send an error reply and
				// wait for the next payment.
				payIntent, err := extractPaymentIntent(nextPayment)
				if err != nil {
					if err := stream.send(&lnrpc.SendResponse{
						PaymentError: err.Error(),
						PaymentHash:  payIntent.rHash[:],
					}); err != nil {
						select {
						case errChan <- err:
						case <-reqQuit:
							return
						}
					}
					continue
				}

				// If the payment was well formed, then we'll
				// send to the dispatch goroutine, or exit,
				// which ever comes first
				select {
				case payChan <- &payIntent:
				case <-reqQuit:
					return
				}
			}
		}
	}()

	for {
		select {
		case err := <-errChan:
			return err

		case payIntent := <-payChan:
			// We launch a new goroutine to execute the current
			// payment so we can continue to serve requests while
			// this payment is being dispatched.
			go func() {
				// Attempt to grab a free semaphore slot, using
				// a defer to eventually release the slot
				// regardless of payment success.
				<-htlcSema
				defer func() {
					htlcSema <- struct{}{}
				}()

				resp, saveErr := r.dispatchPaymentIntent(
					payIntent,
				)

				switch {
				// If we were unable to save the state of the
				// payment, then we'll return the error to the
				// user, and terminate.
				case saveErr != nil:
					errChan <- saveErr
					return

				// If we receive payment error than, instead of
				// terminating the stream, send error response
				// to the user.
				case resp.Err != nil:
					err := stream.send(&lnrpc.SendResponse{
						PaymentError: resp.Err.Error(),
						PaymentHash:  payIntent.rHash[:],
					})
					if err != nil {
						errChan <- err
					}
					return
				}

				marshalledRouted := r.marshallRoute(resp.Route)
				err := stream.send(&lnrpc.SendResponse{
					PaymentHash:     payIntent.rHash[:],
					PaymentPreimage: resp.Preimage[:],
					PaymentRoute:    marshalledRouted,
				})
				if err != nil {
					errChan <- err
					return
				}
			}()
		}
	}
}

// SendPaymentSync is the synchronous non-streaming version of SendPayment.
// This RPC is intended to be consumed by clients of the REST proxy.
// Additionally, this RPC expects the destination's public key and the payment
// hash (if any) to be encoded as hex strings.
func (r *rpcServer) SendPaymentSync(ctx context.Context,
	nextPayment *lnrpc.SendRequest) (*lnrpc.SendResponse, error) {

	return r.sendPaymentSync(ctx, &rpcPaymentRequest{
		SendRequest: nextPayment,
	})
}

// SendToRouteSync is the synchronous non-streaming version of SendToRoute.
// This RPC is intended to be consumed by clients of the REST proxy.
// Additionally, this RPC expects the payment hash (if any) to be encoded as
// hex strings.
func (r *rpcServer) SendToRouteSync(ctx context.Context,
	req *lnrpc.SendToRouteRequest) (*lnrpc.SendResponse, error) {

	if len(req.Routes) == 0 {
		return nil, fmt.Errorf("unable to send, no routes provided")
	}

	graph := r.server.chanDB.ChannelGraph()

	paymentRequest, err := unmarshallSendToRouteRequest(req, graph)
	if err != nil {
		return nil, err
	}

	return r.sendPaymentSync(ctx, paymentRequest)
}

// sendPaymentSync is the synchronous variant of sendPayment. It will block and
// wait until the payment has been fully completed.
func (r *rpcServer) sendPaymentSync(ctx context.Context,
	nextPayment *rpcPaymentRequest) (*lnrpc.SendResponse, error) {

	// We don't allow payments to be sent while the daemon itself is still
	// syncing as we may be trying to sent a payment over a "stale"
	// channel.
	if !r.server.Started() {
		return nil, fmt.Errorf("chain backend is still syncing, server " +
			"not active yet")
	}

	// First we'll attempt to map the proto describing the next payment to
	// an intent that we can pass to local sub-systems.
	payIntent, err := extractPaymentIntent(nextPayment)
	if err != nil {
		return nil, err
	}

	// With the payment validated, we'll now attempt to dispatch the
	// payment.
	resp, saveErr := r.dispatchPaymentIntent(&payIntent)
	switch {
	case saveErr != nil:
		return nil, saveErr

	case resp.Err != nil:
		return &lnrpc.SendResponse{
			PaymentError: resp.Err.Error(),
			PaymentHash:  payIntent.rHash[:],
		}, nil
	}

	return &lnrpc.SendResponse{
		PaymentHash:     payIntent.rHash[:],
		PaymentPreimage: resp.Preimage[:],
		PaymentRoute:    r.marshallRoute(resp.Route),
	}, nil
}

// AddInvoice attempts to add a new invoice to the invoice database. Any
// duplicated invoices are rejected, therefore all invoices *must* have a
// unique payment preimage.
func (r *rpcServer) AddInvoice(ctx context.Context,
	invoice *lnrpc.Invoice) (*lnrpc.AddInvoiceResponse, error) {

	defaultDelta := cfg.Bitcoin.TimeLockDelta
	if registeredChains.PrimaryChain() == litecoinChain {
		defaultDelta = cfg.Litecoin.TimeLockDelta
	}

	addInvoiceCfg := &invoicesrpc.AddInvoiceConfig{
		InvoiceRegistry:   r.server.invoices,
		Switch:            r.server.htlcSwitch,
		ChainParams:       activeNetParams.Params,
		NodeSigner:        r.server.nodeSigner,
		MaxPaymentMSat:    maxPaymentMSat,
		DefaultCLTVExpiry: defaultDelta,
		ChanDB:            r.server.chanDB,
	}

	var preimage *lntypes.Preimage
	if invoice.RPreimage != nil {
		var err error
		preimage, err = lntypes.NewPreimage(invoice.RPreimage)
		if err != nil {
			return nil, err
		}
	}

	addInvoiceData := &invoicesrpc.AddInvoiceData{
		Memo:            invoice.Memo,
		Receipt:         invoice.Receipt,
		Preimage:        preimage,
		Value:           btcutil.Amount(invoice.Value),
		DescriptionHash: invoice.DescriptionHash,
		Expiry:          invoice.Expiry,
		FallbackAddr:    invoice.FallbackAddr,
		CltvExpiry:      invoice.CltvExpiry,
		Private:         invoice.Private,
	}

	hash, dbInvoice, err := invoicesrpc.AddInvoice(
		ctx, addInvoiceCfg, addInvoiceData,
	)
	if err != nil {
		return nil, err
	}

	return &lnrpc.AddInvoiceResponse{
		AddIndex:       dbInvoice.AddIndex,
		PaymentRequest: string(dbInvoice.PaymentRequest),
		RHash:          hash[:],
	}, nil
}

// LookupInvoice attempts to look up an invoice according to its payment hash.
// The passed payment hash *must* be exactly 32 bytes, if not an error is
// returned.
func (r *rpcServer) LookupInvoice(ctx context.Context,
	req *lnrpc.PaymentHash) (*lnrpc.Invoice, error) {

	var (
		payHash [32]byte
		rHash   []byte
		err     error
	)

	// If the RHash as a raw string was provided, then decode that and use
	// that directly. Otherwise, we use the raw bytes provided.
	if req.RHashStr != "" {
		rHash, err = hex.DecodeString(req.RHashStr)
		if err != nil {
			return nil, err
		}
	} else {
		rHash = req.RHash
	}

	// Ensure that the payment hash is *exactly* 32-bytes.
	if len(rHash) != 0 && len(rHash) != 32 {
		return nil, fmt.Errorf("payment hash must be exactly "+
			"32 bytes, is instead %v", len(rHash))
	}
	copy(payHash[:], rHash)

	rpcsLog.Tracef("[lookupinvoice] searching for invoice %x", payHash[:])

	invoice, _, err := r.server.invoices.LookupInvoice(payHash)
	if err != nil {
		return nil, err
	}

	rpcsLog.Tracef("[lookupinvoice] located invoice %v",
		newLogClosure(func() string {
			return spew.Sdump(invoice)
		}))

	rpcInvoice, err := invoicesrpc.CreateRPCInvoice(
		&invoice, activeNetParams.Params,
	)
	if err != nil {
		return nil, err
	}

	return rpcInvoice, nil
}

// ListInvoices returns a list of all the invoices currently stored within the
// database. Any active debug invoices are ignored.
func (r *rpcServer) ListInvoices(ctx context.Context,
	req *lnrpc.ListInvoiceRequest) (*lnrpc.ListInvoiceResponse, error) {

	// If the number of invoices was not specified, then we'll default to
	// returning the latest 100 invoices.
	if req.NumMaxInvoices == 0 {
		req.NumMaxInvoices = 100
	}

	// Next, we'll map the proto request into a format that is understood by
	// the database.
	q := channeldb.InvoiceQuery{
		IndexOffset:    req.IndexOffset,
		NumMaxInvoices: req.NumMaxInvoices,
		PendingOnly:    req.PendingOnly,
		Reversed:       req.Reversed,
	}
	invoiceSlice, err := r.server.chanDB.QueryInvoices(q)
	if err != nil {
		return nil, fmt.Errorf("unable to query invoices: %v", err)
	}

	// Before returning the response, we'll need to convert each invoice
	// into it's proto representation.
	resp := &lnrpc.ListInvoiceResponse{
		Invoices:         make([]*lnrpc.Invoice, len(invoiceSlice.Invoices)),
		FirstIndexOffset: invoiceSlice.FirstIndexOffset,
		LastIndexOffset:  invoiceSlice.LastIndexOffset,
	}
	for i, invoice := range invoiceSlice.Invoices {
		resp.Invoices[i], err = invoicesrpc.CreateRPCInvoice(
			&invoice, activeNetParams.Params,
		)
		if err != nil {
			return nil, err
		}
	}

	return resp, nil
}

// SubscribeInvoices returns a uni-directional stream (server -> client) for
// notifying the client of newly added/settled invoices.
func (r *rpcServer) SubscribeInvoices(req *lnrpc.InvoiceSubscription,
	updateStream lnrpc.Lightning_SubscribeInvoicesServer) error {

	invoiceClient := r.server.invoices.SubscribeNotifications(
		req.AddIndex, req.SettleIndex,
	)
	defer invoiceClient.Cancel()

	for {
		select {
		case newInvoice := <-invoiceClient.NewInvoices:
			rpcInvoice, err := invoicesrpc.CreateRPCInvoice(
				newInvoice, activeNetParams.Params,
			)
			if err != nil {
				return err
			}

			if err := updateStream.Send(rpcInvoice); err != nil {
				return err
			}

		case settledInvoice := <-invoiceClient.SettledInvoices:
			rpcInvoice, err := invoicesrpc.CreateRPCInvoice(
				settledInvoice, activeNetParams.Params,
			)
			if err != nil {
				return err
			}

			if err := updateStream.Send(rpcInvoice); err != nil {
				return err
			}

		case <-r.quit:
			return nil
		}
	}
}

// SubscribeTransactions creates a uni-directional stream (server -> client) in
// which any newly discovered transactions relevant to the wallet are sent
// over.
func (r *rpcServer) SubscribeTransactions(req *lnrpc.GetTransactionsRequest,
	updateStream lnrpc.Lightning_SubscribeTransactionsServer) error {

	txClient, err := r.server.cc.wallet.SubscribeTransactions()
	if err != nil {
		return err
	}
	defer txClient.Cancel()

	for {
		select {
		case tx := <-txClient.ConfirmedTransactions():
			detail := &lnrpc.Transaction{
				TxHash:           tx.Hash.String(),
				Amount:           int64(tx.Value),
				NumConfirmations: tx.NumConfirmations,
				BlockHash:        tx.BlockHash.String(),
				TimeStamp:        tx.Timestamp,
				TotalFees:        tx.TotalFees,
			}
			if err := updateStream.Send(detail); err != nil {
				return err
			}

		case tx := <-txClient.UnconfirmedTransactions():
			detail := &lnrpc.Transaction{
				TxHash:    tx.Hash.String(),
				Amount:    int64(tx.Value),
				TimeStamp: tx.Timestamp,
				TotalFees: tx.TotalFees,
			}
			if err := updateStream.Send(detail); err != nil {
				return err
			}

		case <-r.quit:
			return nil
		}
	}
}

// GetTransactions returns a list of describing all the known transactions
// relevant to the wallet.
func (r *rpcServer) GetTransactions(ctx context.Context,
	_ *lnrpc.GetTransactionsRequest) (*lnrpc.TransactionDetails, error) {

	// TODO(roasbeef): add pagination support
	transactions, err := r.server.cc.wallet.ListTransactionDetails()
	if err != nil {
		return nil, err
	}

	txDetails := &lnrpc.TransactionDetails{
		Transactions: make([]*lnrpc.Transaction, len(transactions)),
	}
	for i, tx := range transactions {
		var destAddresses []string
		for _, destAddress := range tx.DestAddresses {
			destAddresses = append(destAddresses, destAddress.EncodeAddress())
		}

		// We also get unconfirmed transactions, so BlockHash can be
		// nil.
		blockHash := ""
		if tx.BlockHash != nil {
			blockHash = tx.BlockHash.String()
		}

		txDetails.Transactions[i] = &lnrpc.Transaction{
			TxHash:           tx.Hash.String(),
			Amount:           int64(tx.Value),
			NumConfirmations: tx.NumConfirmations,
			BlockHash:        blockHash,
			BlockHeight:      tx.BlockHeight,
			TimeStamp:        tx.Timestamp,
			TotalFees:        tx.TotalFees,
			DestAddresses:    destAddresses,
		}
	}

	return txDetails, nil
}

// DescribeGraph returns a description of the latest graph state from the PoV
// of the node. The graph information is partitioned into two components: all
// the nodes/vertexes, and all the edges that connect the vertexes themselves.
// As this is a directed graph, the edges also contain the node directional
// specific routing policy which includes: the time lock delta, fee
// information, etc.
func (r *rpcServer) DescribeGraph(ctx context.Context,
	req *lnrpc.ChannelGraphRequest) (*lnrpc.ChannelGraph, error) {

	resp := &lnrpc.ChannelGraph{}
	includeUnannounced := req.IncludeUnannounced

	// Obtain the pointer to the global singleton channel graph, this will
	// provide a consistent view of the graph due to bolt db's
	// transactional model.
	graph := r.server.chanDB.ChannelGraph()

	// First iterate through all the known nodes (connected or unconnected
	// within the graph), collating their current state into the RPC
	// response.
	err := graph.ForEachNode(nil, func(_ *bbolt.Tx, node *channeldb.LightningNode) error {
		nodeAddrs := make([]*lnrpc.NodeAddress, 0)
		for _, addr := range node.Addresses {
			nodeAddr := &lnrpc.NodeAddress{
				Network: addr.Network(),
				Addr:    addr.String(),
			}
			nodeAddrs = append(nodeAddrs, nodeAddr)
		}

		nodeColor := fmt.Sprintf("#%02x%02x%02x", node.Color.R, node.Color.G, node.Color.B)
		resp.Nodes = append(resp.Nodes, &lnrpc.LightningNode{
			LastUpdate: uint32(node.LastUpdate.Unix()),
			PubKey:     hex.EncodeToString(node.PubKeyBytes[:]),
			Addresses:  nodeAddrs,
			Alias:      node.Alias,
			Color:      nodeColor,
		})

		return nil
	})
	if err != nil {
		return nil, err
	}

	// Next, for each active channel we know of within the graph, create a
	// similar response which details both the edge information as well as
	// the routing policies of th nodes connecting the two edges.
	err = graph.ForEachChannel(func(edgeInfo *channeldb.ChannelEdgeInfo,
		c1, c2 *channeldb.ChannelEdgePolicy) error {

		// Do not include unannounced channels unless specifically
		// requested. Unannounced channels include both private channels as
		// well as public channels whose authentication proof were not
		// confirmed yet, hence were not announced.
		if !includeUnannounced && edgeInfo.AuthProof == nil {
			return nil
		}

		edge := marshalDbEdge(edgeInfo, c1, c2)
		resp.Edges = append(resp.Edges, edge)

		return nil
	})
	if err != nil && err != channeldb.ErrGraphNoEdgesFound {
		return nil, err
	}

	return resp, nil
}

func marshalDbEdge(edgeInfo *channeldb.ChannelEdgeInfo,
	c1, c2 *channeldb.ChannelEdgePolicy) *lnrpc.ChannelEdge {

	var (
		lastUpdate int64
	)

	if c2 != nil {
		lastUpdate = c2.LastUpdate.Unix()
	}
	if c1 != nil {
		lastUpdate = c1.LastUpdate.Unix()
	}

	edge := &lnrpc.ChannelEdge{
		ChannelId: edgeInfo.ChannelID,
		ChanPoint: edgeInfo.ChannelPoint.String(),
		// TODO(roasbeef): update should be on edge info itself
		LastUpdate: uint32(lastUpdate),
		Node1Pub:   hex.EncodeToString(edgeInfo.NodeKey1Bytes[:]),
		Node2Pub:   hex.EncodeToString(edgeInfo.NodeKey2Bytes[:]),
		Capacity:   int64(edgeInfo.Capacity),
	}

	if c1 != nil {
		edge.Node1Policy = &lnrpc.RoutingPolicy{
			TimeLockDelta:    uint32(c1.TimeLockDelta),
			MinHtlc:          int64(c1.MinHTLC),
			FeeBaseMsat:      int64(c1.FeeBaseMSat),
			FeeRateMilliMsat: int64(c1.FeeProportionalMillionths),
			Disabled:         c1.ChannelFlags&lnwire.ChanUpdateDisabled != 0,
		}
	}

	if c2 != nil {
		edge.Node2Policy = &lnrpc.RoutingPolicy{
			TimeLockDelta:    uint32(c2.TimeLockDelta),
			MinHtlc:          int64(c2.MinHTLC),
			FeeBaseMsat:      int64(c2.FeeBaseMSat),
			FeeRateMilliMsat: int64(c2.FeeProportionalMillionths),
			Disabled:         c2.ChannelFlags&lnwire.ChanUpdateDisabled != 0,
		}
	}

	return edge
}

// GetChanInfo returns the latest authenticated network announcement for the
// given channel identified by its channel ID: an 8-byte integer which uniquely
// identifies the location of transaction's funding output within the block
// chain.
func (r *rpcServer) GetChanInfo(ctx context.Context,
	in *lnrpc.ChanInfoRequest) (*lnrpc.ChannelEdge, error) {

	graph := r.server.chanDB.ChannelGraph()

	edgeInfo, edge1, edge2, err := graph.FetchChannelEdgesByID(in.ChanId)
	if err != nil {
		return nil, err
	}

	// Convert the database's edge format into the network/RPC edge format
	// which couples the edge itself along with the directional node
	// routing policies of each node involved within the channel.
	channelEdge := marshalDbEdge(edgeInfo, edge1, edge2)

	return channelEdge, nil
}

// GetNodeInfo returns the latest advertised and aggregate authenticated
// channel information for the specified node identified by its public key.
func (r *rpcServer) GetNodeInfo(ctx context.Context,
	in *lnrpc.NodeInfoRequest) (*lnrpc.NodeInfo, error) {

	graph := r.server.chanDB.ChannelGraph()

	// First, parse the hex-encoded public key into a full in-memory public
	// key object we can work with for querying.
	pubKeyBytes, err := hex.DecodeString(in.PubKey)
	if err != nil {
		return nil, err
	}
	pubKey, err := btcec.ParsePubKey(pubKeyBytes, btcec.S256())
	if err != nil {
		return nil, err
	}

	// With the public key decoded, attempt to fetch the node corresponding
	// to this public key. If the node cannot be found, then an error will
	// be returned.
	node, err := graph.FetchLightningNode(pubKey)
	if err != nil {
		return nil, err
	}

	// With the node obtained, we'll now iterate through all its out going
	// edges to gather some basic statistics about its out going channels.
	var (
		numChannels   uint32
		totalCapacity btcutil.Amount
	)
	if err := node.ForEachChannel(nil, func(_ *bbolt.Tx, edge *channeldb.ChannelEdgeInfo,
		_, _ *channeldb.ChannelEdgePolicy) error {

		numChannels++
		totalCapacity += edge.Capacity
		return nil
	}); err != nil {
		return nil, err
	}

	nodeAddrs := make([]*lnrpc.NodeAddress, 0)
	for _, addr := range node.Addresses {
		nodeAddr := &lnrpc.NodeAddress{
			Network: addr.Network(),
			Addr:    addr.String(),
		}
		nodeAddrs = append(nodeAddrs, nodeAddr)
	}
	// TODO(roasbeef): list channels as well?

	nodeColor := fmt.Sprintf("#%02x%02x%02x", node.Color.R, node.Color.G, node.Color.B)
	return &lnrpc.NodeInfo{
		Node: &lnrpc.LightningNode{
			LastUpdate: uint32(node.LastUpdate.Unix()),
			PubKey:     in.PubKey,
			Addresses:  nodeAddrs,
			Alias:      node.Alias,
			Color:      nodeColor,
		},
		NumChannels:   numChannels,
		TotalCapacity: int64(totalCapacity),
	}, nil
}

// QueryRoutes attempts to query the daemons' Channel Router for a possible
// route to a target destination capable of carrying a specific amount of
// satoshis within the route's flow. The retuned route contains the full
// details required to craft and send an HTLC, also including the necessary
// information that should be present within the Sphinx packet encapsulated
// within the HTLC.
//
// TODO(roasbeef): should return a slice of routes in reality
//  * create separate PR to send based on well formatted route
func (r *rpcServer) QueryRoutes(ctx context.Context,
	in *lnrpc.QueryRoutesRequest) (*lnrpc.QueryRoutesResponse, error) {

	// First parse the hex-encoded public key into a full public key object
	// we can properly manipulate.
	pubKeyBytes, err := hex.DecodeString(in.PubKey)
	if err != nil {
		return nil, err
	}
	pubKey, err := btcec.ParsePubKey(pubKeyBytes, btcec.S256())
	if err != nil {
		return nil, err
	}

	// Currently, within the bootstrap phase of the network, we limit the
	// largest payment size allotted to (2^32) - 1 mSAT or 4.29 million
	// satoshis.
	amt := btcutil.Amount(in.Amt)
	amtMSat := lnwire.NewMSatFromSatoshis(amt)
	if amtMSat > maxPaymentMSat {
		return nil, fmt.Errorf("payment of %v is too large, max payment "+
			"allowed is %v", amt, maxPaymentMSat.ToSatoshis())
	}

	feeLimit := calculateFeeLimit(in.FeeLimit, amtMSat)

	// numRoutes will default to 10 if not specified explicitly.
	numRoutesIn := uint32(in.NumRoutes)
	if numRoutesIn == 0 {
		numRoutesIn = 10
	}

	// Query the channel router for a possible path to the destination that
	// can carry `in.Amt` satoshis _including_ the total fee required on
	// the route.
	var (
		routes  []*routing.Route
		findErr error
	)
	if in.FinalCltvDelta == 0 {
		routes, findErr = r.server.chanRouter.FindRoutes(
			pubKey, amtMSat, feeLimit, numRoutesIn,
		)
	} else {
		routes, findErr = r.server.chanRouter.FindRoutes(
			pubKey, amtMSat, feeLimit, numRoutesIn,
			uint16(in.FinalCltvDelta),
		)
	}
	if findErr != nil {
		return nil, findErr
	}

	// As the number of returned routes can be less than the number of
	// requested routes, we'll clamp down the length of the response to the
	// minimum of the two.
	numRoutes := uint32(len(routes))
	if numRoutesIn < numRoutes {
		numRoutes = numRoutesIn
	}

	// For each valid route, we'll convert the result into the format
	// required by the RPC system.
	routeResp := &lnrpc.QueryRoutesResponse{
		Routes: make([]*lnrpc.Route, 0, in.NumRoutes),
	}
	for i := uint32(0); i < numRoutes; i++ {
		routeResp.Routes = append(
			routeResp.Routes, r.marshallRoute(routes[i]),
		)
	}

	return routeResp, nil
}

func (r *rpcServer) marshallRoute(route *routing.Route) *lnrpc.Route {
	resp := &lnrpc.Route{
		TotalTimeLock: route.TotalTimeLock,
		TotalFees:     int64(route.TotalFees.ToSatoshis()),
		TotalFeesMsat: int64(route.TotalFees),
		TotalAmt:      int64(route.TotalAmount.ToSatoshis()),
		TotalAmtMsat:  int64(route.TotalAmount),
		Hops:          make([]*lnrpc.Hop, len(route.Hops)),
	}
	graph := r.server.chanDB.ChannelGraph()
	incomingAmt := route.TotalAmount
	for i, hop := range route.Hops {
		fee := route.HopFee(i)

		// Channel capacity is not a defining property of a route. For
		// backwards RPC compatibility, we retrieve it here from the
		// graph.
		var chanCapacity btcutil.Amount
		info, _, _, err := graph.FetchChannelEdgesByID(hop.ChannelID)
		if err == nil {
			chanCapacity = info.Capacity
		} else {
			// If capacity cannot be retrieved, this may be a
			// not-yet-received or private channel. Then report
			// amount that is sent through the channel as capacity.
			chanCapacity = incomingAmt.ToSatoshis()
		}

		resp.Hops[i] = &lnrpc.Hop{
			ChanId:           hop.ChannelID,
			ChanCapacity:     int64(chanCapacity),
			AmtToForward:     int64(hop.AmtToForward.ToSatoshis()),
			AmtToForwardMsat: int64(hop.AmtToForward),
			Fee:              int64(fee.ToSatoshis()),
			FeeMsat:          int64(fee),
			Expiry:           uint32(hop.OutgoingTimeLock),
			PubKey: hex.EncodeToString(
				hop.PubKeyBytes[:]),
		}
		incomingAmt = hop.AmtToForward
	}

	return resp
}

// unmarshallHopByChannelLookup unmarshalls an rpc hop for which the pub key is
// not known. This function will query the channel graph with channel id to
// retrieve both endpoints and determine the hop pubkey using the previous hop
// pubkey. If the channel is unknown, an error is returned.
func unmarshallHopByChannelLookup(graph *channeldb.ChannelGraph, hop *lnrpc.Hop,
	prevPubKeyBytes [33]byte) (*routing.Hop, error) {

	// Discard edge policies, because they may be nil.
	edgeInfo, _, _, err := graph.FetchChannelEdgesByID(hop.ChanId)
	if err != nil {
		return nil, fmt.Errorf("unable to fetch channel edges by "+
			"channel ID %d: %v", hop.ChanId, err)
	}

	var pubKeyBytes [33]byte
	switch {
	case prevPubKeyBytes == edgeInfo.NodeKey1Bytes:
		pubKeyBytes = edgeInfo.NodeKey2Bytes
	case prevPubKeyBytes == edgeInfo.NodeKey2Bytes:
		pubKeyBytes = edgeInfo.NodeKey1Bytes
	default:
		return nil, fmt.Errorf("channel edge does not match expected node")
	}

	return &routing.Hop{
		OutgoingTimeLock: hop.Expiry,
		AmtToForward:     lnwire.MilliSatoshi(hop.AmtToForwardMsat),
		PubKeyBytes:      pubKeyBytes,
		ChannelID:        edgeInfo.ChannelID,
	}, nil
}

// unmarshallKnownPubkeyHop unmarshalls an rpc hop that contains the hop pubkey.
// The channel graph doesn't need to be queried because all information required
// for sending the payment is present.
func unmarshallKnownPubkeyHop(hop *lnrpc.Hop) (*routing.Hop, error) {
	pubKey, err := hex.DecodeString(hop.PubKey)
	if err != nil {
		return nil, fmt.Errorf("cannot decode pubkey %s", hop.PubKey)
	}

	var pubKeyBytes [33]byte
	copy(pubKeyBytes[:], pubKey)

	return &routing.Hop{
		OutgoingTimeLock: hop.Expiry,
		AmtToForward:     lnwire.MilliSatoshi(hop.AmtToForwardMsat),
		PubKeyBytes:      pubKeyBytes,
		ChannelID:        hop.ChanId,
	}, nil
}

// unmarshallHop unmarshalls an rpc hop that may or may not contain a node
// pubkey.
func unmarshallHop(graph *channeldb.ChannelGraph, hop *lnrpc.Hop,
	prevNodePubKey [33]byte) (*routing.Hop, error) {

	if hop.PubKey == "" {
		// If no pub key is given of the hop, the local channel
		// graph needs to be queried to complete the information
		// necessary for routing.
		return unmarshallHopByChannelLookup(graph, hop, prevNodePubKey)
	}

	return unmarshallKnownPubkeyHop(hop)
}

// unmarshallRoute unmarshalls an rpc route. For hops that don't specify a
// pubkey, the channel graph is queried.
func unmarshallRoute(rpcroute *lnrpc.Route,
	graph *channeldb.ChannelGraph) (*routing.Route, error) {

	sourceNode, err := graph.SourceNode()
	if err != nil {
		return nil, fmt.Errorf("unable to fetch source node from graph "+
			"while unmarshaling route. %v", err)
	}

	prevNodePubKey := sourceNode.PubKeyBytes

	hops := make([]*routing.Hop, len(rpcroute.Hops))
	for i, hop := range rpcroute.Hops {
		routeHop, err := unmarshallHop(graph,
			hop, prevNodePubKey)
		if err != nil {
			return nil, err
		}

		hops[i] = routeHop

		prevNodePubKey = routeHop.PubKeyBytes
	}

	route, err := routing.NewRouteFromHops(
		lnwire.MilliSatoshi(rpcroute.TotalAmtMsat),
		rpcroute.TotalTimeLock,
		sourceNode.PubKeyBytes,
		hops,
	)
	if err != nil {
		return nil, err
	}

	return route, nil
}

// GetNetworkInfo returns some basic stats about the known channel graph from
// the PoV of the node.
func (r *rpcServer) GetNetworkInfo(ctx context.Context,
	_ *lnrpc.NetworkInfoRequest) (*lnrpc.NetworkInfo, error) {

	graph := r.server.chanDB.ChannelGraph()

	var (
		numNodes             uint32
		numChannels          uint32
		maxChanOut           uint32
		totalNetworkCapacity btcutil.Amount
		minChannelSize       btcutil.Amount = math.MaxInt64
		maxChannelSize       btcutil.Amount
	)

	// We'll use this map to de-duplicate channels during our traversal.
	// This is needed since channels are directional, so there will be two
	// edges for each channel within the graph.
	seenChans := make(map[uint64]struct{})

	// We'll run through all the known nodes in the within our view of the
	// network, tallying up the total number of nodes, and also gathering
	// each node so we can measure the graph diameter and degree stats
	// below.
	if err := graph.ForEachNode(nil, func(tx *bbolt.Tx, node *channeldb.LightningNode) error {
		// Increment the total number of nodes with each iteration.
		numNodes++

		// For each channel we'll compute the out degree of each node,
		// and also update our running tallies of the min/max channel
		// capacity, as well as the total channel capacity. We pass
		// through the db transaction from the outer view so we can
		// re-use it within this inner view.
		var outDegree uint32
		if err := node.ForEachChannel(tx, func(_ *bbolt.Tx,
			edge *channeldb.ChannelEdgeInfo, _, _ *channeldb.ChannelEdgePolicy) error {

			// Bump up the out degree for this node for each
			// channel encountered.
			outDegree++

			// If we've already seen this channel, then we'll
			// return early to ensure that we don't double-count
			// stats.
			if _, ok := seenChans[edge.ChannelID]; ok {
				return nil
			}

			// Compare the capacity of this channel against the
			// running min/max to see if we should update the
			// extrema.
			chanCapacity := edge.Capacity
			if chanCapacity < minChannelSize {
				minChannelSize = chanCapacity
			}
			if chanCapacity > maxChannelSize {
				maxChannelSize = chanCapacity
			}

			// Accumulate the total capacity of this channel to the
			// network wide-capacity.
			totalNetworkCapacity += chanCapacity

			numChannels++

			seenChans[edge.ChannelID] = struct{}{}
			return nil
		}); err != nil {
			return err
		}

		// Finally, if the out degree of this node is greater than what
		// we've seen so far, update the maxChanOut variable.
		if outDegree > maxChanOut {
			maxChanOut = outDegree
		}

		return nil
	}); err != nil {
		return nil, err
	}

	// If we don't have any channels, then reset the minChannelSize to zero
	// to avoid outputting NaN in encoded JSON.
	if numChannels == 0 {
		minChannelSize = 0
	}

	// TODO(roasbeef): graph diameter

	// TODO(roasbeef): also add oldest channel?
	//  * also add median channel size
	netInfo := &lnrpc.NetworkInfo{
		MaxOutDegree:         maxChanOut,
		AvgOutDegree:         float64(numChannels) / float64(numNodes),
		NumNodes:             numNodes,
		NumChannels:          numChannels,
		TotalNetworkCapacity: int64(totalNetworkCapacity),
		AvgChannelSize:       float64(totalNetworkCapacity) / float64(numChannels),

		MinChannelSize: int64(minChannelSize),
		MaxChannelSize: int64(maxChannelSize),
	}

	// Similarly, if we don't have any channels, then we'll also set the
	// average channel size to zero in order to avoid weird JSON encoding
	// outputs.
	if numChannels == 0 {
		netInfo.AvgChannelSize = 0
	}

	return netInfo, nil
}

// StopDaemon will send a shutdown request to the interrupt handler, triggering
// a graceful shutdown of the daemon.
func (r *rpcServer) StopDaemon(ctx context.Context,
	_ *lnrpc.StopRequest) (*lnrpc.StopResponse, error) {

	signal.RequestShutdown()
	return &lnrpc.StopResponse{}, nil
}

// SubscribeChannelGraph launches a streaming RPC that allows the caller to
// receive notifications upon any changes the channel graph topology from the
// review of the responding node. Events notified include: new nodes coming
// online, nodes updating their authenticated attributes, new channels being
// advertised, updates in the routing policy for a directional channel edge,
// and finally when prior channels are closed on-chain.
func (r *rpcServer) SubscribeChannelGraph(req *lnrpc.GraphTopologySubscription,
	updateStream lnrpc.Lightning_SubscribeChannelGraphServer) error {

	// First, we start by subscribing to a new intent to receive
	// notifications from the channel router.
	client, err := r.server.chanRouter.SubscribeTopology()
	if err != nil {
		return err
	}

	// Ensure that the resources for the topology update client is cleaned
	// up once either the server, or client exists.
	defer client.Cancel()

	for {
		select {

		// A new update has been sent by the channel router, we'll
		// marshal it into the form expected by the gRPC client, then
		// send it off.
		case topChange, ok := <-client.TopologyChanges:
			// If the second value from the channel read is nil,
			// then this means that the channel router is exiting
			// or the notification client was cancelled. So we'll
			// exit early.
			if !ok {
				return errors.New("server shutting down")
			}

			// Convert the struct from the channel router into the
			// form expected by the gRPC service then send it off
			// to the client.
			graphUpdate := marshallTopologyChange(topChange)
			if err := updateStream.Send(graphUpdate); err != nil {
				return err
			}

		// The server is quitting, so we'll exit immediately. Returning
		// nil will close the clients read end of the stream.
		case <-r.quit:
			return nil
		}
	}
}

// marshallTopologyChange performs a mapping from the topology change struct
// returned by the router to the form of notifications expected by the current
// gRPC service.
func marshallTopologyChange(topChange *routing.TopologyChange) *lnrpc.GraphTopologyUpdate {

	// encodeKey is a simple helper function that converts a live public
	// key into a hex-encoded version of the compressed serialization for
	// the public key.
	encodeKey := func(k *btcec.PublicKey) string {
		return hex.EncodeToString(k.SerializeCompressed())
	}

	nodeUpdates := make([]*lnrpc.NodeUpdate, len(topChange.NodeUpdates))
	for i, nodeUpdate := range topChange.NodeUpdates {
		addrs := make([]string, len(nodeUpdate.Addresses))
		for i, addr := range nodeUpdate.Addresses {
			addrs[i] = addr.String()
		}

		nodeUpdates[i] = &lnrpc.NodeUpdate{
			Addresses:      addrs,
			IdentityKey:    encodeKey(nodeUpdate.IdentityKey),
			GlobalFeatures: nodeUpdate.GlobalFeatures,
			Alias:          nodeUpdate.Alias,
		}
	}

	channelUpdates := make([]*lnrpc.ChannelEdgeUpdate, len(topChange.ChannelEdgeUpdates))
	for i, channelUpdate := range topChange.ChannelEdgeUpdates {
		channelUpdates[i] = &lnrpc.ChannelEdgeUpdate{
			ChanId: channelUpdate.ChanID,
			ChanPoint: &lnrpc.ChannelPoint{
				FundingTxid: &lnrpc.ChannelPoint_FundingTxidBytes{
					FundingTxidBytes: channelUpdate.ChanPoint.Hash[:],
				},
				OutputIndex: channelUpdate.ChanPoint.Index,
			},
			Capacity: int64(channelUpdate.Capacity),
			RoutingPolicy: &lnrpc.RoutingPolicy{
				TimeLockDelta:    uint32(channelUpdate.TimeLockDelta),
				MinHtlc:          int64(channelUpdate.MinHTLC),
				FeeBaseMsat:      int64(channelUpdate.BaseFee),
				FeeRateMilliMsat: int64(channelUpdate.FeeRate),
				Disabled:         channelUpdate.Disabled,
			},
			AdvertisingNode: encodeKey(channelUpdate.AdvertisingNode),
			ConnectingNode:  encodeKey(channelUpdate.ConnectingNode),
		}
	}

	closedChans := make([]*lnrpc.ClosedChannelUpdate, len(topChange.ClosedChannels))
	for i, closedChan := range topChange.ClosedChannels {
		closedChans[i] = &lnrpc.ClosedChannelUpdate{
			ChanId:       closedChan.ChanID,
			Capacity:     int64(closedChan.Capacity),
			ClosedHeight: closedChan.ClosedHeight,
			ChanPoint: &lnrpc.ChannelPoint{
				FundingTxid: &lnrpc.ChannelPoint_FundingTxidBytes{
					FundingTxidBytes: closedChan.ChanPoint.Hash[:],
				},
				OutputIndex: closedChan.ChanPoint.Index,
			},
		}
	}

	return &lnrpc.GraphTopologyUpdate{
		NodeUpdates:    nodeUpdates,
		ChannelUpdates: channelUpdates,
		ClosedChans:    closedChans,
	}
}

// ListPayments returns a list of all outgoing payments.
func (r *rpcServer) ListPayments(ctx context.Context,
	_ *lnrpc.ListPaymentsRequest) (*lnrpc.ListPaymentsResponse, error) {

	rpcsLog.Debugf("[ListPayments]")

	payments, err := r.server.chanDB.FetchAllPayments()
	if err != nil && err != channeldb.ErrNoPaymentsCreated {
		return nil, err
	}

	paymentsResp := &lnrpc.ListPaymentsResponse{
		Payments: make([]*lnrpc.Payment, len(payments)),
	}
	for i, payment := range payments {
		path := make([]string, len(payment.Path))
		for i, hop := range payment.Path {
			path[i] = hex.EncodeToString(hop[:])
		}

		msatValue := int64(payment.Terms.Value)
		satValue := int64(payment.Terms.Value.ToSatoshis())

		paymentHash := sha256.Sum256(payment.PaymentPreimage[:])
		paymentsResp.Payments[i] = &lnrpc.Payment{
			PaymentHash:     hex.EncodeToString(paymentHash[:]),
			Value:           satValue,
			ValueMsat:       msatValue,
			ValueSat:        satValue,
			CreationDate:    payment.CreationDate.Unix(),
			Path:            path,
			Fee:             int64(payment.Fee.ToSatoshis()),
			PaymentPreimage: hex.EncodeToString(payment.PaymentPreimage[:]),
		}
	}

	return paymentsResp, nil
}

// DeleteAllPayments deletes all outgoing payments from DB.
func (r *rpcServer) DeleteAllPayments(ctx context.Context,
	_ *lnrpc.DeleteAllPaymentsRequest) (*lnrpc.DeleteAllPaymentsResponse, error) {

	rpcsLog.Debugf("[DeleteAllPayments]")

	if err := r.server.chanDB.DeleteAllPayments(); err != nil {
		return nil, err
	}

	return &lnrpc.DeleteAllPaymentsResponse{}, nil
}

// DebugLevel allows a caller to programmatically set the logging verbosity of
// lnd. The logging can be targeted according to a coarse daemon-wide logging
// level, or in a granular fashion to specify the logging for a target
// sub-system.
func (r *rpcServer) DebugLevel(ctx context.Context,
	req *lnrpc.DebugLevelRequest) (*lnrpc.DebugLevelResponse, error) {

	// If show is set, then we simply print out the list of available
	// sub-systems.
	if req.Show {
		return &lnrpc.DebugLevelResponse{
			SubSystems: strings.Join(supportedSubsystems(), " "),
		}, nil
	}

	rpcsLog.Infof("[debuglevel] changing debug level to: %v", req.LevelSpec)

	// Otherwise, we'll attempt to set the logging level using the
	// specified level spec.
	if err := parseAndSetDebugLevels(req.LevelSpec); err != nil {
		return nil, err
	}

	return &lnrpc.DebugLevelResponse{}, nil
}

// DecodePayReq takes an encoded payment request string and attempts to decode
// it, returning a full description of the conditions encoded within the
// payment request.
func (r *rpcServer) DecodePayReq(ctx context.Context,
	req *lnrpc.PayReqString) (*lnrpc.PayReq, error) {

	rpcsLog.Tracef("[decodepayreq] decoding: %v", req.PayReq)

	// Fist we'll attempt to decode the payment request string, if the
	// request is invalid or the checksum doesn't match, then we'll exit
	// here with an error.
	payReq, err := zpay32.Decode(req.PayReq, activeNetParams.Params)
	if err != nil {
		return nil, err
	}

	// Let the fields default to empty strings.
	desc := ""
	if payReq.Description != nil {
		desc = *payReq.Description
	}

	descHash := []byte("")
	if payReq.DescriptionHash != nil {
		descHash = payReq.DescriptionHash[:]
	}

	fallbackAddr := ""
	if payReq.FallbackAddr != nil {
		fallbackAddr = payReq.FallbackAddr.String()
	}

	// Expiry time will default to 3600 seconds if not specified
	// explicitly.
	expiry := int64(payReq.Expiry().Seconds())

	// Convert between the `lnrpc` and `routing` types.
	routeHints := invoicesrpc.CreateRPCRouteHints(payReq.RouteHints)

	amt := int64(0)
	if payReq.MilliSat != nil {
		amt = int64(payReq.MilliSat.ToSatoshis())
	}

	dest := payReq.Destination.SerializeCompressed()
	return &lnrpc.PayReq{
		Destination:     hex.EncodeToString(dest),
		PaymentHash:     hex.EncodeToString(payReq.PaymentHash[:]),
		NumSatoshis:     amt,
		Timestamp:       payReq.Timestamp.Unix(),
		Description:     desc,
		DescriptionHash: hex.EncodeToString(descHash[:]),
		FallbackAddr:    fallbackAddr,
		Expiry:          expiry,
		CltvExpiry:      int64(payReq.MinFinalCLTVExpiry()),
		RouteHints:      routeHints,
	}, nil
}

// feeBase is the fixed point that fee rate computation are performed over.
// Nodes on the network advertise their fee rate using this point as a base.
// This means that the minimal possible fee rate if 1e-6, or 0.000001, or
// 0.0001%.
const feeBase = 1000000

// FeeReport allows the caller to obtain a report detailing the current fee
// schedule enforced by the node globally for each channel.
func (r *rpcServer) FeeReport(ctx context.Context,
	_ *lnrpc.FeeReportRequest) (*lnrpc.FeeReportResponse, error) {

	// TODO(roasbeef): use UnaryInterceptor to add automated logging

	rpcsLog.Debugf("[feereport]")

	channelGraph := r.server.chanDB.ChannelGraph()
	selfNode, err := channelGraph.SourceNode()
	if err != nil {
		return nil, err
	}

	var feeReports []*lnrpc.ChannelFeeReport
	err = selfNode.ForEachChannel(nil, func(_ *bbolt.Tx, chanInfo *channeldb.ChannelEdgeInfo,
		edgePolicy, _ *channeldb.ChannelEdgePolicy) error {

		// Self node should always have policies for its channels.
		if edgePolicy == nil {
			return fmt.Errorf("no policy for outgoing channel %v ",
				chanInfo.ChannelID)
		}

		// We'll compute the effective fee rate by converting from a
		// fixed point fee rate to a floating point fee rate. The fee
		// rate field in the database the amount of mSAT charged per
		// 1mil mSAT sent, so will divide by this to get the proper fee
		// rate.
		feeRateFixedPoint := edgePolicy.FeeProportionalMillionths
		feeRate := float64(feeRateFixedPoint) / float64(feeBase)

		// TODO(roasbeef): also add stats for revenue for each channel
		feeReports = append(feeReports, &lnrpc.ChannelFeeReport{
			ChanPoint:   chanInfo.ChannelPoint.String(),
			BaseFeeMsat: int64(edgePolicy.FeeBaseMSat),
			FeePerMil:   int64(feeRateFixedPoint),
			FeeRate:     feeRate,
		})

		return nil
	})
	if err != nil {
		return nil, err
	}

	fwdEventLog := r.server.chanDB.ForwardingLog()

	// computeFeeSum is a helper function that computes the total fees for
	// a particular time slice described by a forwarding event query.
	computeFeeSum := func(query channeldb.ForwardingEventQuery) (lnwire.MilliSatoshi, error) {

		var totalFees lnwire.MilliSatoshi

		// We'll continue to fetch the next query and accumulate the
		// fees until the next query returns no events.
		for {
			timeSlice, err := fwdEventLog.Query(query)
			if err != nil {
				return 0, nil
			}

			// If the timeslice is empty, then we'll return as
			// we've retrieved all the entries in this range.
			if len(timeSlice.ForwardingEvents) == 0 {
				break
			}

			// Otherwise, we'll tally up an accumulate the total
			// fees for this time slice.
			for _, event := range timeSlice.ForwardingEvents {
				fee := event.AmtIn - event.AmtOut
				totalFees += fee
			}

			// We'll now take the last offset index returned as
			// part of this response, and modify our query to start
			// at this index. This has a pagination effect in the
			// case that our query bounds has more than 100k
			// entries.
			query.IndexOffset = timeSlice.LastIndexOffset
		}

		return totalFees, nil
	}

	now := time.Now()

	// Before we perform the queries below, we'll instruct the switch to
	// flush any pending events to disk. This ensure we get a complete
	// snapshot at this particular time.
	if r.server.htlcSwitch.FlushForwardingEvents(); err != nil {
		return nil, fmt.Errorf("unable to flush forwarding "+
			"events: %v", err)
	}

	// In addition to returning the current fee schedule for each channel.
	// We'll also perform a series of queries to obtain the total fees
	// earned over the past day, week, and month.
	dayQuery := channeldb.ForwardingEventQuery{
		StartTime:    now.Add(-time.Hour * 24),
		EndTime:      now,
		NumMaxEvents: 1000,
	}
	dayFees, err := computeFeeSum(dayQuery)
	if err != nil {
		return nil, fmt.Errorf("unable to retrieve day fees: %v", err)
	}

	weekQuery := channeldb.ForwardingEventQuery{
		StartTime:    now.Add(-time.Hour * 24 * 7),
		EndTime:      now,
		NumMaxEvents: 1000,
	}
	weekFees, err := computeFeeSum(weekQuery)
	if err != nil {
		return nil, fmt.Errorf("unable to retrieve day fees: %v", err)
	}

	monthQuery := channeldb.ForwardingEventQuery{
		StartTime:    now.Add(-time.Hour * 24 * 30),
		EndTime:      now,
		NumMaxEvents: 1000,
	}
	monthFees, err := computeFeeSum(monthQuery)
	if err != nil {
		return nil, fmt.Errorf("unable to retrieve day fees: %v", err)
	}

	return &lnrpc.FeeReportResponse{
		ChannelFees: feeReports,
		DayFeeSum:   uint64(dayFees.ToSatoshis()),
		WeekFeeSum:  uint64(weekFees.ToSatoshis()),
		MonthFeeSum: uint64(monthFees.ToSatoshis()),
	}, nil
}

// minFeeRate is the smallest permitted fee rate within the network. This is
// derived by the fact that fee rates are computed using a fixed point of
// 1,000,000. As a result, the smallest representable fee rate is 1e-6, or
// 0.000001, or 0.0001%.
const minFeeRate = 1e-6

// UpdateChannelPolicy allows the caller to update the channel forwarding policy
// for all channels globally, or a particular channel.
func (r *rpcServer) UpdateChannelPolicy(ctx context.Context,
	req *lnrpc.PolicyUpdateRequest) (*lnrpc.PolicyUpdateResponse, error) {

	var targetChans []wire.OutPoint
	switch scope := req.Scope.(type) {
	// If the request is targeting all active channels, then we don't need
	// target any channels by their channel point.
	case *lnrpc.PolicyUpdateRequest_Global:

	// Otherwise, we're targeting an individual channel by its channel
	// point.
	case *lnrpc.PolicyUpdateRequest_ChanPoint:
		txidHash, err := getChanPointFundingTxid(scope.ChanPoint)
		if err != nil {
			return nil, err
		}
		txid, err := chainhash.NewHash(txidHash)
		if err != nil {
			return nil, err
		}
		targetChans = append(targetChans, wire.OutPoint{
			Hash:  *txid,
			Index: scope.ChanPoint.OutputIndex,
		})
	default:
		return nil, fmt.Errorf("unknown scope: %v", scope)
	}

	// As a sanity check, we'll ensure that the passed fee rate is below
	// 1e-6, or the lowest allowed fee rate, and that the passed timelock
	// is large enough.
	if req.FeeRate < minFeeRate {
		return nil, fmt.Errorf("fee rate of %v is too small, min fee "+
			"rate is %v", req.FeeRate, minFeeRate)
	}

	if req.TimeLockDelta < minTimeLockDelta {
		return nil, fmt.Errorf("time lock delta of %v is too small, "+
			"minimum supported is %v", req.TimeLockDelta,
			minTimeLockDelta)
	}

	// We'll also need to convert the floating point fee rate we accept
	// over RPC to the fixed point rate that we use within the protocol. We
	// do this by multiplying the passed fee rate by the fee base. This
	// gives us the fixed point, scaled by 1 million that's used within the
	// protocol.
	feeRateFixed := uint32(req.FeeRate * feeBase)
	baseFeeMsat := lnwire.MilliSatoshi(req.BaseFeeMsat)
	feeSchema := routing.FeeSchema{
		BaseFee: baseFeeMsat,
		FeeRate: feeRateFixed,
	}

	chanPolicy := routing.ChannelPolicy{
		FeeSchema:     feeSchema,
		TimeLockDelta: req.TimeLockDelta,
	}

	rpcsLog.Debugf("[updatechanpolicy] updating channel policy base_fee=%v, "+
		"rate_float=%v, rate_fixed=%v, time_lock_delta: %v, targets=%v",
		req.BaseFeeMsat, req.FeeRate, feeRateFixed, req.TimeLockDelta,
		spew.Sdump(targetChans))

	// With the scope resolved, we'll now send this to the
	// AuthenticatedGossiper so it can propagate the new policy for our
	// target channel(s).
	err := r.server.authGossiper.PropagateChanPolicyUpdate(
		chanPolicy, targetChans...,
	)
	if err != nil {
		return nil, err
	}

	// Finally, we'll apply the set of active links amongst the target
	// channels.
	//
	// We create a partially policy as the logic won't overwrite a valid
	// sub-policy with a "nil" one.
	p := htlcswitch.ForwardingPolicy{
		BaseFee:       baseFeeMsat,
		FeeRate:       lnwire.MilliSatoshi(feeRateFixed),
		TimeLockDelta: req.TimeLockDelta,
	}
	err = r.server.htlcSwitch.UpdateForwardingPolicies(p, targetChans...)
	if err != nil {
		// If we're unable update the fees due to the links not being
		// online, then we don't need to fail the call. We'll simply
		// log the failure.
		rpcsLog.Warnf("Unable to update link fees: %v", err)
	}

	return &lnrpc.PolicyUpdateResponse{}, nil
}

// ForwardingHistory allows the caller to query the htlcswitch for a record of
// all HTLC's forwarded within the target time range, and integer offset within
// that time range. If no time-range is specified, then the first chunk of the
// past 24 hrs of forwarding history are returned.

// A list of forwarding events are returned. The size of each forwarding event
// is 40 bytes, and the max message size able to be returned in gRPC is 4 MiB.
// In order to safely stay under this max limit, we'll return 50k events per
// response.  Each response has the index offset of the last entry. The index
// offset can be provided to the request to allow the caller to skip a series
// of records.
func (r *rpcServer) ForwardingHistory(ctx context.Context,
	req *lnrpc.ForwardingHistoryRequest) (*lnrpc.ForwardingHistoryResponse, error) {

	rpcsLog.Debugf("[forwardinghistory]")

	// Before we perform the queries below, we'll instruct the switch to
	// flush any pending events to disk. This ensure we get a complete
	// snapshot at this particular time.
	if err := r.server.htlcSwitch.FlushForwardingEvents(); err != nil {
		return nil, fmt.Errorf("unable to flush forwarding "+
			"events: %v", err)
	}

	var (
		startTime, endTime time.Time

		numEvents uint32
	)

	// If the start and end time were not set, then we'll just return the
	// records over the past 24 hours.
	if req.StartTime == 0 && req.EndTime == 0 {
		now := time.Now()
		startTime = now.Add(-time.Hour * 24)
		endTime = now
	} else {
		startTime = time.Unix(int64(req.StartTime), 0)
		endTime = time.Unix(int64(req.EndTime), 0)
	}

	// If the number of events wasn't specified, then we'll default to
	// returning the last 100 events.
	numEvents = req.NumMaxEvents
	if numEvents == 0 {
		numEvents = 100
	}

	// Next, we'll map the proto request into a format the is understood by
	// the forwarding log.
	eventQuery := channeldb.ForwardingEventQuery{
		StartTime:    startTime,
		EndTime:      endTime,
		IndexOffset:  req.IndexOffset,
		NumMaxEvents: numEvents,
	}
	timeSlice, err := r.server.chanDB.ForwardingLog().Query(eventQuery)
	if err != nil {
		return nil, fmt.Errorf("unable to query forwarding log: %v", err)
	}

	// TODO(roasbeef): add settlement latency?
	//  * use FPE on all records?

	// With the events retrieved, we'll now map them into the proper proto
	// response.
	//
	// TODO(roasbeef): show in ns for the outside?
	resp := &lnrpc.ForwardingHistoryResponse{
		ForwardingEvents: make([]*lnrpc.ForwardingEvent, len(timeSlice.ForwardingEvents)),
		LastOffsetIndex:  timeSlice.LastIndexOffset,
	}
	for i, event := range timeSlice.ForwardingEvents {
		amtInSat := event.AmtIn.ToSatoshis()
		amtOutSat := event.AmtOut.ToSatoshis()
		feeMsat := event.AmtIn - event.AmtOut

		resp.ForwardingEvents[i] = &lnrpc.ForwardingEvent{
			Timestamp: uint64(event.Timestamp.Unix()),
			ChanIdIn:  event.IncomingChanID.ToUint64(),
			ChanIdOut: event.OutgoingChanID.ToUint64(),
			AmtIn:     uint64(amtInSat),
			AmtOut:    uint64(amtOutSat),
			Fee:       uint64(feeMsat.ToSatoshis()),
			FeeMsat:   uint64(feeMsat),
		}
	}

	return resp, nil
}
