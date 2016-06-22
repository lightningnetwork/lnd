package main

import (
	"encoding/hex"
	"fmt"

	"sync"
	"sync/atomic"

	"github.com/lightningnetwork/lnd/lndc"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/roasbeef/btcd/txscript"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
	"github.com/roasbeef/btcwallet/waddrmgr"
	"golang.org/x/net/context"
)

var (
	defaultAccount uint32 = waddrmgr.DefaultAccountNum
)

// rpcServer is a gRPC, RPC front end to the lnd daemon.
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

// newRpcServer creates and returns a new instance of the rpcServer.
func newRpcServer(s *server) *rpcServer {
	return &rpcServer{server: s, quit: make(chan struct{}, 1)}
}

// Start launches any helper goroutines required for the rpcServer
// to function.
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

// SendMany handles a request for a transaction create multiple specified
// outputs in parallel.
func (r *rpcServer) SendMany(ctx context.Context,
	in *lnrpc.SendManyRequest) (*lnrpc.SendManyResponse, error) {

	outputs := make([]*wire.TxOut, 0, len(in.AddrToAmount))
	for addr, amt := range in.AddrToAmount {
		addr, err := btcutil.DecodeAddress(addr, activeNetParams)
		if err != nil {
			return nil, err
		}

		pkscript, err := txscript.PayToAddrScript(addr)
		if err != nil {
			return nil, err
		}

		outputs = append(outputs, wire.NewTxOut(amt, pkscript))
	}

	// Instruct the wallet to create an transaction paying to the specified
	// outputs, selecting any coins with at least one confirmation.
	txid, err := r.server.lnwallet.SendOutputs(outputs, defaultAccount, 1)
	if err != nil {
		return nil, err
	}

	rpcsLog.Infof("Generated txid: %v", txid.String())

	return &lnrpc.SendManyResponse{Txid: txid.String()}, nil
}

// NewAddress creates a new address under control of the local wallet.
func (r *rpcServer) NewAddress(ctx context.Context,
	in *lnrpc.NewAddressRequest) (*lnrpc.NewAddressResponse, error) {

	r.server.lnwallet.KeyGenMtx.Lock()
	defer r.server.lnwallet.KeyGenMtx.Unlock()

	// Translate the gRPC proto address type to the wallet controller's
	// available address types.
	var addrType waddrmgr.AddressType
	switch in.Type {
	case lnrpc.NewAddressRequest_WITNESS_PUBKEY_HASH:
		addrType = waddrmgr.WitnessPubKey
	case lnrpc.NewAddressRequest_NESTED_PUBKEY_HASH:
		addrType = waddrmgr.NestedWitnessPubKey
	case lnrpc.NewAddressRequest_PUBKEY_HASH:
		addrType = waddrmgr.PubKeyHash
	}

	addr, err := r.server.lnwallet.NewAddress(defaultAccount,
		addrType)
	if err != nil {
		return nil, err
	}

	rpcsLog.Infof("Generated new address: %v", addr.String())
	return &lnrpc.NewAddressResponse{Address: addr.String()}, nil
}

// ConnectPeer attempts to establish a connection to a remote peer.
func (r *rpcServer) ConnectPeer(ctx context.Context,
	in *lnrpc.ConnectPeerRequest) (*lnrpc.ConnectPeerResponse, error) {

	if in.Addr == nil {
		return nil, fmt.Errorf("need: lnc pubkeyhash@hostname")
	}

	idAtHost := fmt.Sprintf("%v@%v", in.Addr.PubKeyHash, in.Addr.Host)
	rpcsLog.Debugf("Attempting to connect to peer %v", idAtHost)

	peerAddr, err := lndc.LnAddrFromString(idAtHost)
	if err != nil {
		rpcsLog.Errorf("(connectpeer): error parsing ln addr: %v", err)
		return nil, err
	}

	peerID, err := r.server.ConnectToPeer(peerAddr)
	if err != nil {
		rpcsLog.Errorf("(connectpeer): error connecting to peer: %v", err)
		return nil, err
	}

	rpcsLog.Debugf("Connected to peer: %v", peerAddr.String())
	return &lnrpc.ConnectPeerResponse{peerID}, nil
}

// OpenChannel attempts to open a singly funded channel specified in the
// request to a remote peer.
func (r *rpcServer) OpenChannel(ctx context.Context,
	in *lnrpc.OpenChannelRequest) (*lnrpc.OpenChannelResponse, error) {

	rpcsLog.Tracef("Recieved request to openchannel to peerid(%v) "+
		"allocation(us=%v, them=%v) numconfs=%v", in.TargetPeerId,
		in.LocalFundingAmount, in.RemoteFundingAmount, in.NumConfs)

	localFundingAmt := btcutil.Amount(in.LocalFundingAmount)
	remoteFundingAmt := btcutil.Amount(in.RemoteFundingAmount)
	target := in.TargetPeerId
	numConfs := in.NumConfs
	resp, err := r.server.OpenChannel(target, localFundingAmt,
		remoteFundingAmt, numConfs)
	if err != nil {
		rpcsLog.Errorf("unable to open channel to peerid(%v): %v",
			target, err)
		return nil, err
	}

	rpcsLog.Tracef("Opened channel with peerid(%v), ChannelPoint(%v)",
		in.TargetPeerId, resp)

	return &lnrpc.OpenChannelResponse{
		&lnrpc.ChannelPoint{
			FundingTxid: resp.Hash[:],
			OutputIndex: resp.Index,
		},
	}, nil
}

// CloseChannel attempts to close an active channel identified by its channel
// point. The actions of this method can additionally be augmented to attempt
// a force close after a timeout period in the case of an inactive peer.
func (r *rpcServer) CloseChannel(ctx context.Context,
	in *lnrpc.CloseChannelRequest) (*lnrpc.CloseChannelResponse, error) {

	index := in.ChannelPoint.OutputIndex
	txid, err := wire.NewShaHash(in.ChannelPoint.FundingTxid)
	if err != nil {
		rpcsLog.Errorf("(closechannel) invalid txid: %v", err)
		return nil, err
	}
	targetChannelPoint := wire.NewOutPoint(txid, index)

	rpcsLog.Tracef("Recieved closechannel request for ChannelPoint(%v)",
		targetChannelPoint)

	resp, err := r.server.CloseChannel(targetChannelPoint)
	if err != nil {
		rpcsLog.Errorf("Unable to close ChannelPoint(%v): %v",
			targetChannelPoint, err)
		return nil, err
	}

	return &lnrpc.CloseChannelResponse{resp}, nil
}

// ListPeers returns a verbose listing of all currently active peers.
func (r *rpcServer) ListPeers(ctx context.Context,
	in *lnrpc.ListPeersRequest) (*lnrpc.ListPeersResponse, error) {

	rpcsLog.Tracef("recieved listpeers request")

	serverPeers := r.server.Peers()
	resp := &lnrpc.ListPeersResponse{
		Peers: make([]*lnrpc.Peer, 0, len(serverPeers)),
	}

	for _, serverPeer := range serverPeers {
		// TODO(roasbeef): add a snapshot method which grabs peer read mtx
		peer := &lnrpc.Peer{
			LightningId: hex.EncodeToString(serverPeer.lightningID[:]),
			PeerId:      serverPeer.id,
			Address:     serverPeer.conn.RemoteAddr().String(),
			Inbound:     serverPeer.inbound,
			BytesRecv:   atomic.LoadUint64(&serverPeer.bytesReceived),
			BytesSent:   atomic.LoadUint64(&serverPeer.bytesSent),
		}

		resp.Peers = append(resp.Peers, peer)
	}

	rpcsLog.Tracef("listpeers yielded %v peers", serverPeers)

	return resp, nil
}

// WalletBalance returns the sum of all confirmed unspent outputs under control
// by the wallet. This method can be modified by having the request specify
// only witness outputs should be factored into the final output sum.
// TODO(roasbeef): split into total and confirmed/unconfirmed
func (r *rpcServer) WalletBalance(ctx context.Context,
	in *lnrpc.WalletBalanceRequest) (*lnrpc.WalletBalanceResponse, error) {

	var balance float64

	if in.WitnessOnly {
		witnessOutputs, err := r.server.lnwallet.ListUnspentWitness(1)
		if err != nil {
			return nil, err
		}

		// We need to convert from BTC to satoshi here otherwise, and
		// incorrect sum will be returned.
		var outputSum btcutil.Amount
		for _, witnessOutput := range witnessOutputs {
			outputSum += btcutil.Amount(witnessOutput.Amount * 1e8)
		}

		balance = outputSum.ToBTC()
	} else {
		// TODO(roasbeef): make num confs a param
		outputSum, err := r.server.lnwallet.CalculateBalance(1)
		if err != nil {
			return nil, err
		}

		balance = outputSum.ToBTC()
	}

	rpcsLog.Debugf("walletbalance query response: %v", balance)

	return &lnrpc.WalletBalanceResponse{balance}, nil
}
