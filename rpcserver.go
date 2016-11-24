package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"time"

	"sync"
	"sync/atomic"

	"github.com/btcsuite/fastsha256"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/rt/graph"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/chaincfg"
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
func (r *rpcServer) sendCoinsOnChain(paymentMap map[string]int64) (*wire.ShaHash, error) {
	outputs, err := addrPairsToOutputs(paymentMap)
	if err != nil {
		return nil, err
	}

	return r.server.lnwallet.SendOutputs(outputs)
}

// SendCoins executes a request to send coins to a particular address. Unlike
// SendMany, this RPC call only allows creating a single output at a time.
func (r *rpcServer) SendCoins(ctx context.Context,
	in *lnrpc.SendCoinsRequest) (*lnrpc.SendCoinsResponse, error) {

	rpcsLog.Infof("[sendcoins] addr=%v, amt=%v", in.Addr, btcutil.Amount(in.Amount))

	paymentMap := map[string]int64{in.Addr: in.Amount}
	txid, err := r.sendCoinsOnChain(paymentMap)
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

	txid, err := r.sendCoinsOnChain(in.AddrToAmount)
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

	addr, err := r.server.lnwallet.NewAddress(addrType, false)
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

	addr, err := r.server.lnwallet.NewAddress(lnwallet.WitnessPubKey, false)
	if err != nil {
		return nil, err
	}

	rpcsLog.Infof("[newaddress] addr=%v", addr.String())
	return &lnrpc.NewAddressResponse{Address: addr.String()}, nil
}

// ConnectPeer attempts to establish a connection to a remote peer.
// TODO(roasbeef): also return pubkey and/or identity hash?
func (r *rpcServer) ConnectPeer(ctx context.Context,
	in *lnrpc.ConnectPeerRequest) (*lnrpc.ConnectPeerResponse, error) {

	if in.Addr == nil {
		return nil, fmt.Errorf("need: lnc pubkeyhash@hostname")
	}

	pubkeyHex, err := hex.DecodeString(in.Addr.Pubkey)
	if err != nil {
		return nil, err
	}
	pubkey, err := btcec.ParsePubKey(pubkeyHex, btcec.S256())
	if err != nil {
		return nil, err
	}

	host, err := net.ResolveTCPAddr("tcp", in.Addr.Host)
	if err != nil {
		return nil, err
	}

	peerAddr := &lnwire.NetAddress{
		IdentityKey: pubkey,
		Address:     host,
		ChainNet:    activeNetParams.Net,
	}

	peerID, err := r.server.ConnectToPeer(peerAddr)
	if err != nil {
		rpcsLog.Errorf("(connectpeer): error connecting to peer: %v", err)
		return nil, err
	}

	// TODO(roasbeef): add pubkey return
	rpcsLog.Debugf("Connected to peer: %v", peerAddr.String())
	return &lnrpc.ConnectPeerResponse{peerID}, nil
}

// OpenChannel attempts to open a singly funded channel specified in the
// request to a remote peer.
func (r *rpcServer) OpenChannel(in *lnrpc.OpenChannelRequest,
	updateStream lnrpc.Lightning_OpenChannelServer) error {

	rpcsLog.Tracef("[openchannel] request to peerid(%v) "+
		"allocation(us=%v, them=%v) numconfs=%v", in.TargetPeerId,
		in.LocalFundingAmount, in.RemoteFundingAmount, in.NumConfs)

	localFundingAmt := btcutil.Amount(in.LocalFundingAmount)
	remoteFundingAmt := btcutil.Amount(in.RemoteFundingAmount)

	// TODO(roasbeef): make it optional
	nodepubKey, err := btcec.ParsePubKey(in.NodePubkey, btcec.S256())
	if err != nil {
		return err
	}

	// Instruct the server to trigger the necessary events to attempt to
	// open a new channel. A stream is returned in place, this stream will
	// be used to consume updates of the state of the pending channel.
	updateChan, errChan := r.server.OpenChannel(in.TargetPeerId,
		nodepubKey, localFundingAmt, remoteFundingAmt, in.NumConfs)

	var outpoint wire.OutPoint
out:
	for {
		select {
		case err := <-errChan:
			rpcsLog.Errorf("unable to open channel to "+
				"identityPub(%x) nor peerID(%v): %v",
				nodepubKey, in.TargetPeerId, err)
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
				h, _ := wire.NewShaHash(chanPoint.FundingTxid)
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
		"allocation(us=%v, them=%v) numconfs=%v", in.TargetPeerId,
		in.LocalFundingAmount, in.RemoteFundingAmount, in.NumConfs)

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
	remoteFundingAmt := btcutil.Amount(in.RemoteFundingAmount)

	updateChan, errChan := r.server.OpenChannel(in.TargetPeerId,
		nodepubKey, localFundingAmt, remoteFundingAmt, in.NumConfs)

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
			FundingTxid: chanUpdate.Txid,
		}, nil
	case <-r.quit:
		return nil, nil
	}
}

// CloseChannel attempts to close an active channel identified by its channel
// point. The actions of this method can additionally be augmented to attempt
// a force close after a timeout period in the case of an inactive peer.
func (r *rpcServer) CloseChannel(in *lnrpc.CloseChannelRequest,
	updateStream lnrpc.Lightning_CloseChannelServer) error {

	force := in.Force
	index := in.ChannelPoint.OutputIndex
	txid, err := wire.NewShaHash(in.ChannelPoint.FundingTxid)
	if err != nil {
		rpcsLog.Errorf("[closechannel] invalid txid: %v", err)
		return err
	}
	targetChannelPoint := wire.NewOutPoint(txid, index)

	rpcsLog.Tracef("[closechannel] request for ChannelPoint(%v)",
		targetChannelPoint)

	var closeType LinkCloseType
	switch force {
	case true:
		// TODO(roasbeef): should be able to force close w/o connection
		// to peer
		closeType = CloseForce
	case false:
		closeType = CloseRegular
	}

	updateChan, errChan := r.server.htlcSwitch.CloseLink(targetChannelPoint, closeType)

out:
	for {
		select {
		case err := <-errChan:
			rpcsLog.Errorf("[closechannel] unable to close "+
				"ChannelPoint(%v): %v", targetChannelPoint, err)
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
				h, _ := wire.NewShaHash(closeUpdate.ChanClose.ClosingTxid)
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

// GetInfo serves a request to the "getinfo" RPC call. This call returns
// general information concerning the lightning node including it's LN ID,
// identity address, and information concerning the number of open+pending
// channels.
func (r *rpcServer) GetInfo(ctx context.Context,
	in *lnrpc.GetInfoRequest) (*lnrpc.GetInfoResponse, error) {

	var activeChannels uint32
	serverPeers := r.server.Peers()
	for _, serverPeer := range serverPeers {
		activeChannels += uint32(len(serverPeer.ChannelSnapshots()))
	}

	pendingChannels := r.server.fundingMgr.NumPendingChannels()
	idPub := r.server.identityPriv.PubKey().SerializeCompressed()

	currentHeight, err := r.server.bio.GetCurrentHeight()
	if err != nil {
		return nil, err
	}

	isSynced, err := r.server.lnwallet.IsSynced()
	if err != nil {
		return nil, err
	}

	return &lnrpc.GetInfoResponse{
		IdentityPubkey:     hex.EncodeToString(idPub),
		NumPendingChannels: pendingChannels,
		NumActiveChannels:  activeChannels,
		NumPeers:           uint32(len(serverPeers)),
		BlockHeight:        uint32(currentHeight),
		SyncedToChain:      isSynced,
		Testnet:            activeNetParams.Params == &chaincfg.TestNet3Params,
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
		// TODO(roasbeef): add a snapshot method which grabs peer read mtx

		nodePub := serverPeer.addr.IdentityKey.SerializeCompressed()
		peer := &lnrpc.Peer{
			PubKey:    hex.EncodeToString(nodePub),
			PeerId:    serverPeer.id,
			Address:   serverPeer.conn.RemoteAddr().String(),
			Inbound:   serverPeer.inbound,
			BytesRecv: atomic.LoadUint64(&serverPeer.bytesReceived),
			BytesSent: atomic.LoadUint64(&serverPeer.bytesSent),
		}

		resp.Peers = append(resp.Peers, peer)
	}

	rpcsLog.Debugf("[listpeers] yielded %v peers", serverPeers)

	return resp, nil
}

// WalletBalance returns the sum of all confirmed unspent outputs under control
// by the wallet. This method can be modified by having the request specify
// only witness outputs should be factored into the final output sum.
// TODO(roasbeef): split into total and confirmed/unconfirmed
// TODO(roasbeef): add async hooks into wallet balance changes
func (r *rpcServer) WalletBalance(ctx context.Context,
	in *lnrpc.WalletBalanceRequest) (*lnrpc.WalletBalanceResponse, error) {

	balance, err := r.server.lnwallet.ConfirmedBalance(1, in.WitnessOnly)
	if err != nil {
		return nil, err
	}

	rpcsLog.Debugf("[walletbalance] balance=%v", balance)

	return &lnrpc.WalletBalanceResponse{balance.ToBTC()}, nil
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
		balance += channel.OurBalance
	}

	return &lnrpc.ChannelBalanceResponse{Balance: int64(balance)}, nil
}

// PendingChannels returns a list of all the channels that are currently
// considered "pending". A channel is pending if it has finished the funding
// workflow and is waiting for confirmations for the funding txn, or is in the
// process of closure, either initiated cooperatively or non-coopertively.
func (r *rpcServer) PendingChannels(ctx context.Context,
	in *lnrpc.PendingChannelRequest) (*lnrpc.PendingChannelResponse, error) {

	both := in.Status == lnrpc.ChannelStatus_ALL
	includeOpen := (in.Status == lnrpc.ChannelStatus_OPENING) || both
	includeClose := (in.Status == lnrpc.ChannelStatus_CLOSING) || both
	rpcsLog.Debugf("[pendingchannels] %v", in.Status)

	var pendingChannels []*lnrpc.PendingChannelResponse_PendingChannel
	if includeOpen {
		pendingOpenChans := r.server.fundingMgr.PendingChannels()
		for _, pendingOpen := range pendingOpenChans {
			// TODO(roasbeef): add confirmation progress
			pub := pendingOpen.identityPub.SerializeCompressed()
			pendingChan := &lnrpc.PendingChannelResponse_PendingChannel{
				PeerId:        pendingOpen.peerId,
				IdentityKey:   hex.EncodeToString(pub),
				ChannelPoint:  pendingOpen.channelPoint.String(),
				Capacity:      int64(pendingOpen.capacity),
				LocalBalance:  int64(pendingOpen.localBalance),
				RemoteBalance: int64(pendingOpen.remoteBalance),
				Status:        lnrpc.ChannelStatus_OPENING,
			}
			pendingChannels = append(pendingChannels, pendingChan)
		}
	}
	if includeClose {
	}

	return &lnrpc.PendingChannelResponse{
		PendingChannels: pendingChannels,
	}, nil
}

// ListChannels returns a description of all direct active, open channels the
// node knows of.
// TODO(roasbeef): add 'online' bit to response
func (r *rpcServer) ListChannels(ctx context.Context,
	in *lnrpc.ListChannelsRequest) (*lnrpc.ListChannelsResponse, error) {

	resp := &lnrpc.ListChannelsResponse{}

	dbChannels, err := r.server.chanDB.FetchAllChannels()
	if err != nil {
		return nil, err
	}

	rpcsLog.Infof("[listchannels] fetched %v channels from DB",
		len(dbChannels))

	for _, dbChannel := range dbChannels {
		nodePub := dbChannel.IdentityPub.SerializeCompressed()
		nodeID := hex.EncodeToString(nodePub)

		channel := &lnrpc.ActiveChannel{
			RemotePubkey:          nodeID,
			ChannelPoint:          dbChannel.ChanID.String(),
			Capacity:              int64(dbChannel.Capacity),
			LocalBalance:          int64(dbChannel.OurBalance),
			RemoteBalance:         int64(dbChannel.TheirBalance),
			TotalSatoshisSent:     int64(dbChannel.TotalSatoshisSent),
			TotalSatoshisReceived: int64(dbChannel.TotalSatoshisReceived),
			NumUpdates:            dbChannel.NumUpdates,
			PendingHtlcs:          make([]*lnrpc.HTLC, len(dbChannel.Htlcs)),
		}

		for i, htlc := range dbChannel.Htlcs {
			channel.PendingHtlcs[i] = &lnrpc.HTLC{
				Incoming:         htlc.Incoming,
				Amount:           int64(htlc.Amt),
				HashLock:         htlc.RHash[:],
				ExpirationHeight: htlc.RefundTimeout,
				RevocationDelay:  htlc.RevocationDelay,
			}
		}

		resp.Channels = append(resp.Channels, channel)
	}

	return resp, nil
}

// SendPayment dispatches a bi-directional streaming RPC for sending payments
// through the Lightning Network. A single RPC invocation creates a persistent
// bi-directional stream allowing clients to rapidly send payments through the
// Lightning Network with a single persistent connection.
func (r *rpcServer) SendPayment(paymentStream lnrpc.Lightning_SendPaymentServer) error {
	errChan := make(chan error, 1)
	payChan := make(chan *lnrpc.SendRequest)

	// Launch a new goroutine to handle reading new payment requests from
	// the client. This way we can handle errors independently of blocking
	// and waiting for the next payment request to come through.
	go func() {
		for {
			select {
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
					errChan <- err
					return
				}

				payChan <- nextPayment
			}
		}
	}()

	for {
		select {
		case err := <-errChan:
			return err
		case nextPayment := <-payChan:
			// Query the routing table for a potential path to the
			// destination node. If a path is ultimately
			// unavailable, then an error will be returned.
			destNode := nextPayment.Dest
			targetVertex := graph.NewVertex(destNode)
			path, err := r.server.routingMgr.FindPath(targetVertex)
			if err != nil {
				return err
			}
			rpcsLog.Tracef("[sendpayment] selected route: %v", path)
			// If we're in debug HTLC mode, then all outgoing
			// HTLC's will pay to the same debug rHash. Otherwise,
			// we pay to the rHash specified within the RPC
			// request.
			var rHash [32]byte
			if cfg.DebugHTLC && len(nextPayment.PaymentHash) == 0 {
				rHash = debugHash
			} else {
				copy(rHash[:], nextPayment.PaymentHash)
			}
			// Construct and HTLC packet which a payment route (if
			// one is found) to the destination using a Sphinx
			// onoin packet to encode the route.
			htlcPkt, err := r.constructPaymentRoute([]byte(nextPayment.Dest),
				nextPayment.Amt, rHash)
			if err != nil {
				return err
			}
			// We launch a new goroutine to execute the current
			// payment so we can continue to serve requests while
			// this payment is being dispatiched.
			//
			// TODO(roasbeef): semaphore to limit num outstanding
			// goroutines.
			go func() {
				// Finally, send this next packet to the
				// routing layer in order to complete the next
				// payment.
				if err := r.server.htlcSwitch.SendHTLC(htlcPkt); err != nil {
					errChan <- err
					return
				}

				// TODO(roasbeef): proper responses
				resp := &lnrpc.SendResponse{}
				if err := paymentStream.Send(resp); err != nil {
					errChan <- err
					return
				}
			}()
		}
	}

	return nil
}

// SendPaymentSync is the synchronous non-streaming version of SendPayment.
// This RPC is intended to be consumed by clients of the REST proxy.
// Additionally, this RPC expects the destination's public key and the payment
// hash (if any) to be encoded as hex strings.
func (r *rpcServer) SendPaymentSync(ctx context.Context,
	nextPayment *lnrpc.SendRequest) (*lnrpc.SendResponse, error) {

	// If we're in debug HTLC mode, then all outgoing HTLC's will pay to
	// the same debug rHash. Otherwise, we pay to the rHash specified
	// within the RPC request.
	var rHash [32]byte
	if cfg.DebugHTLC && nextPayment.PaymentHashString == "" {
		rHash = debugHash
	} else {
		paymentHash, err := hex.DecodeString(nextPayment.PaymentHashString)
		if err != nil {
			return nil, err
		}

		copy(rHash[:], paymentHash)
	}

	// Construct and HTLC packet which a payment route (if
	// one is found) to the destination using a Sphinx
	// onoin packet to encode the route.
	htlcPkt, err := r.constructPaymentRoute([]byte(nextPayment.DestString),
		nextPayment.Amt, rHash)
	if err != nil {
		return nil, err
	}

	// Finally, send this next packet to the routing layer in order to
	// complete the next payment.
	if err := r.server.htlcSwitch.SendHTLC(htlcPkt); err != nil {
		return nil, err
	}

	return &lnrpc.SendResponse{}, nil
}

// constructPaymentRoute attempts to construct a complete HTLC packet which
// encapsulates a Sphinx onion packet that encodes the end-to-end route any
// payment instructions necessary to complete an HTLC. If a route is unable to
// be located, then an error is returned indicating as much.
func (r *rpcServer) constructPaymentRoute(destPubkey []byte, amt int64,
	rHash [32]byte) (*htlcPacket, error) {

	const queryTimeout = time.Duration(time.Second * 10)

	// Query the routing table for a potential path to the destination
	// node. If a path is ultimately unavailable, then an error will be
	// returned.
	targetVertex := graph.NewVertex(destPubkey)
	path, err := r.server.routingMgr.FindPath(targetVertex)
	if err != nil {
		return nil, err
	}
	rpcsLog.Tracef("[sendpayment] selected route: %v", path)

	// Generate the raw encoded sphinx packet to be included along with the
	// HTLC add message.  We snip off the first hop from the path as within
	// the routing table's star graph, we're always the first hop.
	sphinxPacket, err := generateSphinxPacket(path[1:], rHash[:])
	if err != nil {
		return nil, err
	}

	// Craft an HTLC packet to send to the routing sub-system. The
	// meta-data within this packet will be used to route the payment
	// through the network.
	htlcAdd := &lnwire.HTLCAddRequest{
		Amount:           btcutil.Amount(amt),
		RedemptionHashes: [][32]byte{rHash},
		OnionBlob:        sphinxPacket,
	}

	firstHopPub := path[1].ToByte()
	destInterface := wire.ShaHash(fastsha256.Sum256(firstHopPub))

	return &htlcPacket{
		dest: destInterface,
		msg:  htlcAdd,
	}, nil
}

// generateSphinxPacket generates then encodes a sphinx packet which encodes
// the onion route specified by the passed list of graph vertexes. The blob
// returned from this function can immediately be included within an HTLC add
// packet to be sent to the first hop within the route.
func generateSphinxPacket(vertexes []graph.Vertex, paymentHash []byte) ([]byte, error) {
	// First convert all the vertexs from the routing table to in-memory
	// public key objects. These objects are necessary in order to perform
	// the series of ECDH operations required to construct the Sphinx
	// packet below.
	route := make([]*btcec.PublicKey, len(vertexes))
	for i, vertex := range vertexes {
		vertexBytes := vertex.ToByte()

		pub, err := btcec.ParsePubKey(vertexBytes, btcec.S256())
		if err != nil {
			return nil, err
		}

		route[i] = pub
	}

	// Next we generate the per-hop payload which gives each node within
	// the route the necessary information (fees, CLTV value, etc) to
	// properly forward the payment.
	// TODO(roasbeef): properly set CLTV value, payment amount, and chain
	// within hop paylods.
	var hopPayloads [][]byte
	for i := 0; i < len(route); i++ {
		payload := bytes.Repeat([]byte{byte('A' + i)},
			sphinx.HopPayloadSize)
		hopPayloads = append(hopPayloads, payload)
	}

	sessionKey, err := btcec.NewPrivateKey(btcec.S256())
	if err != nil {
		return nil, err
	}

	// Next generate the onion routing packet which allows
	// us to perform privacy preserving source routing
	// across the network.
	sphinxPacket, err := sphinx.NewOnionPacket(route, sessionKey,
		hopPayloads, paymentHash)
	if err != nil {
		return nil, err
	}

	// Finally, encode Sphinx packet using it's wire represenation to be
	// included within the HTLC add packet.
	var onionBlob bytes.Buffer
	if err := sphinxPacket.Encode(&onionBlob); err != nil {
		return nil, err
	}

	rpcsLog.Tracef("[sendpayment] generated sphinx packet: %v",
		newLogClosure(func() string {
			// We unset the internal curve here in order to keep
			// the logs from getting noisy.
			sphinxPacket.Header.EphemeralKey.Curve = nil
			return spew.Sdump(sphinxPacket)
		}))

	return onionBlob.Bytes(), nil
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

	// The size of the memo and receipt attached must not exceed the
	// maximum values for either of the fields.
	if len(invoice.Memo) > channeldb.MaxMemoSize {
		return nil, fmt.Errorf("memo too large: %v bytes "+
			"(maxsize=%v)", len(invoice.Memo), channeldb.MaxMemoSize)
	}
	if len(invoice.Receipt) > channeldb.MaxReceiptSize {
		return nil, fmt.Errorf("receipt too large: %v bytes "+
			"(maxsize=%v)", len(invoice.Receipt), channeldb.MaxReceiptSize)
	}

	i := &channeldb.Invoice{
		CreationDate: time.Now(),
		Memo:         []byte(invoice.Memo),
		Receipt:      invoice.Receipt,
		Terms: channeldb.ContractTerm{
			Value: btcutil.Amount(invoice.Value),
		},
	}
	copy(i.Terms.PaymentPreimage[:], paymentPreimage[:])

	rpcsLog.Tracef("[addinvoice] adding new invoice %v",
		newLogClosure(func() string {
			return spew.Sdump(i)
		}))

	// With all sanity checks passed, write the invoice to the database.
	if err := r.server.invoices.AddInvoice(i); err != nil {
		return nil, err
	}

	// Finally generate the payment hash itself from the pre-image. This
	// will be used by clients to query for the state of a particular
	// invoice.
	rHash := fastsha256.Sum256(paymentPreimage[:])

	return &lnrpc.AddInvoiceResponse{
		RHash: rHash[:],
	}, nil
}

// LookupInvoice attemps to look up an invoice according to its payment hash.
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

	return &lnrpc.Invoice{
		Memo:      string(invoice.Memo[:]),
		Receipt:   invoice.Receipt[:],
		RPreimage: invoice.Terms.PaymentPreimage[:],
		Value:     int64(invoice.Terms.Value),
		Settled:   invoice.Terms.Settled,
	}, nil
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
		invoice := &lnrpc.Invoice{
			Memo:         string(dbInvoice.Memo[:]),
			Receipt:      dbInvoice.Receipt[:],
			RPreimage:    dbInvoice.Terms.PaymentPreimage[:],
			Value:        int64(dbInvoice.Terms.Value),
			Settled:      dbInvoice.Terms.Settled,
			CreationDate: dbInvoice.CreationDate.Unix(),
		}

		invoices[i] = invoice
	}

	return &lnrpc.ListInvoiceResponse{
		Invoices: invoices,
	}, nil
}

// SubscribeInvoices returns a uni-directional stream (sever -> client) for
// notifying the client of newly added/settled invoices.
func (r *rpcServer) SubscribeInvoices(req *lnrpc.InvoiceSubscription,
	updateStream lnrpc.Lightning_SubscribeInvoicesServer) error {

	invoiceClient := r.server.invoices.SubscribeNotifications()
	defer invoiceClient.Cancel()

	for {
		select {
		// TODO(roasbeef): include newly added invoices?
		case settledInvoice := <-invoiceClient.SettledInvoices:
			invoice := &lnrpc.Invoice{
				Memo:      string(settledInvoice.Memo[:]),
				Receipt:   settledInvoice.Receipt[:],
				RPreimage: settledInvoice.Terms.PaymentPreimage[:],
				Value:     int64(settledInvoice.Terms.Value),
				Settled:   settledInvoice.Terms.Settled,
			}
			if err := updateStream.Send(invoice); err != nil {
				return err
			}
		case <-r.quit:
			return nil
		}
	}

	return nil
}

// SubscribeTransactions creates a uni-directional stream (server -> client) in
// which any newly discovered transactions relevant to the wallet are sent
// over.
func (r *rpcServer) SubscribeTransactions(req *lnrpc.GetTransactionsRequest,
	updateStream lnrpc.Lightning_SubscribeTransactionsServer) error {

	txClient, err := r.server.lnwallet.SubscribeTransactions()
	if err != nil {
		return err
	}
	defer txClient.Cancel()

	for {
		select {
		case tx := <-txClient.ConfirmedTransactions():
			detail := &lnrpc.Transaction{
				TxHash:           tx.Hash.String(),
				Amount:           tx.Value.ToBTC(),
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
				Amount:    tx.Value.ToBTC(),
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

	return nil
}

// GetTransactions returns a list of describing all the known transactions
// relevant to the wallet.
func (r *rpcServer) GetTransactions(context.Context,
	*lnrpc.GetTransactionsRequest) (*lnrpc.TransactionDetails, error) {

	// TODO(roasbeef): add pagination support
	transactions, err := r.server.lnwallet.ListTransactionDetails()
	if err != nil {
		return nil, err
	}

	txDetails := &lnrpc.TransactionDetails{
		Transactions: make([]*lnrpc.Transaction, len(transactions)),
	}
	for i, tx := range transactions {
		txDetails.Transactions[i] = &lnrpc.Transaction{
			TxHash:           tx.Hash.String(),
			Amount:           tx.Value.ToBTC(),
			NumConfirmations: tx.NumConfirmations,
			BlockHash:        tx.BlockHash.String(),
			TimeStamp:        tx.Timestamp,
			TotalFees:        tx.TotalFees,
		}
	}

	return txDetails, nil
}

// ShowRoutingTable returns a table-formatted dump of the known routing
// topology from the PoV of the source node.
func (r *rpcServer) ShowRoutingTable(ctx context.Context,
	in *lnrpc.ShowRoutingTableRequest) (*lnrpc.ShowRoutingTableResponse, error) {

	rpcsLog.Debugf("[ShowRoutingTable]")

	rtCopy := r.server.routingMgr.GetRTCopy()

	var channels []*lnrpc.RoutingTableLink
	for _, channel := range rtCopy.AllChannels() {
		channels = append(channels,
			&lnrpc.RoutingTableLink{
				Id1:      hex.EncodeToString(channel.Src.ToByte()),
				Id2:      hex.EncodeToString(channel.Tgt.ToByte()),
				Outpoint: channel.Id.String(),
				Capacity: channel.Info.Cpt,
				Weight:   channel.Info.Wgt,
			},
		)
	}

	return &lnrpc.ShowRoutingTableResponse{
		Channels: channels,
	}, nil
}
