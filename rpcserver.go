package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"strconv"
	"strings"
	"time"

	"gopkg.in/macaroon-bakery.v2/bakery"

	"sync"
	"sync/atomic"

	"github.com/boltdb/bolt"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/lightningnetwork/lnd/zpay32"
	"github.com/roasbeef/btcd/blockchain"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/chaincfg"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcd/txscript"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
	"github.com/roasbeef/btcwallet/waddrmgr"
	"github.com/tv42/zbase32"
	"golang.org/x/net/context"
)

var (
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
	}

	// permissions maps RPC calls to the permissions they require.
	permissions = map[string][]bakery.Op{
		"/lnrpc.Lightning/SendCoins": {{
			Entity: "onchain",
			Action: "write",
		}},
		"/lnrpc.Lightning/SendMany": {{
			Entity: "onchain",
			Action: "write",
		}},
		"/lnrpc.Lightning/NewAddress": {{
			Entity: "address",
			Action: "write",
		}},
		"/lnrpc.Lightning/NewWitnessAddress": {{
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
		"/lnrpc.Lightning/OpenchannelSync": {{
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
		"/lnrpc.Lightning/SendPayment": {{
			Entity: "offchain",
			Action: "write",
		}},
		"/lnrpc.Lightning/SendPaymentSync": {{
			Entity: "offchain",
			Action: "write",
		}},
		"/lnrpc.Lightning/AddInvoice": {{
			Entity: "offchain",
			Action: "write",
		}},
		"/lnrpc.Lightning/LookupInvoice": {{
			Entity: "offchain",
			Action: "read",
		}},
		"/lnrpc.Lightning/ListInvoices": {{
			Entity: "offchain",
			Action: "read",
		}},
		"/lnrpc.Lightning/SubscribeInvoices": {{
			Entity: "offchain",
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
	}
)

const (
	// maxPaymentMSat is the maximum allowed payment permitted currently as
	// defined in BOLT-0002.
	maxPaymentMSat = lnwire.MilliSatoshi(math.MaxUint32)
)

// rpcServer is a gRPC, RPC front end to the lnd daemon.
// TODO(roasbeef): pagination support for the list-style calls
type rpcServer struct {
	started  int32 // To be used atomically.
	shutdown int32 // To be used atomically.

	server *server

	wg sync.WaitGroup

	quit chan struct{}
}

// A compile time check to ensure that rpcServer fully implements the
// LightningServer gRPC service.
var _ lnrpc.LightningServer = (*rpcServer)(nil)

// newRPCServer creates and returns a new instance of the rpcServer.
func newRPCServer(s *server) *rpcServer {
	return &rpcServer{
		server: s,
		quit:   make(chan struct{}, 1),
	}
}

// Start launches any helper goroutines required for the rpcServer to function.
func (r *rpcServer) Start() error {
	if atomic.AddInt32(&r.started, 1) != 1 {
		return nil
	}

	return nil
}

// Stop signals any active goroutines for a graceful closure.
func (r *rpcServer) Stop() error {
	if atomic.AddInt32(&r.shutdown, 1) != 1 {
		return nil
	}

	close(r.quit)

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
	feePerByte btcutil.Amount) (*chainhash.Hash, error) {

	outputs, err := addrPairsToOutputs(paymentMap)
	if err != nil {
		return nil, err
	}

	return r.server.cc.wallet.SendOutputs(outputs, feePerByte)
}

// determineFeePerByte will determine the fee in sat/byte that should be paid
// given an estimator, a confirmation target, and a manual value for sat/byte.
// A value is chosen based on the two free parameters as one, or both of them
// can be zero.
func determineFeePerByte(feeEstimator lnwallet.FeeEstimator, targetConf int32,
	satPerByte int64) (btcutil.Amount, error) {

	switch {
	// If the target number of confirmations is set, then we'll use that to
	// consult our fee estimator for an adequate fee.
	case targetConf != 0:
		satPerByte, err := feeEstimator.EstimateFeePerByte(
			uint32(targetConf),
		)
		if err != nil {
			return 0, fmt.Errorf("unable to query fee "+
				"estimator: %v", err)
		}

		return btcutil.Amount(satPerByte), nil

	// If a manual sat/byte fee rate is set, then we'll use that directly.
	case satPerByte != 0:
		return btcutil.Amount(satPerByte), nil

	// Otherwise, we'll attempt a relaxed confirmation target for the
	// transaction
	default:
		satPerByte, err := feeEstimator.EstimateFeePerByte(6)
		if err != nil {
			return 0, fmt.Errorf("unable to query fee "+
				"estimator: %v", err)
		}

		return satPerByte, nil
	}
}

// SendCoins executes a request to send coins to a particular address. Unlike
// SendMany, this RPC call only allows creating a single output at a time.
func (r *rpcServer) SendCoins(ctx context.Context,
	in *lnrpc.SendCoinsRequest) (*lnrpc.SendCoinsResponse, error) {

	// Based on the passed fee related parameters, we'll determine an
	// appropriate fee rate for this transaction.
	feePerByte, err := determineFeePerByte(
		r.server.cc.feeEstimator, in.TargetConf, in.SatPerByte,
	)
	if err != nil {
		return nil, err
	}

	rpcsLog.Infof("[sendcoins] addr=%v, amt=%v, sat/byte=%v",
		in.Addr, btcutil.Amount(in.Amount), int64(feePerByte))

	paymentMap := map[string]int64{in.Addr: in.Amount}
	txid, err := r.sendCoinsOnChain(paymentMap, feePerByte)
	if err != nil {
		return nil, err
	}

	rpcsLog.Infof("[sendcoins] spend generated txid: %v", txid.String())

	return &lnrpc.SendCoinsResponse{Txid: txid.String()}, nil
}

// SendMany handles a request for a transaction create multiple specified
// outputs in parallel.
func (r *rpcServer) SendMany(ctx context.Context,
	in *lnrpc.SendManyRequest) (*lnrpc.SendManyResponse, error) {

	// Based on the passed fee related parameters, we'll determine an
	// approriate fee rate for this transaction.
	feePerByte, err := determineFeePerByte(
		r.server.cc.feeEstimator, in.TargetConf, in.SatPerByte,
	)
	if err != nil {
		return nil, err
	}

	rpcsLog.Infof("[sendmany] outputs=%v, sat/byte=%v",
		spew.Sdump(in.AddrToAmount), int64(feePerByte))

	txid, err := r.sendCoinsOnChain(in.AddrToAmount, feePerByte)
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
	case lnrpc.NewAddressRequest_WITNESS_PUBKEY_HASH:
		addrType = lnwallet.WitnessPubKey
	case lnrpc.NewAddressRequest_NESTED_PUBKEY_HASH:
		addrType = lnwallet.NestedWitnessPubKey
	case lnrpc.NewAddressRequest_PUBKEY_HASH:
		addrType = lnwallet.PubKeyHash
	}

	addr, err := r.server.cc.wallet.NewAddress(addrType, false)
	if err != nil {
		return nil, err
	}

	rpcsLog.Infof("[newaddress] addr=%v", addr.String())
	return &lnrpc.NewAddressResponse{Address: addr.String()}, nil
}

// NewWitnessAddress returns a new native witness address under the control of
// the local wallet.
func (r *rpcServer) NewWitnessAddress(ctx context.Context,
	in *lnrpc.NewWitnessAddressRequest) (*lnrpc.NewAddressResponse, error) {

	addr, err := r.server.cc.wallet.NewAddress(
		lnwallet.NestedWitnessPubKey, false,
	)
	if err != nil {
		return nil, err
	}

	rpcsLog.Infof("[newaddress] addr=%v", addr.String())
	return &lnrpc.NewAddressResponse{Address: addr.String()}, nil
}

// SignMessage signs a message with the resident node's private key. The
// returned signature string is zbase32 encoded and pubkey recoverable,
// meaning that only the message digest and signature are needed for
// verification.
func (r *rpcServer) SignMessage(ctx context.Context,
	in *lnrpc.SignMessageRequest) (*lnrpc.SignMessageResponse, error) {

	if in.Msg == nil {
		return nil, fmt.Errorf("need a message to sign")
	}

	sigBytes, err := r.server.nodeSigner.SignCompact(in.Msg)
	if err != nil {
		return nil, err
	}

	sig := zbase32.EncodeToString(sigBytes)
	return &lnrpc.SignMessageResponse{Signature: sig}, nil
}

// VerifyMessage verifies a signature over a msg. The signature must be
// zbase32 encoded and signed by an active node in the resident node's
// channel database. In addition to returning the validity of the signature,
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

	// If the address doesn't already have a port, we'll assume the current
	// default port.
	var addr string
	_, _, err = net.SplitHostPort(in.Addr.Host)
	if err != nil {
		addr = net.JoinHostPort(in.Addr.Host, strconv.Itoa(defaultPeerPort))
	} else {
		addr = in.Addr.Host
	}

	// We use ResolveTCPAddr here in case we wish to resolve hosts over Tor.
	host, err := cfg.net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		return nil, err
	}

	peerAddr := &lnwire.NetAddress{
		IdentityKey: pubKey,
		Address:     host,
		ChainNet:    activeNetParams.Net,
	}

	if err := r.server.ConnectToPeer(peerAddr, in.Perm); err != nil {
		rpcsLog.Errorf("(connectpeer): error connecting to peer: %v", err)
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
	if len(nodeChannels) > 0 {
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

// OpenChannel attempts to open a singly funded channel specified in the
// request to a remote peer.
func (r *rpcServer) OpenChannel(in *lnrpc.OpenChannelRequest,
	updateStream lnrpc.Lightning_OpenChannelServer) error {

	rpcsLog.Tracef("[openchannel] request to peerid(%v) "+
		"allocation(us=%v, them=%v)", in.TargetPeerId,
		in.LocalFundingAmount, in.PushSat)

	if !r.server.Started() {
		return fmt.Errorf("chain backend is still syncing, server " +
			"not active yet")
	}

	localFundingAmt := btcutil.Amount(in.LocalFundingAmount)
	remoteInitialBalance := btcutil.Amount(in.PushSat)
	minHtlc := lnwire.MilliSatoshi(in.MinHtlcMsat)

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

	const minChannelSize = btcutil.Amount(6000)

	// Restrict the size of the channel we'll actually open. Atm, we
	// require the amount to be above 6k satoshis we currently hard-coded
	// a 5k satoshi fee in several areas. As a result 6k sat is the min
	// channel size that allows us to safely sit above the dust threshold
	// after fees are applied
	// TODO(roasbeef): remove after dynamic fees are in
	if localFundingAmt < minChannelSize {
		return fmt.Errorf("channel is too small, the minimum channel "+
			"size is: %v (6k sat)", minChannelSize)
	}

	var (
		nodePubKey      *btcec.PublicKey
		nodePubKeyBytes []byte
		err             error
	)

	// TODO(roasbeef): also return channel ID?

	// If the node key is set, then we'll parse the raw bytes into a pubkey
	// object so we can easily manipulate it. If this isn't set, then we
	// expected the TargetPeerId to be set accordingly.
	if len(in.NodePubkey) != 0 {
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
	}

	// Based on the passed fee related parameters, we'll determine an
	// appropriate fee rate for the funding transaction.
	feePerByte, err := determineFeePerByte(
		r.server.cc.feeEstimator, in.TargetConf, in.SatPerByte,
	)
	if err != nil {
		return err
	}

	rpcsLog.Debugf("[openchannel]: using fee of %v sat/byte for funding "+
		"tx", int64(feePerByte))

	// Instruct the server to trigger the necessary events to attempt to
	// open a new channel. A stream is returned in place, this stream will
	// be used to consume updates of the state of the pending channel.
	updateChan, errChan := r.server.OpenChannel(
		in.TargetPeerId, nodePubKey, localFundingAmt,
		lnwire.NewMSatFromSatoshis(remoteInitialBalance),
		minHtlc, feePerByte, in.Private,
	)

	var outpoint wire.OutPoint
out:
	for {
		select {
		case err := <-errChan:
			rpcsLog.Errorf("unable to open channel to "+
				"identityPub(%x) nor peerID(%v): %v",
				nodePubKeyBytes, in.TargetPeerId, err)
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

	rpcsLog.Tracef("[openchannel] success peerid(%v), ChannelPoint(%v)",
		in.TargetPeerId, outpoint)
	return nil
}

// OpenChannelSync is a synchronous version of the OpenChannel RPC call. This
// call is meant to be consumed by clients to the REST proxy. As with all other
// sync calls, all byte slices are instead to be populated as hex encoded
// strings.
func (r *rpcServer) OpenChannelSync(ctx context.Context,
	in *lnrpc.OpenChannelRequest) (*lnrpc.ChannelPoint, error) {

	rpcsLog.Tracef("[openchannel] request to peerid(%v) "+
		"allocation(us=%v, them=%v)", in.TargetPeerId,
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
	isSynced, err := r.server.cc.wallet.IsSynced()
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

	// Ensure that the initial balance of the remote party (if pushing
	// satoshis) does not exceed the amount the local party has requested
	// for funding.
	if remoteInitialBalance >= localFundingAmt {
		return nil, fmt.Errorf("amount pushed to remote peer for " +
			"initial state must be below the local funding amount")
	}

	// Based on the passed fee related parameters, we'll determine an
	// appropriate fee rate for the funding transaction.
	feePerByte, err := determineFeePerByte(
		r.server.cc.feeEstimator, in.TargetConf, in.SatPerByte,
	)
	if err != nil {
		return nil, err
	}

	rpcsLog.Tracef("[openchannel] target sat/byte for funding tx: %v",
		int64(feePerByte))

	updateChan, errChan := r.server.OpenChannel(
		in.TargetPeerId, nodepubKey, localFundingAmt,
		lnwire.NewMSatFromSatoshis(remoteInitialBalance),
		minHtlc, feePerByte, in.Private,
	)

	select {
	// If an error occurs them immediately return the error to the client.
	case err := <-errChan:
		rpcsLog.Errorf("unable to open channel to "+
			"identityPub(%x) nor peerID(%v): %v",
			nodepubKey, in.TargetPeerId, err)
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
		updateChan chan *lnrpc.CloseStatusUpdate
		errChan    chan error
	)

	// TODO(roasbeef): if force and peer online then don't force?

	// If a force closure was requested, then we'll handle all the details
	// around the creation and broadcast of the unilateral closure
	// transaction here rather than going to the switch as we don't require
	// interaction from the peer.
	if force {
		// As the first part of the force closure, we first fetch the
		// channel from the database, then execute a direct force
		// closure broadcasting our current commitment transaction.
		channel, err := r.fetchActiveChannel(*chanPoint)
		if err != nil {
			return err
		}
		channel.Stop()

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

		select {
		case r.server.breachArbiter.settledContracts <- *chanPoint:
		case <-r.quit:
			return fmt.Errorf("server shutting down")
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
		updateChan = make(chan *lnrpc.CloseStatusUpdate, 2)
		updateChan <- &lnrpc.CloseStatusUpdate{
			Update: &lnrpc.CloseStatusUpdate_ClosePending{
				ClosePending: &lnrpc.PendingUpdate{
					Txid: closingTxid[:],
				},
			},
		}

		errChan = make(chan error, 1)
		notifier := r.server.cc.chainNotifier
		go waitForChanToClose(uint32(bestHeight), notifier, errChan, chanPoint,
			&closingTxid, func() {
				// Respond to the local subsystem which
				// requested the channel closure.
				updateChan <- &lnrpc.CloseStatusUpdate{
					Update: &lnrpc.CloseStatusUpdate_ChanClose{
						ChanClose: &lnrpc.ChannelCloseUpdate{
							ClosingTxid: closingTxid[:],
							Success:     true,
						},
					},
				}
			})
	} else {
		// Based on the passed fee related parameters, we'll determine
		// an appropriate fee rate for the cooperative closure
		// transaction.
		feePerByte, err := determineFeePerByte(
			r.server.cc.feeEstimator, in.TargetConf, in.SatPerByte,
		)
		if err != nil {
			return err
		}

		rpcsLog.Debugf("Target sat/byte for closing transaction: %v",
			int64(feePerByte))

		// When crating commitment transaction, or closure
		// transactions, we typically deal in fees per-kw, so we'll
		// convert now before passing the close request to the switch.
		feePerWeight := (feePerByte / blockchain.WitnessScaleFactor)
		if feePerWeight == 0 {
			// If the fee rate returned isn't usable, then we'll
			// fall back to an lax fee estimate.
			feePerWeight, err = r.server.cc.feeEstimator.EstimateFeePerWeight(6)
			if err != nil {
				return err
			}
		}

		// Otherwise, the caller has requested a regular interactive
		// cooperative channel closure. So we'll forward the request to
		// the htlc switch which will handle the negotiation and
		// broadcast details.
		feePerKw := feePerWeight * 1000
		updateChan, errChan = r.server.htlcSwitch.CloseLink(chanPoint,
			htlcswitch.CloseRegular, feePerKw)
	}
out:
	for {
		select {
		case err := <-errChan:
			rpcsLog.Errorf("[closechannel] unable to close "+
				"ChannelPoint(%v): %v", chanPoint, err)
			return err
		case closingUpdate := <-updateChan:
			rpcsLog.Tracef("[closechannel] sending update: %v",
				closingUpdate)
			if err := updateStream.Send(closingUpdate); err != nil {
				return err
			}

			// If a final channel closing updates is being sent,
			// then we can break out of our dispatch loop as we no
			// longer need to process any further updates.
			switch closeUpdate := closingUpdate.Update.(type) {
			case *lnrpc.CloseStatusUpdate_ChanClose:
				h, _ := chainhash.NewHash(closeUpdate.ChanClose.ClosingTxid)
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

// fetchActiveChannel attempts to locate a channel identified by its channel
// point from the database's set of all currently opened channels.
func (r *rpcServer) fetchActiveChannel(chanPoint wire.OutPoint) (*lnwallet.LightningChannel, error) {
	dbChannels, err := r.server.chanDB.FetchAllChannels()
	if err != nil {
		return nil, err
	}

	// With the channels fetched, attempt to locate the target channel
	// according to its channel point.
	var dbChan *channeldb.OpenChannel
	for _, dbChannel := range dbChannels {
		if dbChannel.FundingOutpoint == chanPoint {
			dbChan = dbChannel
			break
		}
	}

	// If the channel cannot be located, then we exit with an error to the
	// caller.
	if dbChan == nil {
		return nil, fmt.Errorf("unable to find channel")
	}

	// Otherwise, we create a fully populated channel state machine which
	// uses the db channel as backing storage.
	return lnwallet.NewLightningChannel(
		r.server.cc.wallet.Cfg.Signer, nil, dbChan,
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

	isSynced, err := r.server.cc.wallet.IsSynced()
	if err != nil {
		return nil, fmt.Errorf("unable to sync PoV of the wallet "+
			"with current best block in the main chain: %v", err)
	}

	activeChains := make([]string, registeredChains.NumActiveChains())
	for i, chain := range registeredChains.ActiveChains() {
		activeChains[i] = chain.String()
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
		IdentityPubkey:     encodedIDPub,
		NumPendingChannels: nPendingChannels,
		NumActiveChannels:  activeChannels,
		NumPeers:           uint32(len(serverPeers)),
		BlockHeight:        uint32(bestHeight),
		BlockHash:          bestHash.String(),
		SyncedToChain:      isSynced,
		Testnet:            activeNetParams.Params == &chaincfg.TestNet3Params,
		Chains:             activeChains,
		Uris:               uris,
		Alias:              nodeAnn.Alias.String(),
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
			PeerId:    serverPeer.id,
			Address:   serverPeer.conn.RemoteAddr().String(),
			Inbound:   !serverPeer.inbound, // Flip for display
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
	totalBal, err := r.server.cc.wallet.ConfirmedBalance(0, in.WitnessOnly)
	if err != nil {
		return nil, err
	}

	// Get confirmed balance, from txs that have >= 1 confirmations.
	confirmedBal, err := r.server.cc.wallet.ConfirmedBalance(1, in.WitnessOnly)
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

	channels, err := r.server.chanDB.FetchAllChannels()
	if err != nil {
		return nil, err
	}

	var balance btcutil.Amount
	for _, channel := range channels {
		if !channel.IsPending {
			balance += channel.LocalCommitment.LocalBalance.ToSatoshis()
		}
	}

	return &lnrpc.ChannelBalanceResponse{Balance: int64(balance)}, nil
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
		commitWeight := commitBaseWeight + lnwallet.WitnessCommitmentTxWeight

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
		case channeldb.ForceClose:
			forceClose := &lnrpc.PendingChannelsResponse_ForceClosedChannel{
				Channel:     channel,
				ClosingTxid: closeTXID,
			}

			// Query for the maturity state for this force closed
			// channel. If we didn't have any time-locked outputs,
			// then the nursery may not know of the contract.
			nurseryInfo, err := r.server.utxoNursery.NurseryReport(&chanPoint)
			if err != nil && err != ErrContractNotFound {
				return nil, fmt.Errorf("unable to obtain "+
					"nursery report for ChannelPoint(%v): %v",
					chanPoint, err)
			}

			// If the nursery knows of this channel, then we can
			// populate information detailing exactly how much
			// funds are time locked and also the height in which
			// we can ultimately sweep the funds into the wallet.
			if nurseryInfo != nil {
				forceClose.LimboBalance = int64(nurseryInfo.limboBalance)
				forceClose.RecoveredBalance = int64(nurseryInfo.recoveredBalance)
				forceClose.MaturityHeight = nurseryInfo.maturityHeight

				// If the transaction has been confirmed, then
				// we can compute how many blocks it has left.
				if forceClose.MaturityHeight != 0 {
					forceClose.BlocksTilMaturity =
						int32(forceClose.MaturityHeight) -
							currentHeight
				}

				for _, htlcReport := range nurseryInfo.htlcs {
					// TODO(conner) set incoming flag
					// appropriately after handling incoming
					// incubation
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

				resp.TotalLimboBalance += int64(nurseryInfo.limboBalance)
			}

			resp.PendingForceClosingChannels = append(
				resp.PendingForceClosingChannels,
				forceClose,
			)
		}
	}

	return resp, nil
}

// ListChannels returns a description of all the open channels that this node
// is a participant in.
func (r *rpcServer) ListChannels(ctx context.Context,
	in *lnrpc.ListChannelsRequest) (*lnrpc.ListChannelsResponse, error) {

	resp := &lnrpc.ListChannelsResponse{}

	graph := r.server.chanDB.ChannelGraph()

	dbChannels, err := r.server.chanDB.FetchAllChannels()
	if err != nil {
		return nil, err
	}

	rpcsLog.Infof("[listchannels] fetched %v channels from DB",
		len(dbChannels))

	for _, dbChannel := range dbChannels {
		if dbChannel.IsPending {
			continue
		}

		nodePub := dbChannel.IdentityPub
		nodeID := hex.EncodeToString(nodePub.SerializeCompressed())
		chanPoint := dbChannel.FundingOutpoint

		// With the channel point known, retrieve the network channel
		// ID from the database.
		var chanID uint64
		chanID, _ = graph.ChannelID(&chanPoint)

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

		// As this is required for display purposes, we'll calculate
		// the weight of the commitment transaction. We also add on the
		// estimated weight of the witness to calculate the weight of
		// the transaction if it were to be immediately unilaterally
		// broadcast.
		localCommit := dbChannel.LocalCommitment
		utx := btcutil.NewTx(localCommit.CommitTx)
		commitBaseWeight := blockchain.GetTransactionWeight(utx)
		commitWeight := commitBaseWeight + lnwallet.WitnessCommitmentTxWeight

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

		channel := &lnrpc.ActiveChannel{
			Active:                peerOnline && linkActive,
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
		}

		for i, htlc := range localCommit.Htlcs {
			channel.PendingHtlcs[i] = &lnrpc.HTLC{
				Incoming:         htlc.Incoming,
				Amount:           int64(htlc.Amt.ToSatoshis()),
				HashLock:         htlc.RHash[:],
				ExpirationHeight: htlc.RefundTimeout,
			}
		}

		resp.Channels = append(resp.Channels, channel)
	}

	return resp, nil
}

// savePayment saves a successfully completed payment to the database for
// historical record keeping.
func (r *rpcServer) savePayment(route *routing.Route, amount lnwire.MilliSatoshi, preImage []byte) error {

	paymentPath := make([][33]byte, len(route.Hops))
	for i, hop := range route.Hops {
		hopPub := hop.Channel.Node.PubKeyBytes
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

// SendPayment dispatches a bi-directional streaming RPC for sending payments
// through the Lightning Network. A single RPC invocation creates a persistent
// bi-directional stream allowing clients to rapidly send payments through the
// Lightning Network with a single persistent connection.
func (r *rpcServer) SendPayment(paymentStream lnrpc.Lightning_SendPaymentServer) error {
	// For each payment we need to know the msat amount, the destination
	// public key, and the payment hash.
	type payment struct {
		msat      lnwire.MilliSatoshi
		dest      []byte
		pHash     []byte
		cltvDelta uint16
	}
	payChan := make(chan *payment)
	errChan := make(chan error, 1)

	// TODO(roasbeef): enforce fee limits, pass into router, ditch if exceed limit
	//  * limit either a %, or absolute, or iff more than sending

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
				nextPayment, err := paymentStream.Recv()
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
				// fields.
				p := &payment{}

				// If the payment request field isn't blank,
				// then the details of the invoice are encoded
				// entirely within the encoded payReq. So we'll
				// attempt to decode it, populating the
				// payment accordingly.
				if nextPayment.PaymentRequest != "" {
					payReq, err := zpay32.Decode(nextPayment.PaymentRequest)
					if err != nil {
						select {
						case errChan <- err:
						case <-reqQuit:
						}
						return
					}

					// TODO(roasbeef): eliminate necessary
					// encode/decode

					// We first check that this payment
					// request has not expired.
					err = validatePayReqExpiry(payReq)
					if err != nil {
						select {
						case errChan <- err:
						case <-reqQuit:
						}
						return
					}
					p.dest = payReq.Destination.SerializeCompressed()

					// If the amount was not included in the
					// invoice, then we let the payee
					// specify the amount of satoshis they
					// wish to send. We override the amount
					// to pay with the amount provided from
					// the payment request.
					if payReq.MilliSat == nil {
						p.msat = lnwire.NewMSatFromSatoshis(
							btcutil.Amount(nextPayment.Amt),
						)
					} else {
						p.msat = *payReq.MilliSat
					}

					p.pHash = payReq.PaymentHash[:]
					p.cltvDelta = uint16(payReq.MinFinalCLTVExpiry())
				} else {
					// If the payment request field was not
					// specified, construct the payment from
					// the other fields.
					p.msat = lnwire.NewMSatFromSatoshis(
						btcutil.Amount(nextPayment.Amt),
					)
					p.dest = nextPayment.Dest
					p.pHash = nextPayment.PaymentHash
					p.cltvDelta = uint16(nextPayment.FinalCltvDelta)
				}

				select {
				case payChan <- p:
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
		case p := <-payChan:
			// Currently, within the bootstrap phase of the
			// network, we limit the largest payment size allotted
			// to (2^32) - 1 mSAT or 4.29 million satoshis.
			if p.msat > maxPaymentMSat {
				// In this case, we'll send an error to the
				// caller, but continue our loop for the next
				// payment.
				pErr := fmt.Errorf("payment of %v is too "+
					"large, max payment allowed is %v",
					p.msat, maxPaymentMSat)

				if err := paymentStream.Send(&lnrpc.SendResponse{
					PaymentError: pErr.Error(),
				}); err != nil {
					return err
				}
				continue
			}

			// Parse the details of the payment which include the
			// pubkey of the destination and the payment amount.
			destNode, err := btcec.ParsePubKey(p.dest, btcec.S256())
			if err != nil {
				return err
			}

			// If we're in debug HTLC mode, then all outgoing HTLCs
			// will pay to the same debug rHash. Otherwise, we pay
			// to the rHash specified within the RPC request.
			var rHash [32]byte
			if cfg.DebugHTLC && len(p.pHash) == 0 {
				rHash = debugHash
			} else {
				copy(rHash[:], p.pHash)
			}

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

				// Construct a payment request to send to the
				// channel router. If the payment is
				// successful, the route chosen will be
				// returned. Otherwise, we'll get a non-nil
				// error.
				payment := &routing.LightningPayment{
					Target:      destNode,
					Amount:      p.msat,
					PaymentHash: rHash,
				}
				if p.cltvDelta != 0 {
					payment.FinalCLTVDelta = &p.cltvDelta
				}
				preImage, route, err := r.server.chanRouter.SendPayment(payment)
				if err != nil {
					// If we receive payment error than,
					// instead of terminating the stream,
					// send error response to the user.
					err := paymentStream.Send(&lnrpc.SendResponse{
						PaymentError: err.Error(),
					})
					if err != nil {
						errChan <- err
					}
					return
				}

				// Save the completed payment to the database
				// for record keeping purposes.
				if err := r.savePayment(route, p.msat, preImage[:]); err != nil {
					errChan <- err
					return
				}

				err = paymentStream.Send(&lnrpc.SendResponse{
					PaymentPreimage: preImage[:],
					PaymentRoute:    marshallRoute(route),
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

	// TODO(roasbeef): enforce fee limits, pass into router, ditch if exceed limit
	//  * limit either a %, or absolute, or iff more than sending

	// We don't allow payments to be sent while the daemon itself is still
	// syncing as we may be trying to sent a payment over a "stale"
	// channel.
	if !r.server.Started() {
		return nil, fmt.Errorf("chain backend is still syncing, server " +
			"not active yet")
	}

	var (
		destPub   *btcec.PublicKey
		amtMSat   lnwire.MilliSatoshi
		rHash     [32]byte
		cltvDelta uint16
	)

	// If the proto request has an encoded payment request, then we we'll
	// use that solely to dispatch the payment.
	if nextPayment.PaymentRequest != "" {
		payReq, err := zpay32.Decode(nextPayment.PaymentRequest)
		if err != nil {
			return nil, err
		}

		// We first check that this payment request has not expired.
		if err := validatePayReqExpiry(payReq); err != nil {
			return nil, err
		}

		destPub = payReq.Destination

		// If the amount was not included in the invoice, then we let
		// the payee specify the amount of satoshis they wish to send.
		// We override the amount to pay with the amount provided from
		// the payment request.
		if payReq.MilliSat == nil {
			amtMSat = lnwire.NewMSatFromSatoshis(
				btcutil.Amount(nextPayment.Amt),
			)
		} else {
			amtMSat = *payReq.MilliSat
		}

		rHash = *payReq.PaymentHash
		cltvDelta = uint16(payReq.MinFinalCLTVExpiry())

		// Otherwise, the payment conditions have been manually
		// specified in the proto.
	} else {
		// If we're in debug HTLC mode, then all outgoing HTLCs will
		// pay to the same debug rHash. Otherwise, we pay to the rHash
		// specified within the RPC request.
		if cfg.DebugHTLC && nextPayment.PaymentHashString == "" {
			rHash = debugHash
		} else {
			paymentHash, err := hex.DecodeString(nextPayment.PaymentHashString)
			if err != nil {
				return nil, err
			}

			copy(rHash[:], paymentHash)
		}

		pubBytes, err := hex.DecodeString(nextPayment.DestString)
		if err != nil {
			return nil, err
		}
		destPub, err = btcec.ParsePubKey(pubBytes, btcec.S256())
		if err != nil {
			return nil, err
		}

		amtMSat = lnwire.NewMSatFromSatoshis(
			btcutil.Amount(nextPayment.Amt),
		)
	}

	// Currently, within the bootstrap phase of the network, we limit the
	// largest payment size allotted to (2^32) - 1 mSAT or 4.29 million
	// satoshis.
	if amtMSat > maxPaymentMSat {
		err := fmt.Errorf("payment of %v is too large, max payment "+
			"allowed is %v", nextPayment.Amt, maxPaymentMSat.ToSatoshis())
		return &lnrpc.SendResponse{
			PaymentError: err.Error(),
		}, nil
	}

	// Finally, send a payment request to the channel router. If the
	// payment succeeds, then the returned route will be that was used
	// successfully within the payment.
	payment := &routing.LightningPayment{
		Target:      destPub,
		Amount:      amtMSat,
		PaymentHash: rHash,
	}
	if cltvDelta != 0 {
		payment.FinalCLTVDelta = &cltvDelta
	}
	preImage, route, err := r.server.chanRouter.SendPayment(payment)
	if err != nil {
		return &lnrpc.SendResponse{
			PaymentError: err.Error(),
		}, nil
	}

	// With the payment completed successfully, we now ave the details of
	// the completed payment to the database for historical record keeping.
	if err := r.savePayment(route, amtMSat, preImage[:]); err != nil {
		return nil, err
	}

	return &lnrpc.SendResponse{
		PaymentPreimage: preImage[:],
		PaymentRoute:    marshallRoute(route),
	}, nil
}

// AddInvoice attempts to add a new invoice to the invoice database. Any
// duplicated invoices are rejected, therefore all invoices *must* have a
// unique payment preimage.
func (r *rpcServer) AddInvoice(ctx context.Context,
	invoice *lnrpc.Invoice) (*lnrpc.AddInvoiceResponse, error) {

	var paymentPreimage [32]byte

	switch {
	// If a preimage wasn't specified, then we'll generate a new preimage
	// from fresh cryptographic randomness.
	case len(invoice.RPreimage) == 0:
		if _, err := rand.Read(paymentPreimage[:]); err != nil {
			return nil, err
		}

	// Otherwise, if a preimage was specified, then it MUST be exactly
	// 32-bytes.
	case len(invoice.RPreimage) > 0 && len(invoice.RPreimage) != 32:
		return nil, fmt.Errorf("payment preimage must be exactly "+
			"32 bytes, is instead %v", len(invoice.RPreimage))

	// If the preimage meets the size specifications, then it can be used
	// as is.
	default:
		copy(paymentPreimage[:], invoice.RPreimage[:])
	}

	// The size of the memo, receipt and description hash attached must not
	// exceed the maximum values for either of the fields.
	if len(invoice.Memo) > channeldb.MaxMemoSize {
		return nil, fmt.Errorf("memo too large: %v bytes "+
			"(maxsize=%v)", len(invoice.Memo), channeldb.MaxMemoSize)
	}
	if len(invoice.Receipt) > channeldb.MaxReceiptSize {
		return nil, fmt.Errorf("receipt too large: %v bytes "+
			"(maxsize=%v)", len(invoice.Receipt), channeldb.MaxReceiptSize)
	}
	if len(invoice.DescriptionHash) > 0 && len(invoice.DescriptionHash) != 32 {
		return nil, fmt.Errorf("description hash is %v bytes, must be %v",
			len(invoice.DescriptionHash), channeldb.MaxPaymentRequestSize)
	}

	amt := btcutil.Amount(invoice.Value)
	amtMSat := lnwire.NewMSatFromSatoshis(amt)

	// The value of the invoice must also not exceed the current soft-limit
	// on the largest payment within the network.
	if amtMSat > maxPaymentMSat {
		return nil, fmt.Errorf("payment of %v is too large, max "+
			"payment allowed is %v", amt, maxPaymentMSat.ToSatoshis())
	}

	// Next, generate the payment hash itself from the preimage. This will
	// be used by clients to query for the state of a particular invoice.
	rHash := sha256.Sum256(paymentPreimage[:])

	// We also create an encoded payment request which allows the
	// caller to compactly send the invoice to the payer. We'll create a
	// list of options to be added to the encoded payment request. For now
	// we only support the required fields description/description_hash,
	// expiry, fallback address, and the amount field.
	var options []func(*zpay32.Invoice)

	// We only include the amount in the invoice if it is greater than 0.
	// By not including the amount, we enable the creation of invoices that
	// allow the payee to specify the amount of satoshis they wish to send.
	if amtMSat > 0 {
		options = append(options, zpay32.Amount(amtMSat))
	}

	// If specified, add a fallback address to the payment request.
	if len(invoice.FallbackAddr) > 0 {
		addr, err := btcutil.DecodeAddress(invoice.FallbackAddr,
			activeNetParams.Params)
		if err != nil {
			return nil, fmt.Errorf("invalid fallback address: %v",
				err)
		}
		options = append(options, zpay32.FallbackAddr(addr))
	}

	// If expiry is set, specify it. If it is not provided, no expiry time
	// will be explicitly added to this payment request, which will imply
	// the default 3600 seconds.
	if invoice.Expiry > 0 {
		exp := time.Duration(invoice.Expiry) * time.Second
		options = append(options, zpay32.Expiry(exp))
	}

	// If the description hash is set, then we add it do the list of options.
	// If not, use the memo field as the payment request description.
	if len(invoice.DescriptionHash) > 0 {
		var descHash [32]byte
		copy(descHash[:], invoice.DescriptionHash[:])
		options = append(options, zpay32.DescriptionHash(descHash))
	} else {
		// Use the memo field as the description. If this is not set
		// this will just be an empty string.
		options = append(options, zpay32.Description(invoice.Memo))
	}

	// We'll use our current default CLTV value unless one was specified as
	// an option on the command line when creating an invoice.
	switch {
	case invoice.CltvExpiry > math.MaxUint16:
		return nil, fmt.Errorf("CLTV delta of %v is too large, max "+
			"accepted is: %v", invoice.CltvExpiry, math.MaxUint16)
	case invoice.CltvExpiry != 0:
		options = append(options,
			zpay32.CLTVExpiry(invoice.CltvExpiry))
	default:
		// TODO(roasbeef): assumes set delta between versions
		defaultDelta := cfg.Bitcoin.TimeLockDelta
		options = append(options, zpay32.CLTVExpiry(uint64(defaultDelta)))
	}

	// Create and encode the payment request as a bech32 (zpay32) string.
	creationDate := time.Now()
	payReq, err := zpay32.NewInvoice(
		activeNetParams.Params,
		rHash,
		creationDate,
		options...,
	)
	if err != nil {
		return nil, err
	}

	payReqString, err := payReq.Encode(
		zpay32.MessageSigner{
			SignCompact: r.server.nodeSigner.SignDigestCompact,
		},
	)
	if err != nil {
		return nil, err
	}

	i := &channeldb.Invoice{
		CreationDate:   creationDate,
		Memo:           []byte(invoice.Memo),
		Receipt:        invoice.Receipt,
		PaymentRequest: []byte(payReqString),
		Terms: channeldb.ContractTerm{
			Value: amtMSat,
		},
	}
	copy(i.Terms.PaymentPreimage[:], paymentPreimage[:])

	rpcsLog.Tracef("[addinvoice] adding new invoice %v",
		newLogClosure(func() string {
			return spew.Sdump(i)
		}),
	)

	// With all sanity checks passed, write the invoice to the database.
	if err := r.server.invoices.AddInvoice(i); err != nil {
		return nil, err
	}

	return &lnrpc.AddInvoiceResponse{
		RHash:          rHash[:],
		PaymentRequest: payReqString,
	}, nil
}

// createRPCInvoice creates an *lnrpc.Invoice from the *channeldb.Invoice.
func createRPCInvoice(invoice *channeldb.Invoice) (*lnrpc.Invoice, error) {
	paymentRequest := string(invoice.PaymentRequest)
	decoded, err := zpay32.Decode(paymentRequest)
	if err != nil {
		return nil, fmt.Errorf("unable to decode payment request: %v",
			err)
	}

	descHash := []byte("")
	if decoded.DescriptionHash != nil {
		descHash = decoded.DescriptionHash[:]
	}

	fallbackAddr := ""
	if decoded.FallbackAddr != nil {
		fallbackAddr = decoded.FallbackAddr.String()
	}

	settleDate := int64(0)
	if !invoice.SettleDate.IsZero() {
		settleDate = invoice.SettleDate.Unix()
	}

	// Expiry time will default to 3600 seconds if not specified
	// explicitly.
	expiry := int64(decoded.Expiry().Seconds())

	// The expiry will default to 9 blocks if not specified explicitly.
	cltvExpiry := decoded.MinFinalCLTVExpiry()

	preimage := invoice.Terms.PaymentPreimage
	satAmt := invoice.Terms.Value.ToSatoshis()

	return &lnrpc.Invoice{
		Memo:            string(invoice.Memo[:]),
		Receipt:         invoice.Receipt[:],
		RHash:           decoded.PaymentHash[:],
		RPreimage:       preimage[:],
		Value:           int64(satAmt),
		CreationDate:    invoice.CreationDate.Unix(),
		SettleDate:      settleDate,
		Settled:         invoice.Terms.Settled,
		PaymentRequest:  paymentRequest,
		DescriptionHash: descHash,
		Expiry:          expiry,
		CltvExpiry:      cltvExpiry,
		FallbackAddr:    fallbackAddr,
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

	invoice, err := r.server.invoices.LookupInvoice(payHash)
	if err != nil {
		return nil, err
	}

	rpcsLog.Tracef("[lookupinvoice] located invoice %v",
		newLogClosure(func() string {
			return spew.Sdump(invoice)
		}))

	rpcInvoice, err := createRPCInvoice(&invoice)
	if err != nil {
		return nil, err
	}

	return rpcInvoice, nil
}

// ListInvoices returns a list of all the invoices currently stored within the
// database. Any active debug invoices are ignored.
func (r *rpcServer) ListInvoices(ctx context.Context,
	req *lnrpc.ListInvoiceRequest) (*lnrpc.ListInvoiceResponse, error) {

	dbInvoices, err := r.server.chanDB.FetchAllInvoices(req.PendingOnly)
	if err != nil {
		return nil, err
	}

	invoices := make([]*lnrpc.Invoice, len(dbInvoices))
	for i, dbInvoice := range dbInvoices {

		rpcInvoice, err := createRPCInvoice(dbInvoice)
		if err != nil {
			return nil, err
		}

		invoices[i] = rpcInvoice
	}

	return &lnrpc.ListInvoiceResponse{
		Invoices: invoices,
	}, nil
}

// SubscribeInvoices returns a uni-directional stream (server -> client) for
// notifying the client of newly added/settled invoices.
func (r *rpcServer) SubscribeInvoices(req *lnrpc.InvoiceSubscription,
	updateStream lnrpc.Lightning_SubscribeInvoicesServer) error {

	invoiceClient := r.server.invoices.SubscribeNotifications()
	defer invoiceClient.Cancel()

	for {
		select {
		// TODO(roasbeef): include newly added invoices?
		case settledInvoice := <-invoiceClient.SettledInvoices:

			rpcInvoice, err := createRPCInvoice(settledInvoice)
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

		txDetails.Transactions[i] = &lnrpc.Transaction{
			TxHash:           tx.Hash.String(),
			Amount:           int64(tx.Value),
			NumConfirmations: tx.NumConfirmations,
			BlockHash:        tx.BlockHash.String(),
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
	_ *lnrpc.ChannelGraphRequest) (*lnrpc.ChannelGraph, error) {

	resp := &lnrpc.ChannelGraph{}

	// Obtain the pointer to the global singleton channel graph, this will
	// provide a consistent view of the graph due to bolt db's
	// transactional model.
	graph := r.server.chanDB.ChannelGraph()

	// First iterate through all the known nodes (connected or unconnected
	// within the graph), collating their current state into the RPC
	// response.
	err := graph.ForEachNode(nil, func(_ *bolt.Tx, node *channeldb.LightningNode) error {
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
		}
	}

	if c2 != nil {
		edge.Node2Policy = &lnrpc.RoutingPolicy{
			TimeLockDelta:    uint32(c2.TimeLockDelta),
			MinHtlc:          int64(c2.MinHTLC),
			FeeBaseMsat:      int64(c2.FeeBaseMSat),
			FeeRateMilliMsat: int64(c2.FeeProportionalMillionths),
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
	if err := node.ForEachChannel(nil, func(_ *bolt.Tx, edge *channeldb.ChannelEdgeInfo,
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

	// Query the channel router for a possible path to the destination that
	// can carry `in.Amt` satoshis _including_ the total fee required on
	// the route.
	routes, err := r.server.chanRouter.FindRoutes(pubKey, amtMSat)
	if err != nil {
		return nil, err
	}

	// For each valid route, we'll convert the result into the format
	// required by the RPC system.
	routeResp := &lnrpc.QueryRoutesResponse{
		Routes: make([]*lnrpc.Route, len(routes)),
	}
	for i, route := range routes {
		routeResp.Routes[i] = marshallRoute(route)
	}

	return routeResp, nil
}

func marshallRoute(route *routing.Route) *lnrpc.Route {
	resp := &lnrpc.Route{
		TotalTimeLock: route.TotalTimeLock,
		TotalFees:     int64(route.TotalFees.ToSatoshis()),
		TotalAmt:      int64(route.TotalAmount.ToSatoshis()),
		Hops:          make([]*lnrpc.Hop, len(route.Hops)),
	}
	for i, hop := range route.Hops {
		resp.Hops[i] = &lnrpc.Hop{
			ChanId:       hop.Channel.ChannelID,
			ChanCapacity: int64(hop.Channel.Capacity),
			AmtToForward: int64(hop.AmtToForward.ToSatoshis()),
			Fee:          int64(hop.Fee.ToSatoshis()),
			Expiry:       uint32(hop.OutgoingTimeLock),
		}
	}

	return resp
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
	if err := graph.ForEachNode(nil, func(tx *bolt.Tx, node *channeldb.LightningNode) error {
		// Increment the total number of nodes with each iteration.
		numNodes++

		// For each channel we'll compute the out degree of each node,
		// and also update our running tallies of the min/max channel
		// capacity, as well as the total channel capacity. We pass
		// through the db transaction from the outer view so we can
		// re-use it within this inner view.
		var outDegree uint32
		if err := node.ForEachChannel(tx, func(_ *bolt.Tx,
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

	shutdownRequestChannel <- struct{}{}
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

		paymentHash := sha256.Sum256(payment.PaymentPreimage[:])
		paymentsResp.Payments[i] = &lnrpc.Payment{
			PaymentHash:     hex.EncodeToString(paymentHash[:]),
			Value:           int64(payment.Terms.Value.ToSatoshis()),
			CreationDate:    payment.CreationDate.Unix(),
			Path:            path,
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
	payReq, err := zpay32.Decode(req.PayReq)
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

	channelGraph := r.server.chanDB.ChannelGraph()
	selfNode, err := channelGraph.SourceNode()
	if err != nil {
		return nil, err
	}

	var feeReports []*lnrpc.ChannelFeeReport
	err = selfNode.ForEachChannel(nil, func(_ *bolt.Tx, chanInfo *channeldb.ChannelEdgeInfo,
		edgePolicy, _ *channeldb.ChannelEdgePolicy) error {

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

	return &lnrpc.FeeReportResponse{
		ChannelFees: feeReports,
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

	rpcsLog.Tracef("[updatechanpolicy] updating channel policy base_fee=%v, "+
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
