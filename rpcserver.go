package main

import (
	"encoding/hex"
	"fmt"
	"io"

	"sync"
	"sync/atomic"

	"github.com/lightningnetwork/lnd/lndc"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
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

// ConnectPeer attempts to establish a connection to a remote peer.
func (r *rpcServer) ConnectPeer(ctx context.Context,
	in *lnrpc.ConnectPeerRequest) (*lnrpc.ConnectPeerResponse, error) {

	if in.Addr == nil {
		return nil, fmt.Errorf("need: lnc pubkeyhash@hostname")
	}

	idAtHost := fmt.Sprintf("%v@%v", in.Addr.PubKeyHash, in.Addr.Host)
	rpcsLog.Debugf("[connectpeer] peer=%v", idAtHost)

	peerAddr, err := lndc.LnAddrFromString(idAtHost, activeNetParams.Params)
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
func (r *rpcServer) OpenChannel(in *lnrpc.OpenChannelRequest,
	updateStream lnrpc.Lightning_OpenChannelServer) error {

	rpcsLog.Tracef("[openchannel] request to peerid(%v) "+
		"allocation(us=%v, them=%v) numconfs=%v", in.TargetPeerId,
		in.LocalFundingAmount, in.RemoteFundingAmount, in.NumConfs)

	localFundingAmt := btcutil.Amount(in.LocalFundingAmount)
	remoteFundingAmt := btcutil.Amount(in.RemoteFundingAmount)
	target := in.TargetPeerId
	numConfs := in.NumConfs
	updateChan, errChan := r.server.OpenChannel(target, localFundingAmt,
		remoteFundingAmt, numConfs)

	var outpoint wire.OutPoint
out:
	for {
		select {
		case err := <-errChan:
			rpcsLog.Errorf("unable to open channel to peerid(%v): %v",
				target, err)
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

// CloseChannel attempts to close an active channel identified by its channel
// point. The actions of this method can additionally be augmented to attempt
// a force close after a timeout period in the case of an inactive peer.
func (r *rpcServer) CloseChannel(in *lnrpc.CloseChannelRequest,
	updateStream lnrpc.Lightning_CloseChannelServer) error {

	index := in.ChannelPoint.OutputIndex
	txid, err := wire.NewShaHash(in.ChannelPoint.FundingTxid)
	if err != nil {
		rpcsLog.Errorf("[closechannel] invalid txid: %v", err)
		return err
	}
	targetChannelPoint := wire.NewOutPoint(txid, index)

	rpcsLog.Tracef("[closechannel] request for ChannelPoint(%v)",
		targetChannelPoint)

	updateChan, errChan := r.server.htlcSwitch.CloseLink(targetChannelPoint)

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
				rpcsLog.Errorf("[closechannel] close completed: "+
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
	idAddr, err := btcutil.NewAddressPubKeyHash(btcutil.Hash160(idPub), activeNetParams.Params)
	if err != nil {
		return nil, err
	}

	return &lnrpc.GetInfoResponse{
		LightningId:        hex.EncodeToString(r.server.lightningID[:]),
		IdentityAddress:    idAddr.String(),
		NumPendingChannels: pendingChannels,
		NumActiveChannels:  activeChannels,
		NumPeers:           uint32(len(serverPeers)),
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

		lnID := hex.EncodeToString(serverPeer.lightningID[:])
		peer := &lnrpc.Peer{
			LightningId: lnID,
			PeerId:      serverPeer.id,
			Address:     serverPeer.conn.RemoteAddr().String(),
			Inbound:     serverPeer.inbound,
			BytesRecv:   atomic.LoadUint64(&serverPeer.bytesReceived),
			BytesSent:   atomic.LoadUint64(&serverPeer.bytesSent),
		}

		chanSnapshots := serverPeer.ChannelSnapshots()
		peer.Channels = make([]*lnrpc.ActiveChannel, 0, len(chanSnapshots))
		for _, chanSnapshot := range chanSnapshots {
			channel := &lnrpc.ActiveChannel{
				RemoteId:      lnID,
				ChannelPoint:  chanSnapshot.ChannelPoint.String(),
				Capacity:      int64(chanSnapshot.Capacity),
				LocalBalance:  int64(chanSnapshot.LocalBalance),
				RemoteBalance: int64(chanSnapshot.RemoteBalance),
				NumUpdates:    chanSnapshot.NumUpdates,
			}
			peer.Channels = append(peer.Channels, channel)
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

	return &lnrpc.WalletBalanceResponse{float64(balance)}, nil
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
			pendingChan := &lnrpc.PendingChannelResponse_PendingChannel{
				PeerId:        pendingOpen.peerId,
				LightningId:   hex.EncodeToString(pendingOpen.lightningID[:]),
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

// SendPayment dispatches a bi-directional streaming RPC for sending payments
// through the Lightning Network. A single RPC invocation creates a persistent
// bi-directional stream allowing clients to rapidly send payments through the
// Lightning Network with a single persistent connection.
func (r *rpcServer) SendPayment(paymentStream lnrpc.Lightning_SendPaymentServer) error {
	errChan := make(chan error, 1)
	for {
		select {
		case err := <-errChan:
			return err
		default:
			// Receive the next pending payment within the stream sent by
			// the client. If we read the EOF sentinel, then the client has
			// closed the stream, and we can exit normally.
			nextPayment, err := paymentStream.Recv()
			if err == io.EOF {
				return nil
			} else if err != nil {
				return err
			}

			// Craft an HTLC packet to send to the routing sub-system. The
			// meta-data within this packet will be used to route the
			// payment through the network.
			htlcAdd := &lnwire.HTLCAddRequest{
				Amount:           lnwire.CreditsAmount(nextPayment.Amt),
				RedemptionHashes: [][32]byte{debugHash},
			}
			destAddr, err := wire.NewShaHash(nextPayment.Dest)
			if err != nil {
				return err
			}
			htlcPkt := &htlcPacket{
				dest: *destAddr,
				msg:  htlcAdd,
			}

			// TODO(roasbeef): semaphore to limit num outstanding
			// goroutines.
			go func() {
				// Finally, send this next packet to the routing layer in order
				// to complete the next payment.
				// TODO(roasbeef): this should go through the L3 router once
				// multi-hop is in place.
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

func (r *rpcServer) ShowRoutingTable(ctx context.Context,
	in *lnrpc.ShowRoutingTableRequest) (*lnrpc.ShowRoutingTableResponse, error) {
	rpcsLog.Debugf("[ShowRoutingTable]")
	rtCopy := r.server.routingMgr.GetRTCopy()
	channels := make([]*lnrpc.RoutingTableLink, 0)
	for _, channel := range rtCopy.AllChannels() {
		channels = append(channels,
			&lnrpc.RoutingTableLink{
				Id1:       channel.Id1.String(),
				Id2:       channel.Id2.String(),
				Outpoint:  channel.EdgeID.String(),
				Capacity:  channel.Info.Capacity(),
				Weight:    channel.Info.Weight(),
			},
		)
	}
	return &lnrpc.ShowRoutingTableResponse{
		Channels: channels,
	}, nil
}
