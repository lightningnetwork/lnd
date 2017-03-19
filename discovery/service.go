package discovery

import (
	"sync"
	"sync/atomic"
	"time"

	"encoding/hex"

	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcutil"
)

// networkMsg couples a routing related wire message with the peer that
// originally sent it.
type networkMsg struct {
	msg      lnwire.Message
	isRemote bool
	peer     *btcec.PublicKey
}

// syncRequest represents a request from an outside subsystem to the wallet to
// sync a new node to the latest graph state.
type syncRequest struct {
	node *btcec.PublicKey
}

// Config defines the configuration for the service. ALL elements within the
// configuration MUST be non-nil for the service to carry out its duties.
type Config struct {
	// Router is the subsystem which is responsible for managing the
	// topology of lightning network. After incoming channel, node,
	// channel updates announcements are validated they are sent to the
	// router in order to be included in the LN graph.
	Router routing.ChannelGraphSource

	// Notifier is used for receiving notifications of incoming blocks.
	// With each new incoming block found we process previously premature
	// announcements.
	// TODO(roasbeef): could possibly just replace this with an epoch
	// channel.
	Notifier chainntnfs.ChainNotifier

	// Broadcast broadcasts a particular set of announcements to all peers
	// that the daemon is connected to. If supplied, the exclude parameter
	// indicates that the target peer should be excluded from the broadcast.
	Broadcast func(exclude *btcec.PublicKey, msg ...lnwire.Message) error

	// SendMessages is a function which allows the service to send a set of
	// messages to a particular peer identified by the target public
	// key.
	SendMessages func(target *btcec.PublicKey, msg ...lnwire.Message) error
}

// New create new discovery service structure.
func New(cfg Config) (*Discovery, error) {
	// TODO(roasbeef): remove this place holder after sigs are properly
	// stored in the graph.
	s := "30450221008ce2bc69281ce27da07e6683571319d18e949ddfa2965fb6caa" +
		"1bf0314f882d70220299105481d63e0f4bc2a88121167221b6700d72a0e" +
		"ad154c03be696a292d24ae"
	fakeSigHex, err := hex.DecodeString(s)
	if err != nil {
		return nil, err
	}
	fakeSig, err := btcec.ParseSignature(fakeSigHex, btcec.S256())
	if err != nil {
		return nil, err
	}

	return &Discovery{
		cfg:                    &cfg,
		networkMsgs:            make(chan *networkMsg),
		quit:                   make(chan bool),
		syncRequests:           make(chan *syncRequest),
		prematureAnnouncements: make(map[uint32][]*networkMsg),
		fakeSig:                fakeSig,
	}, nil
}

// Discovery is a subsystem which is responsible for receiving announcements
// validate them and apply the changes to router, syncing lightning network
// with newly connected nodes, broadcasting announcements after validation,
// negotiating the channel announcement proofs exchange and handling the
// premature announcements.
type Discovery struct {
	// Parameters which are needed to properly handle the start and stop
	// of the service.
	started uint32
	stopped uint32
	quit    chan bool
	wg      sync.WaitGroup

	// cfg is a copy of the configuration struct that the discovery service
	// was initialized with.
	cfg *Config

	// newBlocks is a channel in which new blocks connected to the end of
	// the main chain are sent over.
	newBlocks <-chan *chainntnfs.BlockEpoch

	// prematureAnnouncements maps a blockheight to a set of announcements
	// which are "premature" from our PoV. An message is premature if
	// it claims to be anchored in a block which is beyond the current main
	// chain tip as we know it. Premature network messages will be processed
	// once the chain tip as we know it extends to/past the premature
	// height.
	//
	// TODO(roasbeef): limit premature networkMsgs to N
	prematureAnnouncements map[uint32][]*networkMsg

	// networkMsgs is a channel that carries new network broadcasted
	// message from outside the discovery service to be processed by the
	// networkHandler.
	networkMsgs chan *networkMsg

	// syncRequests is a channel that carries requests to synchronize newly
	// connected peers to the state of the lightning network topology from
	// our PoV.
	syncRequests chan *syncRequest

	// bestHeight is the height of the block at the tip of the main chain
	// as we know it.
	bestHeight uint32

	fakeSig *btcec.Signature
}

// ProcessRemoteAnnouncement sends a new remote announcement message along with
// the peer that sent the routing message. The announcement will be processed then
// added to a queue for batched trickled announcement to all connected peers.
// Remote channel announcements should contain the announcement proof and be
// fully validated.
func (d *Discovery) ProcessRemoteAnnouncement(msg lnwire.Message,
	src *btcec.PublicKey) error {

	aMsg := &networkMsg{
		msg:      msg,
		isRemote: true,
		peer:     src,
	}

	select {
	case d.networkMsgs <- aMsg:
		return nil
	case <-d.quit:
		return errors.New("discovery has been shutted down")
	}
}

// ProcessLocalAnnouncement sends a new remote announcement message along with
// the peer that sent the routing message. The announcement will be processed then
// added to a queue for batched trickled announcement to all connected peers.
// Local channel announcements not contain the announcement proof and should be
// fully validated. The channels proofs will be included farther if nodes agreed
// to announce this channel to the rest of the network.
func (d *Discovery) ProcessLocalAnnouncement(msg lnwire.Message,
	src *btcec.PublicKey) error {

	aMsg := &networkMsg{
		msg:      msg,
		isRemote: false,
		peer:     src,
	}

	select {
	case d.networkMsgs <- aMsg:
		return nil
	case <-d.quit:
		return errors.New("discovery has been shutted down")
	}
}

// SynchronizeNode sends a message to the service indicating it should
// synchronize lightning topology state with the target node. This method
// is to be utilized when a node connections for the first time to provide it
// with the latest topology update state.
func (d *Discovery) SynchronizeNode(pub *btcec.PublicKey) {
	select {
	case d.syncRequests <- &syncRequest{
		node: pub,
	}:
	case <-d.quit:
		return
	}
}

// Start spawns network messages handler goroutine and registers on new block
// notifications in order to properly handle the premature announcements.
func (d *Discovery) Start() error {
	if !atomic.CompareAndSwapUint32(&d.started, 0, 1) {
		return nil
	}

	// First we register for new notifications of newly discovered blocks.
	// We do this immediately so we'll later be able to consume any/all
	// blocks which were discovered.
	blockEpochs, err := d.cfg.Notifier.RegisterBlockEpochNtfn()
	if err != nil {
		return err
	}
	d.newBlocks = blockEpochs.Epochs

	height, err := d.cfg.Router.CurrentBlockHeight()
	if err != nil {
		return err
	}
	d.bestHeight = height

	d.wg.Add(1)
	go d.networkHandler()

	log.Info("Discovery service is started")
	return nil
}

// Stop signals any active goroutines for a graceful closure.
func (d *Discovery) Stop() {
	if !atomic.CompareAndSwapUint32(&d.stopped, 0, 1) {
		return
	}

	close(d.quit)
	d.wg.Wait()
	log.Info("Discovery service is stoped.")
}

// networkHandler is the primary goroutine. The roles of this goroutine include
// answering queries related to the state of the network, syncing up newly
// connected peers, and also periodically broadcasting our latest topology state
// to all connected peers.
//
// NOTE: This MUST be run as a goroutine.
func (d *Discovery) networkHandler() {
	defer d.wg.Done()

	var announcementBatch []lnwire.Message

	// TODO(roasbeef): parametrize the above
	retransmitTimer := time.NewTicker(time.Minute * 30)
	defer retransmitTimer.Stop()

	// TODO(roasbeef): parametrize the above
	trickleTimer := time.NewTicker(time.Millisecond * 300)
	defer trickleTimer.Stop()

	for {
		select {
		case announcement := <-d.networkMsgs:
			// Process the network announcement to determine if
			// this is either a new announcement from our PoV or an
			// updates to a prior vertex/edge we previously
			// accepted.
			accepted := d.processNetworkAnnouncement(announcement)

			// If the updates was accepted, then add it to our next
			// announcement batch to be broadcast once the trickle
			// timer ticks gain.
			if accepted {
				// TODO(roasbeef): exclude peer that sent
				announcementBatch = append(
					announcementBatch,
					announcement.msg,
				)
			}

		// A new block has arrived, so we can re-process the
		// previously premature announcements.
		case newBlock, ok := <-d.newBlocks:
			// If the channel has been closed, then this indicates
			// the daemon is shutting down, so we exit ourselves.
			if !ok {
				return
			}

			// Once a new block arrives, we updates our running
			// track of the height of the chain tip.
			blockHeight := uint32(newBlock.Height)
			d.bestHeight = blockHeight

			// Next we check if we have any premature announcements
			// for this height, if so, then we process them once
			// more as normal announcements.
			prematureAnns := d.prematureAnnouncements[uint32(newBlock.Height)]
			if len(prematureAnns) != 0 {
				log.Infof("Re-processing %v premature "+
					"announcements for height %v",
					len(prematureAnns), blockHeight)
			}

			for _, ann := range prematureAnns {
				accepted := d.processNetworkAnnouncement(ann)

				if accepted {
					announcementBatch = append(
						announcementBatch,
						ann.msg,
					)
				}
			}
			delete(d.prematureAnnouncements, blockHeight)

		// The trickle timer has ticked, which indicates we should
		// flush to the network the pending batch of new announcements
		// we've received since the last trickle tick.
		case <-trickleTimer.C:
			// If the current announcement batch is nil, then we
			// have no further work here.
			if len(announcementBatch) == 0 {
				continue
			}

			log.Infof("Broadcasting batch of %v new announcements",
				len(announcementBatch))

			// If we have new things to announce then broadcast
			// them to all our immediately connected peers.
			err := d.cfg.Broadcast(nil, announcementBatch...)
			if err != nil {
				log.Errorf("unable to send batch announcement: %v", err)
				continue
			}

			// If we're able to broadcast the current batch
			// successfully, then we reset the batch for a new
			// round of announcements.
			announcementBatch = nil

		// The retransmission timer has ticked which indicates that we
		// should broadcast our personal channels to the network. This
		// addresses the case of channel advertisements whether being
		// dropped, or not properly propagated through the network.
		case <-retransmitTimer.C:
			var selfChans []lnwire.Message

			// Iterate over our channels and construct the
			// announcements array.
			err := d.cfg.Router.ForAllOutgoingChannels(
				func(p *channeldb.ChannelEdgePolicy) error {
					c := &lnwire.ChannelUpdateAnnouncement{
						Signature:                 d.fakeSig,
						ChannelID:                 lnwire.NewChanIDFromInt(p.ChannelID),
						Timestamp:                 uint32(p.LastUpdate.Unix()),
						Flags:                     p.Flags,
						TimeLockDelta:             p.TimeLockDelta,
						HtlcMinimumMsat:           uint32(p.MinHTLC),
						FeeBaseMsat:               uint32(p.FeeBaseMSat),
						FeeProportionalMillionths: uint32(p.FeeProportionalMillionths),
					}
					selfChans = append(selfChans, c)
					return nil
				})
			if err != nil {
				log.Errorf("unable to iterate over chann"+
					"els: %v", err)
				continue
			} else if len(selfChans) == 0 {
				continue
			}

			log.Debugf("Retransmitting %v outgoing channels",
				len(selfChans))

			// With all the wire announcements properly crafted,
			// we'll broadcast our known outgoing channel to all our
			// immediate peers.
			if err := d.cfg.Broadcast(nil, selfChans...); err != nil {
				log.Errorf("unable to re-broadcast "+
					"channels: %v", err)
			}

		// We've just received a new request to synchronize a peer with
		// our latest lightning network topology state. This indicates
		// that a peer has just connected for the first time, so for now
		// we dump our entire network graph and allow them to sift
		// through the (subjectively) new information on their own.
		case syncReq := <-d.syncRequests:
			nodePub := syncReq.node.SerializeCompressed()
			log.Infof("Synchronizing channel graph with %x", nodePub)

			if err := d.synchronize(syncReq); err != nil {
				log.Errorf("unable to sync graph state with %x: %v",
					nodePub, err)
			}

		// The discovery has been signalled to exit, to we exit our main
		// loop so the wait group can be decremented.
		case <-d.quit:
			return
		}
	}
}

// processNetworkAnnouncement processes a new network relate authenticated
// channel or node announcement. If the updates didn't affect the internal state
// of the draft due to either being out of date, invalid, or redundant, then
// false is returned. Otherwise, true is returned indicating that the caller
// may want to batch this request to be broadcast to immediate peers during the
// next announcement epoch.
func (d *Discovery) processNetworkAnnouncement(aMsg *networkMsg) bool {
	isPremature := func(chanID *lnwire.ChannelID) bool {
		return chanID.BlockHeight > d.bestHeight
	}

	switch msg := aMsg.msg.(type) {

	// A new node announcement has arrived which either presents a new
	// node, or a node updating previously advertised information.
	case *lnwire.NodeAnnouncement:
		if aMsg.isRemote {
			// TODO(andrew.shvv) Add node validation
		}

		node := &channeldb.LightningNode{
			LastUpdate: time.Unix(int64(msg.Timestamp), 0),
			Addresses:  msg.Addresses,
			PubKey:     msg.NodeID,
			Alias:      msg.Alias.String(),
			AuthSig:    msg.Signature,
			Features:   msg.Features,
		}

		if err := d.cfg.Router.AddNode(node); err != nil {
			log.Errorf("unable to add node: %v", err)
			return false
		}

	// A new channel announcement has arrived, this indicates the
	// *creation* of a new channel within the network. This only advertises
	// the existence of a channel and not yet the routing policies in
	// either direction of the channel.
	case *lnwire.ChannelAnnouncement:
		// If the advertised inclusionary block is beyond our knowledge
		// of the chain tip, then we'll put the announcement in limbo
		// to be fully verified once we advance forward in the chain.
		if isPremature(&msg.ChannelID) {
			blockHeight := msg.ChannelID.BlockHeight
			log.Infof("Announcement for chan_id=(%v), is "+
				"premature: advertises height %v, only height "+
				"%v is known", msg.ChannelID, msg.ChannelID.BlockHeight,
				d.bestHeight)

			d.prematureAnnouncements[blockHeight] = append(
				d.prematureAnnouncements[blockHeight],
				aMsg,
			)
			return false
		}

		var proof *channeldb.ChannelAuthProof
		if aMsg.isRemote {
			// TODO(andrew.shvv) Add channel validation
		}

		proof = &channeldb.ChannelAuthProof{
			NodeSig1:    msg.FirstNodeSig,
			NodeSig2:    msg.SecondNodeSig,
			BitcoinSig1: msg.FirstBitcoinSig,
			BitcoinSig2: msg.SecondBitcoinSig,
		}

		edge := &channeldb.ChannelEdgeInfo{
			ChannelID:   msg.ChannelID.ToUint64(),
			NodeKey1:    msg.FirstNodeID,
			NodeKey2:    msg.SecondNodeID,
			BitcoinKey1: msg.FirstBitcoinKey,
			BitcoinKey2: msg.SecondBitcoinKey,
			AuthProof:   proof,
		}

		if err := d.cfg.Router.AddEdge(edge); err != nil {
			if !routing.IsError(err, routing.ErrOutdated) {
				log.Errorf("unable to add edge: %v", err)
			} else {
				log.Info("Unable to add edge: %v", err)
			}

			return false
		}

	// A new authenticated channel updates has arrived, this indicates
	// that the directional information for an already known channel has
	// been updated.
	case *lnwire.ChannelUpdateAnnouncement:
		chanID := msg.ChannelID.ToUint64()

		// If the advertised inclusionary block is beyond our knowledge
		// of the chain tip, then we'll put the announcement in limbo
		// to be fully verified once we advance forward in the chain.
		if isPremature(&msg.ChannelID) {
			blockHeight := msg.ChannelID.BlockHeight
			log.Infof("Update announcement for chan_id=(%v), is "+
				"premature: advertises height %v, only height "+
				"%v is known", chanID, blockHeight,
				d.bestHeight)

			d.prematureAnnouncements[blockHeight] = append(
				d.prematureAnnouncements[blockHeight],
				aMsg,
			)
			return false
		}

		if aMsg.isRemote {
			// TODO(andrew.shvv) Add update channel validation
		}

		// TODO(roasbeef): should be msat here
		update := &channeldb.ChannelEdgePolicy{
			ChannelID:                 chanID,
			LastUpdate:                time.Unix(int64(msg.Timestamp), 0),
			Flags:                     msg.Flags,
			TimeLockDelta:             msg.TimeLockDelta,
			MinHTLC:                   btcutil.Amount(msg.HtlcMinimumMsat),
			FeeBaseMSat:               btcutil.Amount(msg.FeeBaseMsat),
			FeeProportionalMillionths: btcutil.Amount(msg.FeeProportionalMillionths),
		}

		if err := d.cfg.Router.UpdateEdge(update); err != nil {
			log.Errorf("unable to update edge: %v", err)
			return false
		}
	}

	return true
}

// synchronize attempts to synchronize the target node in the syncReq to
// the latest channel graph state. In order to accomplish this, (currently) the
// entire network graph is read from disk, then serialized to the format
// defined within the current wire protocol. This cache of graph data is then
// sent directly to the target node.
func (d *Discovery) synchronize(syncReq *syncRequest) error {
	targetNode := syncReq.node

	// TODO(roasbeef): need to also store sig data in db
	//  * will be nice when we switch to pairing sigs would only need one ^_^

	// We'll collate all the gathered routing messages into a single slice
	// containing all the messages to be sent to the target peer.
	var announceMessages []lnwire.Message

	// First run through all the vertexes in the graph, retrieving the data
	// for the announcement we originally retrieved.
	var numNodes uint32
	if err := d.cfg.Router.ForEachNode(func(node *channeldb.LightningNode) error {
		alias, err := lnwire.NewAlias(node.Alias)
		if err != nil {
			return err
		}

		ann := &lnwire.NodeAnnouncement{
			Signature: d.fakeSig,
			Timestamp: uint32(node.LastUpdate.Unix()),
			Addresses: node.Addresses,
			NodeID:    node.PubKey,
			Alias:     alias,
			Features:  node.Features,
		}
		announceMessages = append(announceMessages, ann)

		numNodes++

		return nil
	}); err != nil {
		return err
	}

	// With the vertexes gathered, we'll no retrieve the initial
	// announcement, as well as the latest channel update announcement for
	// both of the directed edges that make up the channel.
	var numEdges uint32
	if err := d.cfg.Router.ForEachChannel(func(chanInfo *channeldb.ChannelEdgeInfo,
		e1, e2 *channeldb.ChannelEdgePolicy) error {

		chanID := lnwire.NewChanIDFromInt(chanInfo.ChannelID)

		// First, using the parameters of the channel, along with the
		// channel authentication proof, we'll create re-create the
		// original authenticated channel announcement.
		// TODO(andrew.shvv) skip if proof is nil
		authProof := chanInfo.AuthProof
		chanAnn := &lnwire.ChannelAnnouncement{
			FirstNodeSig:     authProof.NodeSig1,
			SecondNodeSig:    authProof.NodeSig2,
			ChannelID:        chanID,
			FirstBitcoinSig:  authProof.BitcoinSig1,
			SecondBitcoinSig: authProof.BitcoinSig2,
			FirstNodeID:      chanInfo.NodeKey1,
			SecondNodeID:     chanInfo.NodeKey2,
			FirstBitcoinKey:  chanInfo.BitcoinKey1,
			SecondBitcoinKey: chanInfo.BitcoinKey2,
		}
		announceMessages = append(announceMessages, chanAnn)

		// Since it's up to a node's policy as to whether they
		// advertise the edge in dire direction, we don't create an
		// advertisement if the edge is nil.
		if e1 != nil {
			announceMessages = append(announceMessages, &lnwire.ChannelUpdateAnnouncement{
				Signature:                 d.fakeSig,
				ChannelID:                 chanID,
				Timestamp:                 uint32(e1.LastUpdate.Unix()),
				Flags:                     0,
				TimeLockDelta:             e1.TimeLockDelta,
				HtlcMinimumMsat:           uint32(e1.MinHTLC),
				FeeBaseMsat:               uint32(e1.FeeBaseMSat),
				FeeProportionalMillionths: uint32(e1.FeeProportionalMillionths),
			})
		}
		if e2 != nil {
			announceMessages = append(announceMessages, &lnwire.ChannelUpdateAnnouncement{
				Signature:                 d.fakeSig,
				ChannelID:                 chanID,
				Timestamp:                 uint32(e2.LastUpdate.Unix()),
				Flags:                     1,
				TimeLockDelta:             e2.TimeLockDelta,
				HtlcMinimumMsat:           uint32(e2.MinHTLC),
				FeeBaseMsat:               uint32(e2.FeeBaseMSat),
				FeeProportionalMillionths: uint32(e2.FeeProportionalMillionths),
			})
		}

		numEdges++
		return nil
	}); err != nil && err != channeldb.ErrGraphNoEdgesFound {
		log.Errorf("unable to sync edges w/ peer: %v", err)
		return err
	}

	log.Infof("Syncing channel graph state with %x, sending %v "+
		"nodes and %v edges", targetNode.SerializeCompressed(),
		numNodes, numEdges)

	// With all the announcement messages gathered, send them all in a
	// single batch to the target peer.
	return d.cfg.SendMessages(targetNode, announceMessages...)
}
