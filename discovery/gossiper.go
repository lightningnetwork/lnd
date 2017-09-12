package discovery

import (
	"bytes"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/davecgh/go-spew/spew"
	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcd/wire"
)

// networkMsg couples a routing related wire message with the peer that
// originally sent it.
type networkMsg struct {
	peer *btcec.PublicKey
	msg  lnwire.Message

	isRemote bool

	err chan error
}

// syncRequest represents a request from an outside subsystem to the wallet to
// sync a new node to the latest graph state.
type syncRequest struct {
	node *btcec.PublicKey
}

// feeUpdateRequest is a request that is sent to the server when a caller
// wishes to update the fees for a particular set of channels. New UpdateFee
// messages will be crafted to be sent out during the next broadcast epoch and
// the fee updates committed to the lower layer.
type feeUpdateRequest struct {
	targetChans []wire.OutPoint
	newSchema   routing.FeeSchema

	errResp chan error
}

// Config defines the configuration for the service. ALL elements within the
// configuration MUST be non-nil for the service to carry out its duties.
type Config struct {
	// ChainHash is a hash that indicates which resident chain of the
	// AuthenticatedGossiper. Any announcements that don't match this
	// chain hash will be ignored.
	//
	// TODO(roasbeef): eventually make into map so can de-multiplex
	// incoming announcements
	//   * also need to do same for Notifier
	ChainHash chainhash.Hash

	// Router is the subsystem which is responsible for managing the
	// topology of lightning network. After incoming channel, node, channel
	// updates announcements are validated they are sent to the router in
	// order to be included in the LN graph.
	Router routing.ChannelGraphSource

	// Notifier is used for receiving notifications of incoming blocks.
	// With each new incoming block found we process previously premature
	// announcements.
	//
	// TODO(roasbeef): could possibly just replace this with an epoch
	// channel.
	Notifier chainntnfs.ChainNotifier

	// Broadcast broadcasts a particular set of announcements to all peers
	// that the daemon is connected to. If supplied, the exclude parameter
	// indicates that the target peer should be excluded from the
	// broadcast.
	Broadcast func(exclude *btcec.PublicKey, msg ...lnwire.Message) error

	// SendToPeer is a function which allows the service to send a set of
	// messages to a particular peer identified by the target public key.
	SendToPeer func(target *btcec.PublicKey, msg ...lnwire.Message) error

	// ProofMatureDelta the number of confirmations which is needed before
	// exchange the channel announcement proofs.
	ProofMatureDelta uint32

	// TrickleDelay the period of trickle timer which flushing to the
	// network the pending batch of new announcements we've received since
	// the last trickle tick.
	TrickleDelay time.Duration

	// DB is a global boltdb instance which is needed to pass it in waiting
	// proof storage to make waiting proofs persistent.
	DB *channeldb.DB

	// AnnSigner is an instance of the MessageSigner interface which will
	// be used to manually sign any outgoing channel updates. The signer
	// implementation should be backed by the public key of the backing
	// Lightning node.
	//
	// TODO(roasbeef): extract ann crafting + sign from fundingMgr into
	// here?
	AnnSigner lnwallet.MessageSigner
}

// AuthenticatedGossiper is a subsystem which is responsible for receiving
// announcements validate them and apply the changes to router, syncing
// lightning network with newly connected nodes, broadcasting announcements
// after validation, negotiating the channel announcement proofs exchange and
// handling the premature announcements. All outgoing announcements are
// expected to be properly signed as dictated in BOLT#7, additionally, all
// incoming message are expected to be well formed and signed. Invalid messages
// will be rejected by this struct.
type AuthenticatedGossiper struct {
	// Parameters which are needed to properly handle the start and stop of
	// the service.
	started uint32
	stopped uint32
	quit    chan struct{}
	wg      sync.WaitGroup

	// cfg is a copy of the configuration struct that the gossiper service
	// was initialized with.
	cfg *Config

	// newBlocks is a channel in which new blocks connected to the end of
	// the main chain are sent over.
	newBlocks <-chan *chainntnfs.BlockEpoch

	// prematureAnnouncements maps a block height to a set of network
	// messages which are "premature" from our PoV. An message is premature
	// if it claims to be anchored in a block which is beyond the current
	// main chain tip as we know it. Premature network messages will be
	// processed once the chain tip as we know it extends to/past the
	// premature height.
	//
	// TODO(roasbeef): limit premature networkMsgs to N
	prematureAnnouncements map[uint32][]*networkMsg

	// waitingProofs is a persistent storage of partial channel proof
	// announcement messages. We use it to buffer half of the material
	// needed to reconstruct a full authenticated channel announcement. Once
	// we receive the other half the channel proof, we'll be able to
	// properly validate it an re-broadcast it out to the network.
	waitingProofs *channeldb.WaitingProofStore

	// networkMsgs is a channel that carries new network broadcasted
	// message from outside the gossiper service to be processed by the
	// networkHandler.
	networkMsgs chan *networkMsg

	// syncRequests is a channel that carries requests to synchronize newly
	// connected peers to the state of the lightning network topology from
	// our PoV.
	syncRequests chan *syncRequest

	// feeUpdates is a channel that requests to update the fee schedule of
	// a set of channels is sent over.
	feeUpdates chan *feeUpdateRequest

	// bestHeight is the height of the block at the tip of the main chain
	// as we know it.
	bestHeight uint32

	// selfKey is the identity public key of the backing Lighting node.
	selfKey *btcec.PublicKey
}

// New creates a new AuthenticatedGossiper instance, initialized with the
// passed configuration parameters.
func New(cfg Config, selfKey *btcec.PublicKey) (*AuthenticatedGossiper, error) {
	storage, err := channeldb.NewWaitingProofStore(cfg.DB)
	if err != nil {
		return nil, err
	}

	return &AuthenticatedGossiper{
		selfKey:                selfKey,
		cfg:                    &cfg,
		networkMsgs:            make(chan *networkMsg),
		quit:                   make(chan struct{}),
		syncRequests:           make(chan *syncRequest),
		feeUpdates:             make(chan *feeUpdateRequest),
		prematureAnnouncements: make(map[uint32][]*networkMsg),
		waitingProofs:          storage,
	}, nil
}

// SynchronizeNode sends a message to the service indicating it should
// synchronize lightning topology state with the target node. This method is to
// be utilized when a node connections for the first time to provide it with
// the latest topology update state.
func (d *AuthenticatedGossiper) SynchronizeNode(pub *btcec.PublicKey) {
	select {
	case d.syncRequests <- &syncRequest{
		node: pub,
	}:
	case <-d.quit:
		return
	}
}

// PropagateFeeUpdate signals the AuthenticatedGossiper to update the fee
// schema for the specified channels. If no channels are specified, then the
// fee update will be applied to all outgoing channels from the source node.
// Fee updates are done in two stages: first, the AuthenticatedGossiper ensures
// the updated has been committed by dependant sub-systems, then it signs and
// broadcasts new updates to the network.
func (d *AuthenticatedGossiper) PropagateFeeUpdate(newSchema routing.FeeSchema,
	chanPoints ...wire.OutPoint) error {

	errChan := make(chan error, 1)
	feeUpdate := &feeUpdateRequest{
		targetChans: chanPoints,
		newSchema:   newSchema,
		errResp:     errChan,
	}

	select {
	case d.feeUpdates <- feeUpdate:
		return <-errChan
	case <-d.quit:
		return fmt.Errorf("AuthenticatedGossiper shutting down")
	}
}

// Start spawns network messages handler goroutine and registers on new block
// notifications in order to properly handle the premature announcements.
func (d *AuthenticatedGossiper) Start() error {
	if !atomic.CompareAndSwapUint32(&d.started, 0, 1) {
		return nil
	}

	log.Info("Authenticated Gossiper is starting")

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

	return nil
}

// Stop signals any active goroutines for a graceful closure.
func (d *AuthenticatedGossiper) Stop() {
	if !atomic.CompareAndSwapUint32(&d.stopped, 0, 1) {
		return
	}

	log.Info("Authenticated Gossiper is stopping")

	close(d.quit)
	d.wg.Wait()
}

// ProcessRemoteAnnouncement sends a new remote announcement message along with
// the peer that sent the routing message. The announcement will be processed
// then added to a queue for batched trickled announcement to all connected
// peers.  Remote channel announcements should contain the announcement proof
// and be fully validated.
func (d *AuthenticatedGossiper) ProcessRemoteAnnouncement(msg lnwire.Message,
	src *btcec.PublicKey) chan error {

	nMsg := &networkMsg{
		msg:      msg,
		isRemote: true,
		peer:     src,
		err:      make(chan error, 1),
	}

	select {
	case d.networkMsgs <- nMsg:
	case <-d.quit:
		nMsg.err <- errors.New("gossiper has shut down")
	}

	return nMsg.err
}

// ProcessLocalAnnouncement sends a new remote announcement message along with
// the peer that sent the routing message. The announcement will be processed
// then added to a queue for batched trickled announcement to all connected
// peers.  Local channel announcements don't contain the announcement proof and
// will not be fully validated. Once the channel proofs are received, the
// entire channel announcement and update messages will be re-constructed and
// broadcast to the rest of the network.
func (d *AuthenticatedGossiper) ProcessLocalAnnouncement(msg lnwire.Message,
	src *btcec.PublicKey) chan error {

	nMsg := &networkMsg{
		msg:      msg,
		isRemote: false,
		peer:     src,
		err:      make(chan error, 1),
	}

	select {
	case d.networkMsgs <- nMsg:
	case <-d.quit:
		nMsg.err <- errors.New("gossiper has shut down")
	}

	return nMsg.err
}

// networkHandler is the primary goroutine that drives this service. The roles
// of this goroutine includes answering queries related to the state of the
// network, syncing up newly connected peers, and also periodically
// broadcasting our latest topology state to all connected peers.
//
// NOTE: This MUST be run as a goroutine.
func (d *AuthenticatedGossiper) networkHandler() {
	defer d.wg.Done()

	// TODO(roasbeef): changes for spec compliance
	//  * make into de-duplicated struct
	//  * always send chan ann -> node ann -> chan update
	//  * buffer recv'd node ann until after chan ann that includes is
	//    created
	//    * can use mostly empty struct in db as place holder
	var announcementBatch []lnwire.Message

	// TODO(roasbeef): parametrize the above
	retransmitTimer := time.NewTicker(time.Minute * 30)
	defer retransmitTimer.Stop()

	// TODO(roasbeef): parametrize the above
	trickleTimer := time.NewTicker(d.cfg.TrickleDelay)
	defer trickleTimer.Stop()

	for {
		select {
		// A new fee update has arrived. We'll commit it to the
		// sub-systems below us, then craft, sign, and broadcast a new
		// ChannelUpdate for the set of affected clients.
		case feeUpdate := <-d.feeUpdates:
			// First, we'll now create new fully signed updates for
			// the affected channels and also update the underlying
			// graph with the new state.
			newChanUpdates, err := d.processFeeChanUpdate(feeUpdate)
			if err != nil {
				log.Errorf("Unable to craft fee updates: %v", err)
				feeUpdate.errResp <- err
				continue
			}

			// Finally, with the updates committed, we'll now add
			// them to the announcement batch to be flushed at the
			// start of the next epoch.
			announcementBatch = append(announcementBatch,
				newChanUpdates...)

			feeUpdate.errResp <- nil

		case announcement := <-d.networkMsgs:
			// Process the network announcement to determine if
			// this is either a new announcement from our PoV or an
			// edges to a prior vertex/edge we previously
			// proceeded.
			emittedAnnouncements := d.processNetworkAnnouncement(announcement)

			// If the announcement was accepted, then add the
			// emitted announcements to our announce batch to be
			// broadcast once the trickle timer ticks gain.
			if emittedAnnouncements != nil {
				// TODO(roasbeef): exclude peer that sent
				announcementBatch = append(
					announcementBatch,
					emittedAnnouncements...,
				)
			}

		// A new block has arrived, so we can re-process the previously
		// premature announcements.
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
				emittedAnnouncements := d.processNetworkAnnouncement(ann)
				if emittedAnnouncements != nil {
					announcementBatch = append(
						announcementBatch,
						emittedAnnouncements...,
					)
				}
			}
			delete(d.prematureAnnouncements, blockHeight)

		// The trickle timer has ticked, which indicates we should
		// flush to the network the pending batch of new announcements
		// we've received since the last trickle tick.
		case <-trickleTimer.C:
			// If the current announcements batch is nil, then we
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
				log.Errorf("unable to send batch "+
					"announcements: %v", err)
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
			err := d.cfg.Router.ForAllOutgoingChannels(func(ei *channeldb.ChannelEdgeInfo,
				p *channeldb.ChannelEdgePolicy) error {

				c := &lnwire.ChannelUpdate{
					Signature:       p.Signature,
					ShortChannelID:  lnwire.NewShortChanIDFromInt(p.ChannelID),
					ChainHash:       ei.ChainHash,
					Timestamp:       uint32(p.LastUpdate.Unix()),
					Flags:           p.Flags,
					TimeLockDelta:   p.TimeLockDelta,
					HtlcMinimumMsat: p.MinHTLC,
					BaseFee:         uint32(p.FeeBaseMSat),
					FeeRate:         uint32(p.FeeProportionalMillionths),
				}
				selfChans = append(selfChans, c)
				return nil
			})
			if err != nil {
				log.Errorf("unable to retrieve outgoing channels: %v", err)
				continue
			}

			if len(selfChans) == 0 {
				continue
			}

			log.Debugf("Retransmitting %v outgoing channels",
				len(selfChans))

			// With all the wire announcements properly crafted,
			// we'll broadcast our known outgoing channel to all
			// our immediate peers.
			if err := d.cfg.Broadcast(nil, selfChans...); err != nil {
				log.Errorf("unable to re-broadcast "+
					"channels: %v", err)
			}

		// We've just received a new request to synchronize a peer with
		// our latest lightning network topology state. This indicates
		// that a peer has just connected for the first time, so for
		// now we dump our entire network graph and allow them to sift
		// through the (subjectively) new information on their own.
		case syncReq := <-d.syncRequests:
			nodePub := syncReq.node.SerializeCompressed()
			if err := d.synchronizeWithNode(syncReq); err != nil {
				log.Errorf("unable to sync graph state with %x: %v",
					nodePub, err)
			}

		// The gossiper has been signalled to exit, to we exit our
		// main loop so the wait group can be decremented.
		case <-d.quit:
			return
		}
	}
}

// processFeeChanUpdate generates a new set of channel updates with the new fee
// schema applied for each specified channel identified by its channel point.
// In the case that no channel points are specified, then the fee update will
// be applied to all channels. Finally, the backing ChannelGraphSource is
// updated with the latest information reflecting the
// applied fee updates.
//
// TODO(roasbeef): generalize into generic for any channel update
func (d *AuthenticatedGossiper) processFeeChanUpdate(feeUpdate *feeUpdateRequest) ([]lnwire.Message, error) {
	// First, we'll construct a set of all the channels that need to be
	// updated.
	chansToUpdate := make(map[wire.OutPoint]struct{})
	for _, chanPoint := range feeUpdate.targetChans {
		chansToUpdate[chanPoint] = struct{}{}
	}

	haveChanFilter := len(chansToUpdate) != 0

	var chanUpdates []*lnwire.ChannelUpdate
	chanEdges := make(map[lnwire.ShortChannelID]*channeldb.ChannelEdgePolicy)

	// Next, we'll loop over all the outgoing channels the router knows of.
	// If we have a filter then we'll only collected those channels,
	// otherwise we'll collect them all.
	err := d.cfg.Router.ForAllOutgoingChannels(func(info *channeldb.ChannelEdgeInfo,
		edge *channeldb.ChannelEdgePolicy) error {

		// If we have a channel filter, and this channel isn't a part
		// of it, then we'll skip it.
		if _, ok := chansToUpdate[info.ChannelPoint]; !ok && haveChanFilter {
			return nil
		}

		// Otherwise, add the channel update to our batch to be
		// updated, as we'll be re-signing it shortly.
		c := &lnwire.ChannelUpdate{
			Signature:       edge.Signature,
			ChainHash:       info.ChainHash,
			ShortChannelID:  lnwire.NewShortChanIDFromInt(edge.ChannelID),
			Timestamp:       uint32(edge.LastUpdate.Unix()),
			Flags:           edge.Flags,
			TimeLockDelta:   edge.TimeLockDelta,
			HtlcMinimumMsat: edge.MinHTLC,
			BaseFee:         uint32(edge.FeeBaseMSat),
			FeeRate:         uint32(edge.FeeProportionalMillionths),
		}
		chanUpdates = append(chanUpdates, c)

		// We'll also add it to our edge map so we can find it easily
		// later to update the state within the database.
		chanEdges[c.ShortChannelID] = edge
		return nil
	})
	if err != nil {
		return nil, err
	}

	// With the set of channel updates we need to sign obtained, we'll not
	// generate new signatures for each of them using applying the new fee
	// schema before signing.
	signedAnns := make([]lnwire.Message, len(chanUpdates))
	for i, chanUpdate := range chanUpdates {
		edge := chanEdges[chanUpdate.ShortChannelID]
		now := time.Now()

		// First, we'll apply the new few schema update to the channel
		// update and also the backing database struct.
		chanUpdate.BaseFee = uint32(feeUpdate.newSchema.BaseFee)
		chanUpdate.FeeRate = feeUpdate.newSchema.FeeRate
		chanUpdate.Timestamp = uint32(now.Unix())
		edge.FeeBaseMSat = feeUpdate.newSchema.BaseFee
		edge.FeeProportionalMillionths = lnwire.MilliSatoshi(
			feeUpdate.newSchema.FeeRate,
		)
		edge.LastUpdate = now

		// With the update applied, we'll generate a new signature over
		// a digest of the channel announcement itself.
		sig, err := SignAnnouncement(d.cfg.AnnSigner, d.selfKey,
			chanUpdate)
		if err != nil {
			return nil, err
		}

		// Next, we'll set the new signature in place, and update the
		// reference in the backing slice.
		edge.Signature = sig
		chanUpdate.Signature = sig
		signedAnns[i] = chanUpdate

		// To ensure that our signature is valid, we'll verify it
		// ourself before committing it to the slice returned.
		err = d.validateChannelUpdateAnn(d.selfKey, chanUpdate)
		if err != nil {
			return nil, fmt.Errorf("generated invalid channel update "+
				"sig: %v", err)
		}

		// Finally, we'll update the fee schema for this edge on disk.
		edge.Node.PubKey.Curve = nil
		if err := d.cfg.Router.UpdateEdge(edge); err != nil {
			return nil, err
		}
	}

	return signedAnns, nil
}

// processNetworkAnnouncement processes a new network relate authenticated
// channel or node announcement or announcements proofs. If the announcement
// didn't affect the internal state due to either being out of date, invalid,
// or redundant, then nil is returned. Otherwise, the set of announcements will
// be returned which should be broadcasted to the rest of the network.
func (d *AuthenticatedGossiper) processNetworkAnnouncement(nMsg *networkMsg) []lnwire.Message {
	isPremature := func(chanID lnwire.ShortChannelID, delta uint32) bool {
		// TODO(roasbeef) make height delta 6
		//  * or configurable
		return chanID.BlockHeight+delta > d.bestHeight
	}

	var announcements []lnwire.Message

	switch msg := nMsg.msg.(type) {

	// A new node announcement has arrived which either presents new
	// information about a node in one of the channels we know about, or a
	// updating previously advertised information.
	case *lnwire.NodeAnnouncement:
		if nMsg.isRemote {
			if err := d.validateNodeAnn(msg); err != nil {
				err := errors.Errorf("unable to validate "+
					"node announcement: %v", err)
				log.Error(err)
				nMsg.err <- err
				return nil
			}
		}

		node := &channeldb.LightningNode{
			HaveNodeAnnouncement: true,
			LastUpdate:           time.Unix(int64(msg.Timestamp), 0),
			Addresses:            msg.Addresses,
			PubKey:               msg.NodeID,
			Alias:                msg.Alias.String(),
			AuthSig:              msg.Signature,
			Features:             msg.Features,
		}

		if err := d.cfg.Router.AddNode(node); err != nil {
			if routing.IsError(err, routing.ErrOutdated,
				routing.ErrIgnored) {

				log.Debug(err)
			} else {
				log.Error(err)
			}

			nMsg.err <- err
			return nil
		}

		// Node announcement was successfully proceeded and know it
		// might be broadcast to other connected nodes.
		announcements = append(announcements, msg)

		nMsg.err <- nil
		// TODO(roasbeef): get rid of the above
		return announcements

	// A new channel announcement has arrived, this indicates the
	// *creation* of a new channel within the network. This only advertises
	// the existence of a channel and not yet the routing policies in
	// either direction of the channel.
	case *lnwire.ChannelAnnouncement:
		// We'll ignore any channel announcements that target any chain
		// other than the set of chains we know of.
		if !bytes.Equal(msg.ChainHash[:], d.cfg.ChainHash[:]) {
			log.Error("Ignoring ChannelAnnouncement from "+
				"chain=%v, gossiper on chain=%v", msg.ChainHash,
				d.cfg.ChainHash)
			return nil
		}

		// If the advertised inclusionary block is beyond our knowledge
		// of the chain tip, then we'll put the announcement in limbo
		// to be fully verified once we advance forward in the chain.
		if isPremature(msg.ShortChannelID, 0) {
			blockHeight := msg.ShortChannelID.BlockHeight
			log.Infof("Announcement for chan_id=(%v), is premature: "+
				"advertises height %v, only height %v is known",
				msg.ShortChannelID.ToUint64(),
				msg.ShortChannelID.BlockHeight, d.bestHeight)

			d.prematureAnnouncements[blockHeight] = append(
				d.prematureAnnouncements[blockHeight],
				nMsg,
			)
			return nil
		}

		// If this is a remote channel announcement, then we'll validate
		// all the signatures within the proof as it should be well
		// formed.
		var proof *channeldb.ChannelAuthProof
		if nMsg.isRemote {
			if err := d.validateChannelAnn(msg); err != nil {
				err := errors.Errorf("unable to validate "+
					"announcement: %v", err)

				log.Error(err)
				nMsg.err <- err
				return nil
			}

			// If the proof checks out, then we'll save the proof
			// itself to the database so we can fetch it later when
			// gossiping with other nodes.
			proof = &channeldb.ChannelAuthProof{
				NodeSig1:    msg.NodeSig1,
				NodeSig2:    msg.NodeSig2,
				BitcoinSig1: msg.BitcoinSig1,
				BitcoinSig2: msg.BitcoinSig2,
			}
		}

		// With the proof validate (if necessary), we can now store it
		// within the database for our path finding and syncing needs.
		var featureBuf bytes.Buffer
		if err := msg.Features.Encode(&featureBuf); err != nil {
			log.Errorf("unable to encode features: %v", err)
			nMsg.err <- err
			return nil
		}

		edge := &channeldb.ChannelEdgeInfo{
			ChannelID:   msg.ShortChannelID.ToUint64(),
			ChainHash:   msg.ChainHash,
			NodeKey1:    msg.NodeID1,
			NodeKey2:    msg.NodeID2,
			BitcoinKey1: msg.BitcoinKey1,
			BitcoinKey2: msg.BitcoinKey2,
			AuthProof:   proof,
			Features:    featureBuf.Bytes(),
		}

		// We will add the edge to the channel router. If the nodes
		// present in this channel are not present in the database, a
		// partial node will be added to represent each node while we
		// wait for a node announcement.
		if err := d.cfg.Router.AddEdge(edge); err != nil {
			if routing.IsError(err, routing.ErrOutdated,
				routing.ErrIgnored) {

				log.Debugf("Router rejected channel edge: %v",
					err)
			} else {
				log.Errorf("Router rejected channel edge: %v",
					err)
			}

			nMsg.err <- err
			return nil
		}

		// Channel announcement was successfully proceeded and know it
		// might be broadcast to other connected nodes if it was
		// announcement with proof (remote).
		if proof != nil {
			announcements = append(announcements, msg)
		}

		nMsg.err <- nil
		return announcements

	// A new authenticated channel edge update has arrived. This indicates
	// that the directional information for an already known channel has
	// been updated.
	case *lnwire.ChannelUpdate:
		// We'll ignore any channel announcements that target any chain
		// other than the set of chains we know of.
		if !bytes.Equal(msg.ChainHash[:], d.cfg.ChainHash[:]) {
			log.Error("Ignoring ChannelUpdate from "+
				"chain=%v, gossiper on chain=%v", msg.ChainHash,
				d.cfg.ChainHash)
			return nil
		}

		blockHeight := msg.ShortChannelID.BlockHeight
		shortChanID := msg.ShortChannelID.ToUint64()

		// If the advertised inclusionary block is beyond our knowledge
		// of the chain tip, then we'll put the announcement in limbo
		// to be fully verified once we advance forward in the chain.
		if isPremature(msg.ShortChannelID, 0) {
			log.Infof("Update announcement for "+
				"short_chan_id(%v), is premature: advertises "+
				"height %v, only height %v is known",
				shortChanID, blockHeight, d.bestHeight)

			d.prematureAnnouncements[blockHeight] = append(
				d.prematureAnnouncements[blockHeight],
				nMsg,
			)
			return nil
		}

		// Get the node pub key as far as we don't have it in channel
		// update announcement message. We'll need this to properly
		// verify message signature.
		chanInfo, _, _, err := d.cfg.Router.GetChannelByID(msg.ShortChannelID)
		if err != nil {
			err := errors.Errorf("unable to validate "+
				"channel update short_chan_id=%v: %v",
				shortChanID, err)
			log.Error(err)
			nMsg.err <- err
			return nil
		}

		// The flag on the channel update announcement tells us "which"
		// side of the channels directed edge is being updated.
		var pubKey *btcec.PublicKey
		switch msg.Flags {
		case 0:
			pubKey = chanInfo.NodeKey1
		case 1:
			pubKey = chanInfo.NodeKey2
		}

		// Validate the channel announcement with the expected public
		// key, In the case of an invalid channel , we'll return an
		// error to the caller and exit early.
		if err := d.validateChannelUpdateAnn(pubKey, msg); err != nil {
			rErr := errors.Errorf("unable to validate channel "+
				"update announcement for short_chan_id=%v: %v",
				spew.Sdump(msg.ShortChannelID), err)

			log.Error(rErr)
			nMsg.err <- rErr
			return nil
		}

		update := &channeldb.ChannelEdgePolicy{
			Signature:                 msg.Signature,
			ChannelID:                 shortChanID,
			LastUpdate:                time.Unix(int64(msg.Timestamp), 0),
			Flags:                     msg.Flags,
			TimeLockDelta:             msg.TimeLockDelta,
			MinHTLC:                   msg.HtlcMinimumMsat,
			FeeBaseMSat:               lnwire.MilliSatoshi(msg.BaseFee),
			FeeProportionalMillionths: lnwire.MilliSatoshi(msg.FeeRate),
		}

		if err := d.cfg.Router.UpdateEdge(update); err != nil {
			if routing.IsError(err, routing.ErrOutdated, routing.ErrIgnored) {
				log.Debug(err)
			} else {
				log.Error(err)
			}

			nMsg.err <- err
			return nil
		}

		// Channel update announcement was successfully processed and
		// now it can be broadcast to the rest of the network. However,
		// we'll only broadcast the channel update announcement if it
		// has an attached authentication proof.
		if chanInfo.AuthProof != nil {
			announcements = append(announcements, msg)
		}

		nMsg.err <- nil
		return announcements

	// A new signature announcement has been received. This indicates
	// willingness of nodes involved in the funding of a channel to
	// announce this new channel to the rest of the world.
	case *lnwire.AnnounceSignatures:
		needBlockHeight := msg.ShortChannelID.BlockHeight + d.cfg.ProofMatureDelta
		shortChanID := msg.ShortChannelID.ToUint64()

		prefix := "local"
		if nMsg.isRemote {
			prefix = "remote"
		}

		log.Infof("Received new channel announcement: %v", spew.Sdump(msg))

		// By the specification, channel announcement proofs should be
		// sent after some number of confirmations after channel was
		// registered in bitcoin blockchain. Therefore, we check if the
		// proof is premature.  If so we'll halt processing until the
		// expected announcement height.  This allows us to be tolerant
		// to other clients if this constraint was changed.
		if isPremature(msg.ShortChannelID, d.cfg.ProofMatureDelta) {
			d.prematureAnnouncements[needBlockHeight] = append(
				d.prematureAnnouncements[needBlockHeight],
				nMsg,
			)
			log.Infof("Premature proof announcement, "+
				"current block height lower than needed: %v <"+
				" %v, add announcement to reprocessing batch",
				d.bestHeight, needBlockHeight)
			return nil
		}

		// Ensure that we know of a channel with the target channel ID
		// before proceeding further.
		chanInfo, e1, e2, err := d.cfg.Router.GetChannelByID(msg.ShortChannelID)
		if err != nil {
			// TODO(andrew.shvv) this is dangerous because remote
			// node might rewrite the waiting proof.
			proof := channeldb.NewWaitingProof(nMsg.isRemote, msg)
			if err := d.waitingProofs.Add(proof); err != nil {
				err := errors.Errorf("unable to store "+
					"the proof for short_chan_id=%v: %v",
					shortChanID, err)
				log.Error(err)
				nMsg.err <- err
				return nil
			}

			log.Infof("Orphan %v proof announcement with "+
				"short_chan_id=%v, adding"+
				"to waiting batch", prefix, shortChanID)
			nMsg.err <- nil
			return nil
		}

		isFirstNode := bytes.Equal(nMsg.peer.SerializeCompressed(),
			chanInfo.NodeKey1.SerializeCompressed())
		isSecondNode := bytes.Equal(nMsg.peer.SerializeCompressed(),
			chanInfo.NodeKey2.SerializeCompressed())

		// Ensure that channel that was retrieved belongs to the peer
		// which sent the proof announcement.
		if !(isFirstNode || isSecondNode) {
			err := errors.Errorf("channel that was received not "+
				"belongs to the peer which sent the proof, "+
				"short_chan_id=%v", shortChanID)
			log.Error(err)
			nMsg.err <- err
			return nil
		}

		// Check that we received the opposite proof. If so, then we're
		// now able to construct the full proof, and create the channel
		// announcement. If we didn't receive the opposite half of the
		// proof than we should store it this one, and wait for
		// opposite to be received.
		proof := channeldb.NewWaitingProof(nMsg.isRemote, msg)
		oppositeProof, err := d.waitingProofs.Get(proof.OppositeKey())
		if err != nil && err != channeldb.ErrWaitingProofNotFound {
			err := errors.Errorf("unable to get "+
				"the opposite proof for short_chan_id=%v: %v",
				shortChanID, err)
			log.Error(err)
			nMsg.err <- err
			return nil
		}

		if err == channeldb.ErrWaitingProofNotFound {
			if err := d.waitingProofs.Add(proof); err != nil {
				err := errors.Errorf("unable to store "+
					"the proof for short_chan_id=%v: %v",
					shortChanID, err)
				log.Error(err)
				nMsg.err <- err
				return nil
			}

			// If proof was sent by a local sub-system, then we'll
			// send the announcement signature to the remote node
			// so they can also reconstruct the full channel
			// announcement.
			if !nMsg.isRemote {
				// Check that first node of the channel info
				// corresponds to us.
				var remotePeer *btcec.PublicKey
				if isFirstNode {
					remotePeer = chanInfo.NodeKey2
				} else {
					remotePeer = chanInfo.NodeKey1
				}

				err := d.cfg.SendToPeer(remotePeer, msg)
				if err != nil {
					log.Errorf("unable to send "+
						"announcement message to peer: %x",
						remotePeer.SerializeCompressed())
				}

				log.Infof("Sent channel announcement proof "+
					"for short_chan_id=%v to remote peer: "+
					"%x", shortChanID,
					remotePeer.SerializeCompressed())
			}

			log.Infof("1/2 of channel ann proof received for "+
				"short_chan_id=%v, waiting for other half",
				shortChanID)

			nMsg.err <- nil
			return nil
		}

		// If we now have both halves of the channel announcement
		// proof, then we'll reconstruct the initial announcement so we
		// can validate it shortly below.
		var dbProof channeldb.ChannelAuthProof
		if isFirstNode {
			dbProof.NodeSig1 = msg.NodeSignature
			dbProof.NodeSig2 = oppositeProof.NodeSignature
			dbProof.BitcoinSig1 = msg.BitcoinSignature
			dbProof.BitcoinSig2 = oppositeProof.BitcoinSignature
		} else {
			dbProof.NodeSig1 = oppositeProof.NodeSignature
			dbProof.NodeSig2 = msg.NodeSignature
			dbProof.BitcoinSig1 = oppositeProof.BitcoinSignature
			dbProof.BitcoinSig2 = msg.BitcoinSignature
		}
		chanAnn, e1Ann, e2Ann := createChanAnnouncement(&dbProof, chanInfo, e1, e2)

		// With all the necessary components assembled validate the
		// full channel announcement proof.
		if err := d.validateChannelAnn(chanAnn); err != nil {
			err := errors.Errorf("channel  announcement proof "+
				"for short_chan_id=%v isn't valid: %v",
				shortChanID, err)

			log.Error(err)
			nMsg.err <- err
			return nil
		}

		// If the channel was returned by the router it means that
		// existence of funding point and inclusion of nodes bitcoin
		// keys in it already checked by the router. In this stage we
		// should check that node keys are attest to the bitcoin keys
		// by validating the signatures of announcement.  If proof is
		// valid then we'll populate the channel edge with it, so we
		// can announce it on peer connect.
		err = d.cfg.Router.AddProof(msg.ShortChannelID, &dbProof)
		if err != nil {
			err := errors.Errorf("unable add proof to the "+
				"channel chanID=%v: %v", msg.ChannelID, err)
			log.Error(err)
			nMsg.err <- err
			return nil
		}

		if err := d.waitingProofs.Remove(proof.OppositeKey()); err != nil {
			err := errors.Errorf("unable remove opposite proof "+
				"for the channel with chanID=%v: %v", msg.ChannelID, err)
			log.Error(err)
			nMsg.err <- err
			return nil
		}

		// Proof was successfully created and now can announce the
		// channel to the remain network.
		log.Infof("Fully valid channel proof for short_chan_id=%v "+
			"constructed, adding to next ann batch",
			shortChanID)

		// Assemble the necessary announcements to add to the next
		// broadcasting batch.
		announcements = append(announcements, chanAnn)
		if e1Ann != nil {
			announcements = append(announcements, e1Ann)
		}
		if e2Ann != nil {
			announcements = append(announcements, e2Ann)
		}

		// If this a local announcement, then we'll send it to the
		// remote side so they can reconstruct the full channel
		// announcement proof.
		if !nMsg.isRemote {
			var remotePeer *btcec.PublicKey
			if isFirstNode {
				remotePeer = chanInfo.NodeKey2
			} else {
				remotePeer = chanInfo.NodeKey1
			}

			if err = d.cfg.SendToPeer(remotePeer, msg); err != nil {
				log.Errorf("unable to send announcement "+
					"message to peer: %x",
					remotePeer.SerializeCompressed())
			}
		}

		nMsg.err <- nil
		return announcements

	default:
		nMsg.err <- errors.New("wrong type of the announcement")
		return nil
	}
}

// synchronizeWithNode attempts to synchronize the target node in the syncReq
// to the latest channel graph state. In order to accomplish this, (currently)
// the entire network graph is read from disk, then serialized to the format
// defined within the current wire protocol. This cache of graph data is then
// sent directly to the target node.
func (d *AuthenticatedGossiper) synchronizeWithNode(syncReq *syncRequest) error {
	targetNode := syncReq.node

	// TODO(roasbeef): need to also store sig data in db
	//  * will be nice when we switch to pairing sigs would only need one ^_^

	// We'll collate all the gathered routing messages into a single slice
	// containing all the messages to be sent to the target peer.
	var announceMessages []lnwire.Message

	// As peers are expecting channel announcements before node
	// announcements, we first retrieve the initial announcement, as well as
	// the latest channel update announcement for both of the directed edges
	// that make up each channel, and queue these to be sent to the peer.
	var numEdges uint32
	if err := d.cfg.Router.ForEachChannel(func(chanInfo *channeldb.ChannelEdgeInfo,
		e1, e2 *channeldb.ChannelEdgePolicy) error {
		// First, using the parameters of the channel, along with the
		// channel authentication proof, we'll create re-create the
		// original authenticated channel announcement.
		if chanInfo.AuthProof != nil {
			chanAnn, e1Ann, e2Ann := createChanAnnouncement(
				chanInfo.AuthProof, chanInfo, e1, e2)

			announceMessages = append(announceMessages, chanAnn)
			if e1Ann != nil {
				announceMessages = append(announceMessages, e1Ann)
			}
			if e2Ann != nil {
				announceMessages = append(announceMessages, e2Ann)
			}

			numEdges++
		}

		return nil
	}); err != nil && err != channeldb.ErrGraphNoEdgesFound {
		log.Errorf("unable to sync infos with peer: %v", err)
		return err
	}

	// Run through all the vertexes in the graph, retrieving the data for
	// the node announcements we originally retrieved.
	var numNodes uint32
	if err := d.cfg.Router.ForEachNode(func(node *channeldb.LightningNode) error {
		// If this is a node we never received a node announcement for,
		// we skip it.
		if !node.HaveNodeAnnouncement {
			return nil
		}

		alias, err := lnwire.NewNodeAlias(node.Alias)
		if err != nil {
			return err
		}
		ann := &lnwire.NodeAnnouncement{
			Signature: node.AuthSig,
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

	log.Infof("Syncing channel graph state with %x, sending %v "+
		"vertexes and %v edges", targetNode.SerializeCompressed(),
		numNodes, numEdges)

	// With all the announcement messages gathered, send them all in a
	// single batch to the target peer.
	return d.cfg.SendToPeer(targetNode, announceMessages...)
}
