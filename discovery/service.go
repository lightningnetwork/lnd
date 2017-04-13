package discovery

import (
	"bytes"
	"sync"
	"sync/atomic"
	"time"

	"github.com/davecgh/go-spew/spew"
	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcutil"
)

// waitingProofKey is the proof key which uniquely identifies the announcement
// signature message. The goal of this key is distinguish the local and remote
// proof for the same channel id.
//
// TODO(andrew.shvv) move to the channeldb package after waiting proof map
// becomes persistent.
type waitingProofKey struct {
	chanID   uint64
	isRemote bool
}

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

// Config defines the configuration for the service. ALL elements within the
// configuration MUST be non-nil for the service to carry out its duties.
type Config struct {
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
}

// New creates a new AuthenticatedGossiper instance, initialized with the
// passed configuration paramters.
func New(cfg Config) (*AuthenticatedGossiper, error) {
	return &AuthenticatedGossiper{
		cfg:                    &cfg,
		networkMsgs:            make(chan *networkMsg),
		quit:                   make(chan struct{}),
		syncRequests:           make(chan *syncRequest),
		prematureAnnouncements: make(map[uint32][]*networkMsg),
		waitingProofs:          make(map[waitingProofKey]*lnwire.AnnounceSignatures),
	}, nil
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

	// cfg is a copy of the configuration struct that the discovery service
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

	// waitingProofs is a map of partial channel proof announcement
	// messages. We use this map to buffer half of the material needed to
	// reconstruct a full authenticated channel announcement. Once we
	// receive the other half the channel proof, we'll be able to properly
	// validate it an re-broadcast it out to the network.
	//
	// TODO(andrew.shvv) make this map persistent.
	waitingProofs map[waitingProofKey]*lnwire.AnnounceSignatures

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
		nMsg.err <- errors.New("discovery has shut down")
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
		nMsg.err <- errors.New("discovery has shut down")
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

	var announcementBatch []lnwire.Message

	// TODO(roasbeef): parametrize the above
	retransmitTimer := time.NewTicker(time.Minute * 30)
	defer retransmitTimer.Stop()

	// TODO(roasbeef): parametrize the above
	trickleTimer := time.NewTicker(d.cfg.TrickleDelay)
	defer trickleTimer.Stop()

	for {
		select {
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
			err := d.cfg.Router.ForAllOutgoingChannels(func(p *channeldb.ChannelEdgePolicy) error {
				c := &lnwire.ChannelUpdateAnnouncement{
					Signature:                 p.Signature,
					ShortChannelID:            lnwire.NewShortChanIDFromInt(p.ChannelID),
					Timestamp:                 uint32(p.LastUpdate.Unix()),
					Flags:                     p.Flags,
					TimeLockDelta:             p.TimeLockDelta,
					HtlcMinimumMsat:           uint32(p.MinHTLC),
					FeeBaseMsat:               uint32(p.FeeBaseMSat),
					FeeProportionalMillionths: uint32(p.FeeProportionalMillionths),
				}
				selfChans = append(selfChans, c)
				return nil
			},
			)
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

		// The discovery has been signalled to exit, to we exit our
		// main loop so the wait group can be decremented.
		case <-d.quit:
			return
		}
	}
}

// processNetworkAnnouncement processes a new network relate authenticated
// channel or node announcement or announcements proofs. If the announcement
// didn't affect the internal state due to either being out of date, invalid,
// or redundant, then nil is returned. Otherwise, the set of announcements will
// be returned which should be broadcasted to the rest of the network.
func (d *AuthenticatedGossiper) processNetworkAnnouncement(nMsg *networkMsg) []lnwire.Message {
	isPremature := func(chanID lnwire.ShortChannelID, delta uint32) bool {
		return chanID.BlockHeight+delta > d.bestHeight
	}

	var announcements []lnwire.Message

	switch msg := nMsg.msg.(type) {

	// A new node announcement has arrived which either presents a new
	// node, or a node updating previously advertised information.
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
			LastUpdate: time.Unix(int64(msg.Timestamp), 0),
			Addresses:  msg.Addresses,
			PubKey:     msg.NodeID,
			Alias:      msg.Alias.String(),
			AuthSig:    msg.Signature,
			Features:   msg.Features,
		}

		if err := d.cfg.Router.AddNode(node); err != nil {
			if routing.IsError(err, routing.ErrOutdated, routing.ErrIgnored) {
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
		// If the advertised inclusionary block is beyond our knowledge
		// of the chain tip, then we'll put the announcement in limbo
		// to be fully verified once we advance forward in the chain.
		if isPremature(msg.ShortChannelID, 0) {
			blockHeight := msg.ShortChannelID.BlockHeight
			log.Infof("Announcement for chan_id=(%v), is premature: "+
				"advertises height %v, only height %v is known",
				msg.ShortChannelID, msg.ShortChannelID.BlockHeight,
				d.bestHeight)

			d.prematureAnnouncements[blockHeight] = append(
				d.prematureAnnouncements[blockHeight],
				nMsg,
			)
			return nil
		}

		// If this is a remote channel annoucement, then we'll validate
		// all the signatues within the proof as it should be well
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
		edge := &channeldb.ChannelEdgeInfo{
			ChannelID:   msg.ShortChannelID.ToUint64(),
			NodeKey1:    msg.NodeID1,
			NodeKey2:    msg.NodeID2,
			BitcoinKey1: msg.BitcoinKey1,
			BitcoinKey2: msg.BitcoinKey2,
			AuthProof:   proof,
		}
		if err := d.cfg.Router.AddEdge(edge); err != nil {
			if routing.IsError(err, routing.ErrOutdated,
				routing.ErrIgnored) {

				log.Debug(err)
			} else {
				log.Error(err)
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
	case *lnwire.ChannelUpdateAnnouncement:
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
			err := errors.Errorf("unable to validate channel "+
				"update announcement for short_chan_id=%v: %v",
				spew.Sdump(msg.ShortChannelID), err)

			log.Error(err)
			nMsg.err <- err
			return nil
		}

		// TODO(roasbeef): should be msat here
		update := &channeldb.ChannelEdgePolicy{
			Signature:                 msg.Signature,
			ChannelID:                 shortChanID,
			LastUpdate:                time.Unix(int64(msg.Timestamp), 0),
			Flags:                     msg.Flags,
			TimeLockDelta:             msg.TimeLockDelta,
			MinHTLC:                   btcutil.Amount(msg.HtlcMinimumMsat),
			FeeBaseMSat:               btcutil.Amount(msg.FeeBaseMsat),
			FeeProportionalMillionths: btcutil.Amount(msg.FeeProportionalMillionths),
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

		log.Infof("Received new channel announcement: %v",
			spew.Sdump(msg))

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
			err := errors.Errorf("unable to process channel "+
				"%v proof with short_chan_id=%v: %v", prefix,
				shortChanID, err)
			nMsg.err <- err
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
		oppositeKey := newProofKey(chanInfo.ChannelID, !nMsg.isRemote)
		oppositeProof, ok := d.waitingProofs[oppositeKey]
		if !ok {
			key := newProofKey(chanInfo.ChannelID, nMsg.isRemote)
			d.waitingProofs[key] = msg

			// If proof was sent b a local sub-system, then we'll
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

	// First run through all the vertexes in the graph, retrieving the data
	// for the announcement we originally retrieved.
	var numNodes uint32
	if err := d.cfg.Router.ForEachNode(func(node *channeldb.LightningNode) error {
		alias, err := lnwire.NewAlias(node.Alias)
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

	// With the vertexes gathered, we'll no retrieve the initial
	// announcement, as well as the latest channel update announcement for
	// both of the directed infos that make up the channel.
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

	log.Infof("Syncing channel graph state with %x, sending %v "+
		"vertexes and %v edges", targetNode.SerializeCompressed(),
		numNodes, numEdges)

	// With all the announcement messages gathered, send them all in a
	// single batch to the target peer.
	return d.cfg.SendToPeer(targetNode, announceMessages...)
}
