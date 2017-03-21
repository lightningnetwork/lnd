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
	"strings"
	"time"

	"sync"
	"sync/atomic"

	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/lightningnetwork/lnd/zpay32"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/chaincfg"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
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

// newRPCServer creates and returns a new instance of the rpcServer.
func newRPCServer(s *server) *rpcServer {
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
func (r *rpcServer) sendCoinsOnChain(paymentMap map[string]int64) (*chainhash.Hash, error) {
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

	if err := r.server.ConnectToPeer(peerAddr, in.Perm); err != nil {
		rpcsLog.Errorf("(connectpeer): error connecting to peer: %v", err)
		return nil, err
	}

	rpcsLog.Debugf("Connected to peer: %v", peerAddr.String())
	return &lnrpc.ConnectPeerResponse{}, nil
}

// OpenChannel attempts to open a singly funded channel specified in the
// request to a remote peer.
func (r *rpcServer) OpenChannel(in *lnrpc.OpenChannelRequest,
	updateStream lnrpc.Lightning_OpenChannelServer) error {

	rpcsLog.Tracef("[openchannel] request to peerid(%v) "+
		"allocation(us=%v, them=%v) numconfs=%v", in.TargetPeerId,
		in.LocalFundingAmount, in.PushSat, in.NumConfs)

	localFundingAmt := btcutil.Amount(in.LocalFundingAmount)
	remoteInitialBalance := btcutil.Amount(in.PushSat)

	// Ensure that the initial balance of the remote party (if pushing
	// satoshis) does not execeed the amount the local party has requested
	// for funding.
	if remoteInitialBalance >= localFundingAmt {
		return fmt.Errorf("amount pushed to remote peer for initial " +
			"state must be below the local funding amount")
	}

	const minChannelSize = btcutil.Amount(6000)

	// Restrict the size of the channel we'll actually open. Atm, we
	// require the amount to be above 6k satoahis s we currently hard-coded
	// a 5k satoshi fee in several areas. As a result 6k sat is the min
	// channnel size that allows us to safely sit above the dust threshold
	// after fees are applied
	// TODO(roasbeef): remove after dynamic fees are in
	if localFundingAmt < minChannelSize {
		return fmt.Errorf("channel is too small, the minimum channel "+
			"size is: %v (6k sat)", minChannelSize)
	}

	var (
		nodepubKey      *btcec.PublicKey
		nodepubKeyBytes []byte
		err             error
	)

	// TODO(roasbeef): also return channel ID?

	// If the node key is set, the we'll parse the raw bytes into a pubkey
	// object so we can easily manipulate it. If this isn't set, then we
	// expected the TargetPeerId to be set accordingly.
	if len(in.NodePubkey) != 0 {
		nodepubKey, err = btcec.ParsePubKey(in.NodePubkey, btcec.S256())
		if err != nil {
			return err
		}
		nodepubKeyBytes = nodepubKey.SerializeCompressed()
	}

	// Instruct the server to trigger the necessary events to attempt to
	// open a new channel. A stream is returned in place, this stream will
	// be used to consume updates of the state of the pending channel.
	updateChan, errChan := r.server.OpenChannel(in.TargetPeerId,
		nodepubKey, localFundingAmt, remoteInitialBalance, in.NumConfs)

	var outpoint wire.OutPoint
out:
	for {
		select {
		case err := <-errChan:
			rpcsLog.Errorf("unable to open channel to "+
				"identityPub(%x) nor peerID(%v): %v",
				nodepubKeyBytes, in.TargetPeerId, err)
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
				h, _ := chainhash.NewHash(chanPoint.FundingTxid)
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
		in.LocalFundingAmount, in.PushSat, in.NumConfs)

	// Creation of channels before the wallet syncs up is currently
	// disallowed.
	isSynced, err := r.server.lnwallet.IsSynced()
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

	// Ensure that the initial balance of the remote party (if pushing
	// satoshis) does not execeed the amount the local party has requested
	// for funding.
	if remoteInitialBalance >= localFundingAmt {
		return nil, fmt.Errorf("amount pushed to remote peer for " +
			"initial state must be below the local funding amount")
	}

	updateChan, errChan := r.server.OpenChannel(in.TargetPeerId,
		nodepubKey, localFundingAmt, remoteInitialBalance, in.NumConfs)

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
	txid, err := chainhash.NewHash(in.ChannelPoint.FundingTxid)
	if err != nil {
		rpcsLog.Errorf("[closechannel] invalid txid: %v", err)
		return err
	}
	chanPoint := wire.NewOutPoint(txid, index)

	rpcsLog.Tracef("[closechannel] request for ChannelPoint(%v)",
		chanPoint)

	var (
		updateChan chan *lnrpc.CloseStatusUpdate
		errChan    chan error
	)

	// If a force closure was requested, then we'll handle all the details
	// around the creation and broadcast of the unilateral closure
	// transaction here rather than going to the switch as we don't require
	// interaction from the peer.
	if force {
		// As the first part of the force closure, we first fetch the
		// channel from the database, then execute a direct force
		// closure broadcasting our current commitment transaction.
		// TODO(roasbeef): d/c peer if connected?
		//  * otherwise safety no guaranteed
		channel, err := r.fetchActiveChannel(*chanPoint)
		if err != nil {
			return err
		}
		closingTxid, err := r.forceCloseChan(channel)
		if err != nil {
			rpcsLog.Errorf("unable to force close transaction: %v", err)

			// If the transaction we broadcast is detected as a
			// double spend, the this indicates that the remote
			// party has broadcast their commitment transaction be
			// we didn't notice.
			if strings.Contains(err.Error(), "fully-spent") ||
				strings.Contains(err.Error(), "double spend") {

				// In this case, we'll clean up the channel
				// state.
				// TODO(roasbeef): check close summary to see
				// if we need to sweep any HTLC's
				if err := channel.DeleteState(); err != nil {
					return err
				}
				return fmt.Errorf("channel has been closed by remote party")
			}

			return err
		}

		updateChan = make(chan *lnrpc.CloseStatusUpdate)
		errChan = make(chan error)
		go func() {
			// With the transaction broadcast, we send our first
			// update to the client.
			updateChan <- &lnrpc.CloseStatusUpdate{
				Update: &lnrpc.CloseStatusUpdate_ClosePending{
					ClosePending: &lnrpc.PendingUpdate{
						Txid: closingTxid[:],
					},
				},
			}

			// Next, we enter the second phase, waiting for the
			// channel to be confirmed before we finalize the force
			// closure.
			notifier := r.server.chainNotifier
			confNtfn, err := notifier.RegisterConfirmationsNtfn(closingTxid, 1)
			if err != nil {
				errChan <- err
				return
			}

			select {
			case txConf, ok := <-confNtfn.Confirmed:
				if !ok {
					return
				}

				// As the channel has been closed, we can now
				// delete it's state from the database.
				rpcsLog.Infof("ChannelPoint(%v) is now "+
					"closed at height %v", chanPoint,
					txConf.BlockHeight)
				if err := channel.DeleteState(); err != nil {
					errChan <- err
					return
				}
			case <-r.quit:
				return
			}

			// Respond to the local subsystem which requested the
			// channel closure.
			updateChan <- &lnrpc.CloseStatusUpdate{
				Update: &lnrpc.CloseStatusUpdate_ChanClose{
					ChanClose: &lnrpc.ChannelCloseUpdate{
						ClosingTxid: closingTxid[:],
						Success:     true,
					},
				},
			}

			// Finally, signal to the breachArbiter that it no
			// longer needs to watch the channel as it's been
			// closed.
			r.server.breachArbiter.settledContracts <- chanPoint
		}()

	} else {
		// Otherwise, the caller has requested a regular interactive
		// cooperative channel closure. So we'll forward the request to
		// the htlc switch which will handle the negotiation and
		// broadcast details.
		updateChan, errChan = r.server.htlcSwitch.CloseLink(chanPoint,
			CloseRegular)
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

// fetchActiveChannel attempts to locate a channel identified by it's channel
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
		if *dbChannel.ChanID == chanPoint {
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
	return lnwallet.NewLightningChannel(r.server.lnwallet.Signer, nil,
		dbChan)
}

// forceCloseChan executes a unilateral close of the target channel by
// broadcasting the current commitment state directly on-chain. Once the
// commitment transaction has been broadcast, a struct describing the final
// state of the channel is sent to the utxoNursery in order to ultimately sweep
// the immature outputs.
func (r *rpcServer) forceCloseChan(channel *lnwallet.LightningChannel) (*chainhash.Hash, error) {
	// Execute a unilateral close shutting down all further channel
	// operation.
	closeSummary, err := channel.ForceClose()
	if err != nil {
		return nil, err
	}

	closeTx := closeSummary.CloseTx
	txid := closeTx.TxHash()

	// With the close transaction in hand, broadcast the transaction to the
	// network, thereby entering the psot channel resolution state.
	rpcsLog.Infof("Broadcasting force close transaction, ChannelPoint(%v): %v",
		channel.ChannelPoint(), newLogClosure(func() string {
			return spew.Sdump(closeTx)
		}))
	if err := r.server.lnwallet.PublishTransaction(closeTx); err != nil {
		return nil, err
	}

	// Send the closed channel summary over to the utxoNursery in order to
	// have its outputs swept back into the wallet once they're mature.
	r.server.utxoNursery.incubateOutputs(closeSummary)

	return &txid, nil
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

	pendingChannels, err := r.server.fundingMgr.NumPendingChannels()
	if err != nil {
		return nil, err
	}

	idPub := r.server.identityPriv.PubKey().SerializeCompressed()

	bestHash, bestHeight, err := r.server.bio.GetBestBlock()
	if err != nil {
		return nil, err
	}

	isSynced, err := r.server.lnwallet.IsSynced()
	if err != nil {
		return nil, err
	}

	// TODO(roasbeef): add synced height n stuff
	return &lnrpc.GetInfoResponse{
		IdentityPubkey:     hex.EncodeToString(idPub),
		NumPendingChannels: pendingChannels,
		NumActiveChannels:  activeChannels,
		NumPeers:           uint32(len(serverPeers)),
		BlockHeight:        uint32(bestHeight),
		BlockHash:          bestHash.String(),
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

		var (
			satSent int64
			satRecv int64
		)

		// In order to display the total number of satoshis of outbound
		// (sent) and inbound (recv'd) satoshis that have been
		// transported through this peer, we'll sum up the sent/recv'd
		// values for each of the active channels we ahve with the
		// peer.
		chans := serverPeer.ChannelSnapshots()
		for _, c := range chans {
			satSent += int64(c.TotalSatoshisSent)
			satRecv += int64(c.TotalSatoshisReceived)
		}

		nodePub := serverPeer.addr.IdentityKey.SerializeCompressed()
		peer := &lnrpc.Peer{
			PubKey:    hex.EncodeToString(nodePub),
			PeerId:    serverPeer.id,
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

	return &lnrpc.WalletBalanceResponse{
		Balance: balance.ToBTC(),
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
		pendingOpenChans, err := r.server.fundingMgr.PendingChannels()
		if err != nil {
			return nil, err
		}
		for _, pendingOpen := range pendingOpenChans {
			// TODO(roasbeef): add confirmation progress
			pub := pendingOpen.identityPub.SerializeCompressed()
			pendingChan := &lnrpc.PendingChannelResponse_PendingChannel{
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
		chanPoint := dbChannel.ChanID

		// With the channel point known, retrieve the network channel
		// ID from the database.
		var chanID uint64
		chanID, _ = graph.ChannelID(chanPoint)

		var peerOnline bool
		if _, err := r.server.findPeer(nodePub); err == nil {
			peerOnline = true
		}

		channel := &lnrpc.ActiveChannel{
			Active:                peerOnline,
			RemotePubkey:          nodeID,
			ChannelPoint:          chanPoint.String(),
			ChanId:                chanID,
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

// savePayment saves a successfully completed payment to the database for
// historical record keeping.
func (r *rpcServer) savePayment(route *routing.Route, amount btcutil.Amount,
	rHash []byte) error {

	paymentPath := make([][33]byte, len(route.Hops))
	for i, hop := range route.Hops {
		hopPub := hop.Channel.Node.PubKey.SerializeCompressed()
		copy(paymentPath[i][:], hopPub)
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
	copy(payment.PaymentHash[:], rHash)

	return r.server.chanDB.AddPayment(payment)
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

				// If the payment request field isn't blank,
				// then the details of the invoice are encoded
				// entirely within the encode payReq. So we'll
				// attempt to decode it, populating the
				// nextPayment accordingly.
				if nextPayment.PaymentRequest != "" {
					payReq, err := zpay32.Decode(nextPayment.PaymentRequest)
					if err != nil {
						errChan <- err
						return
					}

					// TODO(roasbeef): eliminate necessary
					// encode/decode
					nextPayment.Dest = payReq.Destination.SerializeCompressed()
					nextPayment.Amt = int64(payReq.Amount)
					nextPayment.PaymentHash = payReq.PaymentHash[:]
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
			// Parse the details of the payment which include the
			// pubkey of the destination and the payment amount.
			dest := nextPayment.Dest
			amt := btcutil.Amount(nextPayment.Amt)
			destNode, err := btcec.ParsePubKey(dest, btcec.S256())
			if err != nil {
				return err

			}

			// If we're in debug HTLC mode, then all outgoing HTLCs
			// will pay to the same debug rHash. Otherwise, we pay
			// to the rHash specified within the RPC request.
			var rHash [32]byte
			if cfg.DebugHTLC && len(nextPayment.PaymentHash) == 0 {
				rHash = debugHash
			} else {
				copy(rHash[:], nextPayment.PaymentHash)
			}

			// We launch a new goroutine to execute the current
			// payment so we can continue to serve requests while
			// this payment is being dispatched.
			//
			// TODO(roasbeef): semaphore to limit num outstanding
			// goroutines.
			go func() {
				// Construct a payment request to send to the
				// channel router. If the payment is
				// successful, the the route chosen will be
				// returned. Otherwise, we'll get a non-nil
				// error.
				payment := &routing.LightningPayment{
					Target:      destNode,
					Amount:      amt,
					PaymentHash: rHash,
				}
				preImage, route, err := r.server.chanRouter.SendPayment(payment)
				if err != nil {
					errChan <- err
					return
				}

				// Save the completed payment to the database
				// for record keeping purposes.
				if err := r.savePayment(route, amt, rHash[:]); err != nil {
					errChan <- err
					return
				}

				err = paymentStream.Send(&lnrpc.SendResponse{
					PaymentPreimage: preImage[:],
					PaymentRoute:    marshalRoute(route),
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

	var (
		destPub *btcec.PublicKey
		amt     btcutil.Amount
		rHash   [32]byte
	)

	// If the proto request has an encoded payment request, then we we'll
	// use that solely to dipatch the payment.
	if nextPayment.PaymentRequest != "" {
		payReq, err := zpay32.Decode(nextPayment.PaymentRequest)
		if err != nil {
			return nil, err
		}
		destPub = payReq.Destination
		amt = payReq.Amount
		rHash = payReq.PaymentHash

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

		amt = btcutil.Amount(nextPayment.Amt)
	}

	// Finally, send a payment request to the channel router. If the
	// payment succeeds, then the returned route will be that was used
	// successfully within the payment.
	preImage, route, err := r.server.chanRouter.SendPayment(&routing.LightningPayment{
		Target:      destPub,
		Amount:      amt,
		PaymentHash: rHash,
	})
	if err != nil {
		return nil, err
	}

	// With the payment completed successfully, we now ave the details of
	// the completed payment to the databse for historical record keeping.
	if err := r.savePayment(route, amt, rHash[:]); err != nil {
		return nil, err
	}

	return &lnrpc.SendResponse{
		PaymentPreimage: preImage[:],
		PaymentRoute:    marshalRoute(route),
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

	// Finally, the value of an invoice MUST NOT be zero.
	if invoice.Value == 0 {
		return nil, fmt.Errorf("zero value invoices are disallowed")
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

	// Next, generate the payment hash itself from the preimage. This will
	// be used by clients to query for the state of a particular invoice.
	rHash := sha256.Sum256(paymentPreimage[:])

	// Finally we also create an encoded payment request which allows the
	// caller to comactly send the invoice to the payer.
	payReqString := zpay32.Encode(&zpay32.PaymentRequest{
		Destination: r.server.identityPriv.PubKey(),
		PaymentHash: rHash,
		Amount:      btcutil.Amount(invoice.Value),
	})

	return &lnrpc.AddInvoiceResponse{
		RHash:          rHash[:],
		PaymentRequest: payReqString,
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

	preimage := invoice.Terms.PaymentPreimage
	return &lnrpc.Invoice{
		Memo:         string(invoice.Memo[:]),
		Receipt:      invoice.Receipt[:],
		RPreimage:    preimage[:],
		Value:        int64(invoice.Terms.Value),
		CreationDate: invoice.CreationDate.Unix(),
		Settled:      invoice.Terms.Settled,
		PaymentRequest: zpay32.Encode(&zpay32.PaymentRequest{
			Destination: r.server.identityPriv.PubKey(),
			PaymentHash: sha256.Sum256(preimage[:]),
			Amount:      invoice.Terms.Value,
		}),
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
		invoiceAmount := dbInvoice.Terms.Value
		paymentPreimge := dbInvoice.Terms.PaymentPreimage[:]
		invoice := &lnrpc.Invoice{
			Memo:         string(dbInvoice.Memo[:]),
			Receipt:      dbInvoice.Receipt[:],
			RPreimage:    paymentPreimge,
			Value:        int64(invoiceAmount),
			Settled:      dbInvoice.Terms.Settled,
			CreationDate: dbInvoice.CreationDate.Unix(),
			PaymentRequest: zpay32.Encode(&zpay32.PaymentRequest{
				Destination: r.server.identityPriv.PubKey(),
				PaymentHash: sha256.Sum256(paymentPreimge),
				Amount:      invoiceAmount,
			}),
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
			Amount:           int64(tx.Value),
			NumConfirmations: tx.NumConfirmations,
			BlockHash:        tx.BlockHash.String(),
			BlockHeight:      tx.BlockHeight,
			TimeStamp:        tx.Timestamp,
			TotalFees:        tx.TotalFees,
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
func (r *rpcServer) DescribeGraph(context.Context,
	*lnrpc.ChannelGraphRequest) (*lnrpc.ChannelGraph, error) {

	resp := &lnrpc.ChannelGraph{}

	// Obtain the pinter to the global singleton channel graph, this will
	// provide a consistent view of the graph due to bolt db's
	// transactional model.
	graph := r.server.chanDB.ChannelGraph()

	// First iterate through all the known nodes (connected or unconnected
	// within the graph), collating their current state into the RPC
	// response.
	err := graph.ForEachNode(func(node *channeldb.LightningNode) error {
		resp.Nodes = append(resp.Nodes, &lnrpc.LightningNode{
			LastUpdate: uint32(node.LastUpdate.Unix()),
			PubKey:     hex.EncodeToString(node.PubKey.SerializeCompressed()),
			Address:    node.Address.String(),
			Alias:      node.Alias,
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
		Node1Pub:   hex.EncodeToString(edgeInfo.NodeKey1.SerializeCompressed()),
		Node2Pub:   hex.EncodeToString(edgeInfo.NodeKey2.SerializeCompressed()),
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

// GetChainInfo returns the latest authenticated network announcement for the
// given channel identified by its channel ID: an 8-byte integer which uniquely
// identifies the location of transaction's funding output within the block
// chain.
func (r *rpcServer) GetChanInfo(_ context.Context, in *lnrpc.ChanInfoRequest) (*lnrpc.ChannelEdge, error) {
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
func (r *rpcServer) GetNodeInfo(_ context.Context, in *lnrpc.NodeInfoRequest) (*lnrpc.NodeInfo, error) {

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
		numChannels  uint32
		totalCapcity btcutil.Amount
	)
	if err := node.ForEachChannel(nil, func(edge *channeldb.ChannelEdgeInfo,
		_ *channeldb.ChannelEdgePolicy) error {

		numChannels++
		totalCapcity += edge.Capacity
		return nil
	}); err != nil {
		return nil, err
	}

	// TODO(roasbeef): list channels as well?
	return &lnrpc.NodeInfo{
		Node: &lnrpc.LightningNode{
			LastUpdate: uint32(node.LastUpdate.Unix()),
			PubKey:     in.PubKey,
			Address:    node.Address.String(),
			Alias:      node.Alias,
		},
		NumChannels:   numChannels,
		TotalCapacity: int64(totalCapcity),
	}, nil
}

// QueryRoutes attempts to query the daemons' Channel Router for a possible
// route to a target destination capable of carrying a specific amount of
// satoshis within the route's flow. The retuned route contains the full
// details required to craft and send an HTLC, also including the necessary
// information that should be present within the Sphinx packet encapsualted
// within the HTLC.
//
// TODO(roasbeef): should return a slice of routes in reality
//  * create separate PR to send based on well formatted route
func (r *rpcServer) QueryRoutes(_ context.Context,
	in *lnrpc.QueryRoutesRequest) (*lnrpc.QueryRoutesResponse, error) {

	// First parse the hex-encdoed public key into a full public key objet
	// we can properly manipulate.
	pubKeyBytes, err := hex.DecodeString(in.PubKey)
	if err != nil {
		return nil, err
	}
	pubKey, err := btcec.ParsePubKey(pubKeyBytes, btcec.S256())
	if err != nil {
		return nil, err
	}

	// Query the channel router for a possible path to the destination that
	// can carry `in.Amt` satoshis _including_ the total fee required on
	// the route.
	routes, err := r.server.chanRouter.FindRoutes(pubKey,
		btcutil.Amount(in.Amt))
	if err != nil {
		return nil, err
	}

	// For each valid route, we'll convert the result into the format
	// required by the RPC system.
	routeResp := &lnrpc.QueryRoutesResponse{
		Routes: make([]*lnrpc.Route, len(routes)),
	}
	for i, route := range routes {
		routeResp.Routes[i] = marshalRoute(route)
	}

	return routeResp, nil
}

func marshalRoute(route *routing.Route) *lnrpc.Route {
	resp := &lnrpc.Route{
		TotalTimeLock: route.TotalTimeLock,
		TotalFees:     int64(route.TotalFees),
		TotalAmt:      int64(route.TotalAmount),
		Hops:          make([]*lnrpc.Hop, len(route.Hops)),
	}
	for i, hop := range route.Hops {
		resp.Hops[i] = &lnrpc.Hop{
			ChanId:       hop.Channel.ChannelID,
			ChanCapacity: int64(hop.Channel.Capacity),
			AmtToForward: int64(hop.AmtToForward),
			Fee:          int64(hop.Fee),
		}
	}

	return resp
}

// GetNetworkInfo returns some basic stats about the known channel graph from
// the PoV of the node.
func (r *rpcServer) GetNetworkInfo(context.Context, *lnrpc.NetworkInfoRequest) (*lnrpc.NetworkInfo, error) {

	graph := r.server.chanDB.ChannelGraph()

	var (
		numNodes             uint32
		numChannels          uint32
		maxChanOut           uint32
		totalNetworkCapacity btcutil.Amount
		minChannelSize       btcutil.Amount = math.MaxInt64
		maxChannelSize       btcutil.Amount
	)

	// TODO(roasbeef): ideally all below is completed in a single
	// transaction

	// First run through all the known nodes in the within our view of the
	// network, tallying up the total number of nodes, and also gathering
	// each node so we can measure the graph diamter and degree stats
	// below.
	var nodes []*channeldb.LightningNode
	if err := graph.ForEachNode(func(node *channeldb.LightningNode) error {
		numNodes++
		nodes = append(nodes, node)
		return nil
	}); err != nil {
		return nil, err
	}

	// With all the nodes gathered, we can now perform a basic traversal to
	// ascertain the graph's diameter, and also the max out-degree of a
	// node.
	for _, node := range nodes {
		var outDegree uint32
		err := node.ForEachChannel(nil, func(_ *channeldb.ChannelEdgeInfo,
			_ *channeldb.ChannelEdgePolicy) error {

			outDegree++
			return nil
		})
		if err != nil {
			return nil, err
		}

		if outDegree > maxChanOut {
			maxChanOut = outDegree
		}
	}

	// Finally, we traverse each channel visiting both channel edges at
	// once to avoid double counting any stats we're attempting to gather.
	if err := graph.ForEachChannel(func(edge *channeldb.ChannelEdgeInfo,
		_, _ *channeldb.ChannelEdgePolicy) error {

		chanCapacity := edge.Capacity

		if chanCapacity < minChannelSize {
			minChannelSize = chanCapacity
		}
		if chanCapacity > maxChannelSize {
			maxChannelSize = chanCapacity
		}

		totalNetworkCapacity += chanCapacity

		numChannels++

		return nil
	}); err != nil {
		return nil, err
	}

	// TODO(roasbeef): graph diameter

	// TODO(roasbeef): also add oldest channel?
	//  * also add median channel size
	return &lnrpc.NetworkInfo{
		MaxOutDegree:         maxChanOut,
		AvgOutDegree:         float64(numChannels) / float64(numNodes),
		NumNodes:             numNodes,
		NumChannels:          numChannels,
		TotalNetworkCapacity: int64(totalNetworkCapacity),
		AvgChannelSize:       float64(totalNetworkCapacity) / float64(numChannels),
		MinChannelSize:       int64(minChannelSize),
		MaxChannelSize:       int64(maxChannelSize),
	}, nil
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
				return errors.New("sever shutting down")
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

// marshallTopologyChange performs a mapping from the topology change sturct
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
				FundingTxid: channelUpdate.ChanPoint.Hash[:],
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
				FundingTxid: closedChan.ChanPoint.Hash[:],
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
func (r *rpcServer) ListPayments(context.Context,
	*lnrpc.ListPaymentsRequest) (*lnrpc.ListPaymentsResponse, error) {

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

		paymentsResp.Payments[i] = &lnrpc.Payment{
			PaymentHash:  hex.EncodeToString(payment.PaymentHash[:]),
			Value:        int64(payment.Terms.Value),
			CreationDate: payment.CreationDate.Unix(),
			Path:         path,
		}
	}

	return paymentsResp, nil
}

// DeleteAllPayments deletes all outgoing payments from DB.
func (r *rpcServer) DeleteAllPayments(context.Context,
	*lnrpc.DeleteAllPaymentsRequest) (*lnrpc.DeleteAllPaymentsResponse, error) {

	rpcsLog.Debugf("[DeleteAllPayments]")

	if err := r.server.chanDB.DeleteAllPayments(); err != nil {
		return nil, err
	}

	return &lnrpc.DeleteAllPaymentsResponse{}, nil
}

// SetAlias...
func (r *rpcServer) SetAlias(context.Context, *lnrpc.SetAliasRequest) (*lnrpc.SetAliasResponse, error) {
	return nil, nil
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

	// Fist we'll attempt to decode the payment request string, if the
	// request is invalid or the checksum doesn't match, then we'll exit
	// here with an error.
	payReq, err := zpay32.Decode(req.PayReq)
	if err != nil {
		return nil, err
	}

	dest := payReq.Destination.SerializeCompressed()
	return &lnrpc.PayReq{
		Destination: hex.EncodeToString(dest),
		PaymentHash: hex.EncodeToString(payReq.PaymentHash[:]),
		NumSatoshis: int64(payReq.Amount),
	}, nil
}
