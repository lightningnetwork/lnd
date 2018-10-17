package discovery

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/coreos/bbolt"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnpeer"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/multimutex"
	"github.com/lightningnetwork/lnd/routing"
)

var (
	// messageStoreKey is a key used to create a top level bucket in
	// the gossiper database, used for storing messages that are to
	// be sent to peers. Currently this is used for reliably sending
	// AnnounceSignatures messages, by persisting them until a send
	// operation has succeeded.
	messageStoreKey = []byte("message-store")

	// ErrGossiperShuttingDown is an error that is returned if the gossiper
	// is in the process of being shut down.
	ErrGossiperShuttingDown = errors.New("gossiper is shutting down")

	// ErrGossipSyncerNotFound signals that we were unable to find an active
	// gossip syncer corresponding to a gossip query message received from
	// the remote peer.
	ErrGossipSyncerNotFound = errors.New("gossip syncer not found")
)

// networkMsg couples a routing related wire message with the peer that
// originally sent it.
type networkMsg struct {
	peer   lnpeer.Peer
	source *btcec.PublicKey
	msg    lnwire.Message

	isRemote bool

	err chan error
}

// chanPolicyUpdateRequest is a request that is sent to the server when a caller
// wishes to update the channel policy (fees e.g.) for a particular set of
// channels. New ChannelUpdate messages will be crafted to be sent out during
// the next broadcast epoch and the fee updates committed to the lower layer.
type chanPolicyUpdateRequest struct {
	targetChans []wire.OutPoint
	newSchema   routing.ChannelPolicy

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

	// ChanSeries is an interfaces that provides access to a time series
	// view of the current known channel graph. Each gossipSyncer enabled
	// peer will utilize this in order to create and respond to channel
	// graph time series queries.
	ChanSeries ChannelGraphTimeSeries

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
	Broadcast func(skips map[routing.Vertex]struct{},
		msg ...lnwire.Message) error

	// SendToPeer is a function which allows the service to send a set of
	// messages to a particular peer identified by the target public key.
	SendToPeer func(target *btcec.PublicKey, msg ...lnwire.Message) error

	// FindPeer returns the actively registered peer for a given remote
	// public key. An error is returned if the peer was not found or a
	// shutdown has been requested.
	FindPeer func(identityKey *btcec.PublicKey) (lnpeer.Peer, error)

	// NotifyWhenOnline is a function that allows the gossiper to be
	// notified when a certain peer comes online, allowing it to
	// retry sending a peer message.
	//
	// NOTE: The peerChan channel must be buffered.
	NotifyWhenOnline func(peer *btcec.PublicKey, peerChan chan<- lnpeer.Peer)

	// ProofMatureDelta the number of confirmations which is needed before
	// exchange the channel announcement proofs.
	ProofMatureDelta uint32

	// TrickleDelay the period of trickle timer which flushes to the
	// network the pending batch of new announcements we've received since
	// the last trickle tick.
	TrickleDelay time.Duration

	// RetransmitDelay is the period of a timer which indicates that we
	// should check if we need re-broadcast any of our personal channels.
	RetransmitDelay time.Duration

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
// announcements, validating them and applying the changes to router, syncing
// lightning network with newly connected nodes, broadcasting announcements
// after validation, negotiating the channel announcement proofs exchange and
// handling the premature announcements. All outgoing announcements are
// expected to be properly signed as dictated in BOLT#7, additionally, all
// incoming message are expected to be well formed and signed. Invalid messages
// will be rejected by this struct.
type AuthenticatedGossiper struct {
	// Parameters which are needed to properly handle the start and stop of
	// the service. To be used atomically.
	started uint32
	stopped uint32

	// bestHeight is the height of the block at the tip of the main chain
	// as we know it. To be used atomically.
	bestHeight uint32

	quit chan struct{}
	wg   sync.WaitGroup

	// cfg is a copy of the configuration struct that the gossiper service
	// was initialized with.
	cfg *Config

	// blockEpochs encapsulates a stream of block epochs that are sent at
	// every new block height.
	blockEpochs *chainntnfs.BlockEpochEvent

	// prematureAnnouncements maps a block height to a set of network
	// messages which are "premature" from our PoV. A message is premature
	// if it claims to be anchored in a block which is beyond the current
	// main chain tip as we know it. Premature network messages will be
	// processed once the chain tip as we know it extends to/past the
	// premature height.
	//
	// TODO(roasbeef): limit premature networkMsgs to N
	prematureAnnouncements map[uint32][]*networkMsg

	// prematureChannelUpdates is a map of ChannelUpdates we have received
	// that wasn't associated with any channel we know about.  We store
	// them temporarily, such that we can reprocess them when a
	// ChannelAnnouncement for the channel is received.
	prematureChannelUpdates map[uint64][]*networkMsg
	pChanUpdMtx             sync.Mutex

	// waitingProofs is a persistent storage of partial channel proof
	// announcement messages. We use it to buffer half of the material
	// needed to reconstruct a full authenticated channel announcement.
	// Once we receive the other half the channel proof, we'll be able to
	// properly validate it and re-broadcast it out to the network.
	waitingProofs *channeldb.WaitingProofStore

	// networkMsgs is a channel that carries new network broadcasted
	// message from outside the gossiper service to be processed by the
	// networkHandler.
	networkMsgs chan *networkMsg

	// chanPolicyUpdates is a channel that requests to update the
	// forwarding policy of a set of channels is sent over.
	chanPolicyUpdates chan *chanPolicyUpdateRequest

	// selfKey is the identity public key of the backing Lightning node.
	selfKey *btcec.PublicKey

	// channelMtx is used to restrict the database access to one
	// goroutine per channel ID. This is done to ensure that when
	// the gossiper is handling an announcement, the db state stays
	// consistent between when the DB is first read until it's written.
	channelMtx *multimutex.Mutex

	rejectMtx     sync.RWMutex
	recentRejects map[uint64]struct{}

	// peerSyncers keeps track of all the gossip syncers we're maintain for
	// peers that understand this mode of operation. When we go to send out
	// new updates, for all peers in the map, we'll send the messages
	// directly to their gossiper, rather than broadcasting them. With this
	// change, we ensure we filter out all updates properly.
	syncerMtx   sync.RWMutex
	peerSyncers map[routing.Vertex]*gossipSyncer

	sync.Mutex
}

// New creates a new AuthenticatedGossiper instance, initialized with the
// passed configuration parameters.
func New(cfg Config, selfKey *btcec.PublicKey) (*AuthenticatedGossiper, error) {
	storage, err := channeldb.NewWaitingProofStore(cfg.DB)
	if err != nil {
		return nil, err
	}

	return &AuthenticatedGossiper{
		selfKey:                 selfKey,
		cfg:                     &cfg,
		networkMsgs:             make(chan *networkMsg),
		quit:                    make(chan struct{}),
		chanPolicyUpdates:       make(chan *chanPolicyUpdateRequest),
		prematureAnnouncements:  make(map[uint32][]*networkMsg),
		prematureChannelUpdates: make(map[uint64][]*networkMsg),
		waitingProofs:           storage,
		channelMtx:              multimutex.NewMutex(),
		recentRejects:           make(map[uint64]struct{}),
		peerSyncers:             make(map[routing.Vertex]*gossipSyncer),
	}, nil
}

// SynchronizeNode sends a message to the service indicating it should
// synchronize lightning topology state with the target node. This method is to
// be utilized when a node connections for the first time to provide it with
// the latest topology update state.  In order to accomplish this, (currently)
// the entire network graph is read from disk, then serialized to the format
// defined within the current wire protocol. This cache of graph data is then
// sent directly to the target node.
func (d *AuthenticatedGossiper) SynchronizeNode(syncPeer lnpeer.Peer) error {
	// TODO(roasbeef): need to also store sig data in db
	//  * will be nice when we switch to pairing sigs would only need one ^_^

	// We'll collate all the gathered routing messages into a single slice
	// containing all the messages to be sent to the target peer.
	var announceMessages []lnwire.Message

	// We'll use this map to ensure we don't send the same node
	// announcement more than one time as one node may have many channel
	// anns we'll need to send.
	nodePubsSent := make(map[routing.Vertex]struct{})

	// As peers are expecting channel announcements before node
	// announcements, we first retrieve the initial announcement, as well as
	// the latest channel update announcement for both of the directed edges
	// that make up each channel, and queue these to be sent to the peer.
	var (
		numEdges uint32
		numNodes uint32
	)
	if err := d.cfg.Router.ForEachChannel(func(chanInfo *channeldb.ChannelEdgeInfo,
		e1, e2 *channeldb.ChannelEdgePolicy) error {

		// First, using the parameters of the channel, along with the
		// channel authentication proof, we'll create re-create the
		// original authenticated channel announcement. If the channel
		// also has known validated nodes, then we'll send that as
		// well.
		if chanInfo.AuthProof != nil {
			chanAnn, e1Ann, e2Ann, err := CreateChanAnnouncement(
				chanInfo.AuthProof, chanInfo, e1, e2,
			)
			if err != nil {
				return err
			}

			announceMessages = append(announceMessages, chanAnn)
			if e1Ann != nil {
				announceMessages = append(announceMessages, e1Ann)

				// If this edge has a validated node
				// announcement, that we haven't yet sent, then
				// we'll send that as well.
				nodePub := e1.Node.PubKeyBytes
				hasNodeAnn := e1.Node.HaveNodeAnnouncement
				if _, ok := nodePubsSent[nodePub]; !ok && hasNodeAnn {
					nodeAnn, err := e1.Node.NodeAnnouncement(true)
					if err != nil {
						return err
					}

					announceMessages = append(
						announceMessages, nodeAnn,
					)
					nodePubsSent[nodePub] = struct{}{}

					numNodes++
				}
			}
			if e2Ann != nil {
				announceMessages = append(announceMessages, e2Ann)

				// If this edge has a validated node
				// announcement, that we haven't yet sent, then
				// we'll send that as well.
				nodePub := e2.Node.PubKeyBytes
				hasNodeAnn := e2.Node.HaveNodeAnnouncement
				if _, ok := nodePubsSent[nodePub]; !ok && hasNodeAnn {
					nodeAnn, err := e2.Node.NodeAnnouncement(true)
					if err != nil {
						return err
					}

					announceMessages = append(
						announceMessages, nodeAnn,
					)
					nodePubsSent[nodePub] = struct{}{}

					numNodes++
				}
			}

			numEdges++
		}

		return nil
	}); err != nil && err != channeldb.ErrGraphNoEdgesFound {
		log.Errorf("unable to sync infos with peer: %v", err)
		return err
	}

	log.Infof("Syncing channel graph state with %x, sending %v "+
		"vertexes and %v edges", syncPeer.PubKey(),
		numNodes, numEdges)

	// With all the announcement messages gathered, send them all in a
	// single batch to the target peer.
	return syncPeer.SendMessage(false, announceMessages...)
}

// PropagateChanPolicyUpdate signals the AuthenticatedGossiper to update the
// channel forwarding policies for the specified channels. If no channels are
// specified, then the update will be applied to all outgoing channels from the
// source node. Policy updates are done in two stages: first, the
// AuthenticatedGossiper ensures the update has been committed by dependent
// sub-systems, then it signs and broadcasts new updates to the network.
func (d *AuthenticatedGossiper) PropagateChanPolicyUpdate(
	newSchema routing.ChannelPolicy, chanPoints ...wire.OutPoint) error {

	errChan := make(chan error, 1)
	policyUpdate := &chanPolicyUpdateRequest{
		targetChans: chanPoints,
		newSchema:   newSchema,
		errResp:     errChan,
	}

	select {
	case d.chanPolicyUpdates <- policyUpdate:
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
	blockEpochs, err := d.cfg.Notifier.RegisterBlockEpochNtfn(nil)
	if err != nil {
		return err
	}
	d.blockEpochs = blockEpochs

	height, err := d.cfg.Router.CurrentBlockHeight()
	if err != nil {
		return err
	}
	d.bestHeight = height

	// In case we had an AnnounceSignatures ready to be sent when the
	// gossiper was last shut down, we must continue on our quest to
	// deliver this message to our peer such that they can craft the
	// full channel proof.
	if err := d.resendAnnounceSignatures(); err != nil {
		return err
	}

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

	d.blockEpochs.Cancel()

	d.syncerMtx.RLock()
	for _, syncer := range d.peerSyncers {
		syncer.Stop()
	}
	d.syncerMtx.RUnlock()

	close(d.quit)
	d.wg.Wait()
}

// TODO(roasbeef): need method to get current gossip timestamp?
//  * using mtx, check time rotate forward is needed?

// ProcessRemoteAnnouncement sends a new remote announcement message along with
// the peer that sent the routing message. The announcement will be processed
// then added to a queue for batched trickled announcement to all connected
// peers.  Remote channel announcements should contain the announcement proof
// and be fully validated.
func (d *AuthenticatedGossiper) ProcessRemoteAnnouncement(msg lnwire.Message,
	peer lnpeer.Peer) chan error {

	nMsg := &networkMsg{
		msg:      msg,
		isRemote: true,
		peer:     peer,
		source:   peer.IdentityKey(),
		err:      make(chan error, 1),
	}

	select {
	case d.networkMsgs <- nMsg:

	// If the peer that sent us this error is quitting, then we don't need
	// to send back an error and can return immediately.
	case <-peer.QuitSignal():
		return nil
	case <-d.quit:
		nMsg.err <- ErrGossiperShuttingDown
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
	source *btcec.PublicKey) chan error {

	nMsg := &networkMsg{
		msg:      msg,
		isRemote: false,
		source:   source,
		err:      make(chan error, 1),
	}

	select {
	case d.networkMsgs <- nMsg:
	case <-d.quit:
		nMsg.err <- ErrGossiperShuttingDown
	}

	return nMsg.err
}

// channelUpdateID is a unique identifier for ChannelUpdate messages, as
// channel updates can be identified by the (ShortChannelID, Flags)
// tuple.
type channelUpdateID struct {
	// channelID represents the set of data which is needed to
	// retrieve all necessary data to validate the channel existence.
	channelID lnwire.ShortChannelID

	// Flags least-significant bit must be set to 0 if the creating node
	// corresponds to the first node in the previously sent channel
	// announcement and 1 otherwise.
	flags lnwire.ChanUpdateFlag
}

// msgWithSenders is a wrapper struct around a message, and the set of peers
// that originally sent us this message. Using this struct, we can ensure that
// we don't re-send a message to the peer that sent it to us in the first
// place.
type msgWithSenders struct {
	// msg is the wire message itself.
	msg lnwire.Message

	// sender is the set of peers that sent us this message.
	senders map[routing.Vertex]struct{}
}

// mergeSyncerMap is used to merge the set of senders of a particular message
// with peers that we have an active gossipSyncer with. We do this to ensure
// that we don't broadcast messages to any peers that we have active gossip
// syncers for.
func (m *msgWithSenders) mergeSyncerMap(syncers map[routing.Vertex]*gossipSyncer) {
	for peerPub := range syncers {
		m.senders[peerPub] = struct{}{}
	}
}

// deDupedAnnouncements de-duplicates announcements that have been added to the
// batch. Internally, announcements are stored in three maps
// (one each for channel announcements, channel updates, and node
// announcements). These maps keep track of unique announcements and ensure no
// announcements are duplicated. We keep the three message types separate, such
// that we can send channel announcements first, then channel updates, and
// finally node announcements when it's time to broadcast them.
type deDupedAnnouncements struct {
	// channelAnnouncements are identified by the short channel id field.
	channelAnnouncements map[lnwire.ShortChannelID]msgWithSenders

	// channelUpdates are identified by the channel update id field.
	channelUpdates map[channelUpdateID]msgWithSenders

	// nodeAnnouncements are identified by the Vertex field.
	nodeAnnouncements map[routing.Vertex]msgWithSenders

	sync.Mutex
}

// Reset operates on deDupedAnnouncements to reset the storage of
// announcements.
func (d *deDupedAnnouncements) Reset() {
	d.Lock()
	defer d.Unlock()

	d.reset()
}

// reset is the private version of the Reset method. We have this so we can
// call this method within method that are already holding the lock.
func (d *deDupedAnnouncements) reset() {
	// Storage of each type of announcement (channel announcements, channel
	// updates, node announcements) is set to an empty map where the
	// appropriate key points to the corresponding lnwire.Message.
	d.channelAnnouncements = make(map[lnwire.ShortChannelID]msgWithSenders)
	d.channelUpdates = make(map[channelUpdateID]msgWithSenders)
	d.nodeAnnouncements = make(map[routing.Vertex]msgWithSenders)
}

// addMsg adds a new message to the current batch. If the message is already
// present in the current batch, then this new instance replaces the latter,
// and the set of senders is updated to reflect which node sent us this
// message.
func (d *deDupedAnnouncements) addMsg(message networkMsg) {
	// Depending on the message type (channel announcement, channel update,
	// or node announcement), the message is added to the corresponding map
	// in deDupedAnnouncements. Because each identifying key can have at
	// most one value, the announcements are de-duplicated, with newer ones
	// replacing older ones.
	switch msg := message.msg.(type) {

	// Channel announcements are identified by the short channel id field.
	case *lnwire.ChannelAnnouncement:
		deDupKey := msg.ShortChannelID
		sender := routing.NewVertex(message.source)

		mws, ok := d.channelAnnouncements[deDupKey]
		if !ok {
			mws = msgWithSenders{
				msg:     msg,
				senders: make(map[routing.Vertex]struct{}),
			}
			mws.senders[sender] = struct{}{}

			d.channelAnnouncements[deDupKey] = mws

			return
		}

		mws.msg = msg
		mws.senders[sender] = struct{}{}
		d.channelAnnouncements[deDupKey] = mws

	// Channel updates are identified by the (short channel id, flags)
	// tuple.
	case *lnwire.ChannelUpdate:
		sender := routing.NewVertex(message.source)
		deDupKey := channelUpdateID{
			msg.ShortChannelID,
			msg.Flags,
		}

		oldTimestamp := uint32(0)
		mws, ok := d.channelUpdates[deDupKey]
		if ok {
			// If we already have seen this message, record its
			// timestamp.
			oldTimestamp = mws.msg.(*lnwire.ChannelUpdate).Timestamp
		}

		// If we already had this message with a strictly newer
		// timestamp, then we'll just discard the message we got.
		if oldTimestamp > msg.Timestamp {
			return
		}

		// If the message we just got is newer than what we previously
		// have seen, or this is the first time we see it, then we'll
		// add it to our map of announcements.
		if oldTimestamp < msg.Timestamp {
			mws = msgWithSenders{
				msg:     msg,
				senders: make(map[routing.Vertex]struct{}),
			}

			// We'll mark the sender of the message in the
			// senders map.
			mws.senders[sender] = struct{}{}

			d.channelUpdates[deDupKey] = mws

			return
		}

		// Lastly, if we had seen this exact message from before, with
		// the same timestamp, we'll add the sender to the map of
		// senders, such that we can skip sending this message back in
		// the next batch.
		mws.msg = msg
		mws.senders[sender] = struct{}{}
		d.channelUpdates[deDupKey] = mws

	// Node announcements are identified by the Vertex field.  Use the
	// NodeID to create the corresponding Vertex.
	case *lnwire.NodeAnnouncement:
		sender := routing.NewVertex(message.source)
		deDupKey := routing.Vertex(msg.NodeID)

		// We do the same for node announcements as we did for channel
		// updates, as they also carry a timestamp.
		oldTimestamp := uint32(0)
		mws, ok := d.nodeAnnouncements[deDupKey]
		if ok {
			oldTimestamp = mws.msg.(*lnwire.NodeAnnouncement).Timestamp
		}

		// Discard the message if it's old.
		if oldTimestamp > msg.Timestamp {
			return
		}

		// Replace if it's newer.
		if oldTimestamp < msg.Timestamp {
			mws = msgWithSenders{
				msg:     msg,
				senders: make(map[routing.Vertex]struct{}),
			}

			mws.senders[sender] = struct{}{}

			d.nodeAnnouncements[deDupKey] = mws

			return
		}

		// Add to senders map if it's the same as we had.
		mws.msg = msg
		mws.senders[sender] = struct{}{}
		d.nodeAnnouncements[deDupKey] = mws
	}
}

// AddMsgs is a helper method to add multiple messages to the announcement
// batch.
func (d *deDupedAnnouncements) AddMsgs(msgs ...networkMsg) {
	d.Lock()
	defer d.Unlock()

	for _, msg := range msgs {
		d.addMsg(msg)
	}
}

// Emit returns the set of de-duplicated announcements to be sent out during
// the next announcement epoch, in the order of channel announcements, channel
// updates, and node announcements. Each message emitted, contains the set of
// peers that sent us the message. This way, we can ensure that we don't waste
// bandwidth by re-sending a message to the peer that sent it to us in the
// first place. Additionally, the set of stored messages are reset.
func (d *deDupedAnnouncements) Emit() []msgWithSenders {
	d.Lock()
	defer d.Unlock()

	// Get the total number of announcements.
	numAnnouncements := len(d.channelAnnouncements) + len(d.channelUpdates) +
		len(d.nodeAnnouncements)

	// Create an empty array of lnwire.Messages with a length equal to
	// the total number of announcements.
	msgs := make([]msgWithSenders, 0, numAnnouncements)

	// Add the channel announcements to the array first.
	for _, message := range d.channelAnnouncements {
		msgs = append(msgs, message)
	}

	// Then add the channel updates.
	for _, message := range d.channelUpdates {
		msgs = append(msgs, message)
	}

	// Finally add the node announcements.
	for _, message := range d.nodeAnnouncements {
		msgs = append(msgs, message)
	}

	d.reset()

	// Return the array of lnwire.messages.
	return msgs
}

// resendAnnounceSignatures will inspect the messageStore database bucket for
// AnnounceSignatures messages that we recently tried to send to a peer. If the
// associated channels still not have the full channel proofs assembled, we
// will try to resend them. If we have the full proof, we can safely delete the
// message from the messageStore.
func (d *AuthenticatedGossiper) resendAnnounceSignatures() error {
	type msgTuple struct {
		peer  *btcec.PublicKey
		msg   *lnwire.AnnounceSignatures
		dbKey []byte
	}

	// Fetch all the AnnounceSignatures messages that was added to the
	// database.
	//
	// TODO(halseth): database access should be abstracted
	// behind interface.
	var msgsResend []msgTuple
	if err := d.cfg.DB.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(messageStoreKey)
		if bucket == nil {
			return nil
		}

		if err := bucket.ForEach(func(k, v []byte) error {
			// The database value represents the encoded
			// AnnounceSignatures message.
			r := bytes.NewReader(v)
			msg := &lnwire.AnnounceSignatures{}
			if err := msg.Decode(r, 0); err != nil {
				return err
			}

			// The first 33 bytes of the database key is the peer's
			// public key.
			peer, err := btcec.ParsePubKey(k[:33], btcec.S256())
			if err != nil {
				return err
			}

			// Make a copy of the database key corresponding to
			// these AnnounceSignatures.
			dbKey := make([]byte, len(k))
			copy(dbKey, k)

			t := msgTuple{peer, msg, dbKey}

			// Add the message to the slice, such that we can
			// resend it after the database transaction is over.
			msgsResend = append(msgsResend, t)
			return nil
		}); err != nil {
			return err
		}
		return nil
	}); err != nil {
		return err
	}

	// deleteMsg removes the message associated with the passed msgTuple
	// from the messageStore.
	deleteMsg := func(t msgTuple) error {
		log.Debugf("Deleting message for chanID=%v from "+
			"messageStore", t.msg.ChannelID)
		if err := d.cfg.DB.Update(func(tx *bolt.Tx) error {
			bucket := tx.Bucket(messageStoreKey)
			if bucket == nil {
				return fmt.Errorf("bucket " +
					"unexpectedly did not exist")
			}

			return bucket.Delete(t.dbKey[:])
		}); err != nil {
			return fmt.Errorf("Failed deleting message "+
				"from database: %v", err)
		}
		return nil
	}

	// We now iterate over these messages, resending those that we don't
	// have the full proof for, deleting the rest.
	for _, t := range msgsResend {
		// Check if the full channel proof exists in our graph.
		chanInfo, _, _, err := d.cfg.Router.GetChannelByID(
			t.msg.ShortChannelID)
		if err != nil {
			// If the channel cannot be found, it is most likely a
			// leftover message for a channel that was closed.  In
			// this case we delete it from the message store.
			log.Warnf("unable to fetch channel info for "+
				"chanID=%v from graph: %v. Will delete local"+
				"proof from database",
				t.msg.ChannelID, err)
			if err := deleteMsg(t); err != nil {
				return err
			}
			continue
		}

		// 1. If the full proof does not exist in the graph, it means
		// that we haven't received the remote proof yet (or that we
		// crashed before able to assemble the full proof). Since the
		// remote node might think they have delivered their proof to
		// us, we will resend _our_ proof to trigger a resend on their
		// part: they will then be able to assemble and send us the
		// full proof.
		if chanInfo.AuthProof == nil {
			err := d.sendAnnSigReliably(t.msg, t.peer)
			if err != nil {
				return err
			}
			continue
		}

		// 2. If the proof does exist in the graph, we have
		// successfully received the remote proof and assembled the
		// full proof. In this case we can safely delete the local
		// proof from the database. In case the remote hasn't been able
		// to assemble the full proof yet (maybe because of a crash),
		// we will send them the full proof if we notice that they
		// retry sending their half proof.
		if chanInfo.AuthProof != nil {
			log.Debugf("Deleting message for chanID=%v from "+
				"messageStore", t.msg.ChannelID)
			if err := deleteMsg(t); err != nil {
				return err
			}
		}
	}
	return nil
}

// findGossipSyncer is a utility method used by the gossiper to locate the
// gossip syncer for an inbound message so we can properly dispatch the
// incoming message. If a gossip syncer isn't found, then one will be created
// for the target peer.
func (d *AuthenticatedGossiper) findGossipSyncer(pub *btcec.PublicKey) (
	*gossipSyncer, error) {

	target := routing.NewVertex(pub)

	// First, we'll try to find an existing gossiper for this peer.
	d.syncerMtx.RLock()
	syncer, ok := d.peerSyncers[target]
	d.syncerMtx.RUnlock()

	// If one exists, then we'll return it directly.
	if ok {
		return syncer, nil
	}

	return nil, ErrGossipSyncerNotFound
}

// networkHandler is the primary goroutine that drives this service. The roles
// of this goroutine includes answering queries related to the state of the
// network, syncing up newly connected peers, and also periodically
// broadcasting our latest topology state to all connected peers.
//
// NOTE: This MUST be run as a goroutine.
func (d *AuthenticatedGossiper) networkHandler() {
	defer d.wg.Done()

	// Initialize empty deDupedAnnouncements to store announcement batch.
	announcements := deDupedAnnouncements{}
	announcements.Reset()

	retransmitTimer := time.NewTicker(d.cfg.RetransmitDelay)
	defer retransmitTimer.Stop()

	trickleTimer := time.NewTicker(d.cfg.TrickleDelay)
	defer trickleTimer.Stop()

	// To start, we'll first check to see if there are any stale channels
	// that we need to re-transmit.
	if err := d.retransmitStaleChannels(); err != nil {
		log.Errorf("Unable to rebroadcast stale channels: %v", err)
	}

	// We'll use this validation to ensure that we process jobs in their
	// dependency order during parallel validation.
	validationBarrier := routing.NewValidationBarrier(
		runtime.NumCPU()*4, d.quit,
	)

	for {
		select {
		// A new policy update has arrived. We'll commit it to the
		// sub-systems below us, then craft, sign, and broadcast a new
		// ChannelUpdate for the set of affected clients.
		case policyUpdate := <-d.chanPolicyUpdates:
			// First, we'll now create new fully signed updates for
			// the affected channels and also update the underlying
			// graph with the new state.
			newChanUpdates, err := d.processChanPolicyUpdate(
				policyUpdate,
			)
			if err != nil {
				log.Errorf("Unable to craft policy updates: %v",
					err)
				policyUpdate.errResp <- err
				continue
			}

			// Finally, with the updates committed, we'll now add
			// them to the announcement batch to be flushed at the
			// start of the next epoch.
			announcements.AddMsgs(newChanUpdates...)

			policyUpdate.errResp <- nil

		case announcement := <-d.networkMsgs:
			switch msg := announcement.msg.(type) {
			// Channel announcement signatures are amongst the only
			// messages that we'll process serially.
			case *lnwire.AnnounceSignatures:
				emittedAnnouncements := d.processNetworkAnnouncement(
					announcement,
				)
				if emittedAnnouncements != nil {
					announcements.AddMsgs(
						emittedAnnouncements...,
					)
				}
				continue

			// If a peer is updating its current update horizon,
			// then we'll dispatch that directly to the proper
			// gossipSyncer.
			case *lnwire.GossipTimestampRange:
				syncer, err := d.findGossipSyncer(
					announcement.source,
				)
				if err != nil {
					log.Warnf("Unable to find gossip "+
						"syncer for peer=%x: %v",
						announcement.peer.PubKey(), err)
					continue
				}

				// If we've found the message target, then
				// we'll dispatch the message directly to it.
				err = syncer.ApplyGossipFilter(msg)
				if err != nil {
					log.Warnf("unable to apply gossip "+
						"filter for peer=%x: %v",
						announcement.peer.PubKey(), err)
				}
				continue

			// For messages in the known set of channel series
			// queries, we'll dispatch the message directly to the
			// peer, and skip the main processing loop.
			case *lnwire.QueryShortChanIDs,
				*lnwire.QueryChannelRange,
				*lnwire.ReplyChannelRange,
				*lnwire.ReplyShortChanIDsEnd:

				syncer, err := d.findGossipSyncer(
					announcement.source,
				)
				if err != nil {
					log.Warnf("Unable to find gossip "+
						"syncer for peer=%x: %v",
						announcement.source, err)
					continue
				}

				syncer.ProcessQueryMsg(announcement.msg)
				continue
			}

			// If this message was recently rejected, then we won't
			// attempt to re-process it.
			if d.isRecentlyRejectedMsg(announcement.msg) {
				announcement.err <- fmt.Errorf("recently " +
					"rejected")
				continue
			}

			// We'll set up any dependent, and wait until a free
			// slot for this job opens up, this allow us to not
			// have thousands of goroutines active.
			validationBarrier.InitJobDependencies(announcement.msg)

			d.wg.Add(1)
			go func() {
				defer d.wg.Done()
				defer validationBarrier.CompleteJob()

				// If this message has an existing dependency,
				// then we'll wait until that has been fully
				// validated before we proceed.
				err := validationBarrier.WaitForDependants(
					announcement.msg,
				)
				if err != nil {
					if err != routing.ErrVBarrierShuttingDown {
						log.Warnf("unexpected error "+
							"during validation "+
							"barrier shutdown: %v",
							err)
					}
					announcement.err <- err
					return
				}

				// Process the network announcement to
				// determine if this is either a new
				// announcement from our PoV or an edges to a
				// prior vertex/edge we previously proceeded.
				emittedAnnouncements := d.processNetworkAnnouncement(
					announcement,
				)

				// If this message had any dependencies, then
				// we can now signal them to continue.
				validationBarrier.SignalDependants(
					announcement.msg,
				)

				// If the announcement was accepted, then add
				// the emitted announcements to our announce
				// batch to be broadcast once the trickle timer
				// ticks gain.
				if emittedAnnouncements != nil {
					// TODO(roasbeef): exclude peer that
					// sent.
					announcements.AddMsgs(
						emittedAnnouncements...,
					)
				}

			}()

		// A new block has arrived, so we can re-process the previously
		// premature announcements.
		case newBlock, ok := <-d.blockEpochs.Epochs:
			// If the channel has been closed, then this indicates
			// the daemon is shutting down, so we exit ourselves.
			if !ok {
				return
			}

			// Once a new block arrives, we updates our running
			// track of the height of the chain tip.
			blockHeight := uint32(newBlock.Height)
			atomic.StoreUint32(&d.bestHeight, blockHeight)

			// Next we check if we have any premature announcements
			// for this height, if so, then we process them once
			// more as normal announcements.
			d.Lock()
			numPremature := len(d.prematureAnnouncements[blockHeight])
			d.Unlock()

			// Return early if no announcement to process.
			if numPremature == 0 {
				continue
			}

			log.Infof("Re-processing %v premature announcements "+
				"for height %v", numPremature, blockHeight)

			d.Lock()
			for _, ann := range d.prematureAnnouncements[blockHeight] {
				emittedAnnouncements := d.processNetworkAnnouncement(ann)
				if emittedAnnouncements != nil {
					announcements.AddMsgs(
						emittedAnnouncements...,
					)
				}
			}
			delete(d.prematureAnnouncements, blockHeight)
			d.Unlock()

		// The trickle timer has ticked, which indicates we should
		// flush to the network the pending batch of new announcements
		// we've received since the last trickle tick.
		case <-trickleTimer.C:
			// Emit the current batch of announcements from
			// deDupedAnnouncements.
			announcementBatch := announcements.Emit()

			// If the current announcements batch is nil, then we
			// have no further work here.
			if len(announcementBatch) == 0 {
				continue
			}

			// For the set of peers that have an active gossip
			// syncers, we'll collect their pubkeys so we can avoid
			// sending them the full message blast below.
			d.syncerMtx.RLock()
			syncerPeers := make(map[routing.Vertex]*gossipSyncer)
			for peerPub, syncer := range d.peerSyncers {
				syncerPeers[peerPub] = syncer
			}
			d.syncerMtx.RUnlock()

			log.Infof("Broadcasting batch of %v new announcements",
				len(announcementBatch))

			// We'll first attempt to filter out this new message
			// for all peers that have active gossip syncers
			// active.
			for _, syncer := range syncerPeers {
				syncer.FilterGossipMsgs(announcementBatch...)
			}

			// Next, If we have new things to announce then
			// broadcast them to all our immediately connected
			// peers.
			for _, msgChunk := range announcementBatch {
				// With the syncers taken care of, we'll merge
				// the sender map with the set of syncers, so
				// we don't send out duplicate messages.
				msgChunk.mergeSyncerMap(syncerPeers)

				err := d.cfg.Broadcast(
					msgChunk.senders, msgChunk.msg,
				)
				if err != nil {
					log.Errorf("unable to send batch "+
						"announcements: %v", err)
					continue
				}
			}

		// The retransmission timer has ticked which indicates that we
		// should check if we need to prune or re-broadcast any of our
		// personal channels. This addresses the case of "zombie"
		// channels and channel advertisements that have been dropped,
		// or not properly propagated through the network.
		case <-retransmitTimer.C:
			if err := d.retransmitStaleChannels(); err != nil {
				log.Errorf("unable to rebroadcast stale "+
					"channels: %v", err)
			}

		// The gossiper has been signalled to exit, to we exit our
		// main loop so the wait group can be decremented.
		case <-d.quit:
			return
		}
	}
}

// TODO(roasbeef): d/c peers that send updates not on our chain

// InitSyncState is called by outside sub-systems when a connection is
// established to a new peer that understands how to perform channel range
// queries. We'll allocate a new gossip syncer for it, and start any goroutines
// needed to handle new queries. The recvUpdates bool indicates if we should
// continue to receive real-time updates from the remote peer once we've synced
// channel state.
func (d *AuthenticatedGossiper) InitSyncState(syncPeer lnpeer.Peer,
	recvUpdates bool) {

	d.syncerMtx.Lock()
	defer d.syncerMtx.Unlock()

	// If we already have a syncer, then we'll exit early as we don't want
	// to override it.
	nodeID := routing.Vertex(syncPeer.PubKey())
	if _, ok := d.peerSyncers[nodeID]; ok {
		return
	}

	log.Infof("Creating new gossipSyncer for peer=%x", nodeID[:])

	encoding := lnwire.EncodingSortedPlain
	syncer := newGossiperSyncer(gossipSyncerCfg{
		chainHash:       d.cfg.ChainHash,
		syncChanUpdates: recvUpdates,
		channelSeries:   d.cfg.ChanSeries,
		encodingType:    encoding,
		chunkSize:       encodingTypeToChunkSize[encoding],
		sendToPeer: func(msgs ...lnwire.Message) error {
			return syncPeer.SendMessage(false, msgs...)
		},
	})
	copy(syncer.peerPub[:], nodeID[:])
	d.peerSyncers[nodeID] = syncer

	syncer.Start()
}

// PruneSyncState is called by outside sub-systems once a peer that we were
// previously connected to has been disconnected. In this case we can stop the
// existing gossipSyncer assigned to the peer and free up resources.
func (d *AuthenticatedGossiper) PruneSyncState(peer *btcec.PublicKey) {
	d.syncerMtx.Lock()
	defer d.syncerMtx.Unlock()

	log.Infof("Removing gossipSyncer for peer=%x",
		peer.SerializeCompressed())

	vertex := routing.NewVertex(peer)
	syncer, ok := d.peerSyncers[vertex]
	if !ok {
		return
	}

	syncer.Stop()

	delete(d.peerSyncers, vertex)

	return
}

// isRecentlyRejectedMsg returns true if we recently rejected a message, and
// false otherwise, This avoids expensive reprocessing of the message.
func (d *AuthenticatedGossiper) isRecentlyRejectedMsg(msg lnwire.Message) bool {
	d.rejectMtx.RLock()
	defer d.rejectMtx.RUnlock()

	switch m := msg.(type) {
	case *lnwire.ChannelUpdate:
		_, ok := d.recentRejects[m.ShortChannelID.ToUint64()]
		return ok

	case *lnwire.ChannelAnnouncement:
		_, ok := d.recentRejects[m.ShortChannelID.ToUint64()]
		return ok

	default:
		return false
	}
}

// retransmitStaleChannels examines all outgoing channels that the source node
// is known to maintain to check to see if any of them are "stale". A channel
// is stale iff, the last timestamp of its rebroadcast is older then
// broadcastInterval.
func (d *AuthenticatedGossiper) retransmitStaleChannels() error {
	// Iterate over all of our channels and check if any of them fall
	// within the prune interval or re-broadcast interval.
	type updateTuple struct {
		info *channeldb.ChannelEdgeInfo
		edge *channeldb.ChannelEdgePolicy
	}
	var edgesToUpdate []updateTuple
	err := d.cfg.Router.ForAllOutgoingChannels(func(
		info *channeldb.ChannelEdgeInfo,
		edge *channeldb.ChannelEdgePolicy) error {

		// If there's no auth proof attached to this edge, it means
		// that it is a private channel not meant to be announced to
		// the greater network, so avoid sending channel updates for
		// this channel to not leak its
		// existence.
		if info.AuthProof == nil {
			log.Debugf("Skipping retransmission of channel "+
				"without AuthProof: %v", info.ChannelID)
			return nil
		}

		const broadcastInterval = time.Hour * 24

		timeElapsed := time.Since(edge.LastUpdate)

		// If it's been a full day since we've re-broadcasted the
		// channel, add the channel to the set of edges we need to
		// update.
		if timeElapsed >= broadcastInterval {
			edgesToUpdate = append(edgesToUpdate, updateTuple{
				info: info,
				edge: edge,
			})
		}

		return nil
	})
	if err != nil && err != channeldb.ErrGraphNoEdgesFound {
		return fmt.Errorf("unable to retrieve outgoing channels: %v",
			err)
	}

	var signedUpdates []lnwire.Message
	for _, chanToUpdate := range edgesToUpdate {
		// Re-sign and update the channel on disk and retrieve our
		// ChannelUpdate to broadcast.
		chanAnn, chanUpdate, err := d.updateChannel(
			chanToUpdate.info, chanToUpdate.edge,
		)
		if err != nil {
			return fmt.Errorf("unable to update channel: %v", err)
		}

		// If we have a valid announcement to transmit, then we'll send
		// that along with the update.
		if chanAnn != nil {
			signedUpdates = append(signedUpdates, chanAnn)
		}

		signedUpdates = append(signedUpdates, chanUpdate)
	}

	// If we don't have any channels to re-broadcast, then we'll exit
	// early.
	if len(signedUpdates) == 0 {
		return nil
	}

	log.Infof("Retransmitting %v outgoing channels", len(edgesToUpdate))

	// With all the wire announcements properly crafted, we'll broadcast
	// our known outgoing channels to all our immediate peers.
	if err := d.cfg.Broadcast(nil, signedUpdates...); err != nil {
		return fmt.Errorf("unable to re-broadcast channels: %v", err)
	}

	return nil
}

// processChanPolicyUpdate generates a new set of channel updates with the new
// channel policy applied for each specified channel identified by its channel
// point. In the case that no channel points are specified, then the update
// will be applied to all channels. Finally, the backing ChannelGraphSource is
// updated with the latest information reflecting the applied updates.
//
// TODO(roasbeef): generalize into generic for any channel update
func (d *AuthenticatedGossiper) processChanPolicyUpdate(
	policyUpdate *chanPolicyUpdateRequest) ([]networkMsg, error) {
	// First, we'll construct a set of all the channels that need to be
	// updated.
	chansToUpdate := make(map[wire.OutPoint]struct{})
	for _, chanPoint := range policyUpdate.targetChans {
		chansToUpdate[chanPoint] = struct{}{}
	}

	haveChanFilter := len(chansToUpdate) != 0
	if haveChanFilter {
		log.Infof("Updating routing policies for chan_points=%v",
			spew.Sdump(chansToUpdate))
	} else {
		log.Infof("Updating routing policies for all chans")
	}

	type edgeWithInfo struct {
		info *channeldb.ChannelEdgeInfo
		edge *channeldb.ChannelEdgePolicy
	}
	var edgesToUpdate []edgeWithInfo

	// Next, we'll loop over all the outgoing channels the router knows of.
	// If we have a filter then we'll only collected those channels,
	// otherwise we'll collect them all.
	err := d.cfg.Router.ForAllOutgoingChannels(func(
		info *channeldb.ChannelEdgeInfo,
		edge *channeldb.ChannelEdgePolicy) error {

		// If we have a channel filter, and this channel isn't a part
		// of it, then we'll skip it.
		if _, ok := chansToUpdate[info.ChannelPoint]; !ok && haveChanFilter {
			return nil
		}

		// Now that we know we should update this channel, we'll update
		// its set of policies.
		edge.FeeBaseMSat = policyUpdate.newSchema.BaseFee
		edge.FeeProportionalMillionths = lnwire.MilliSatoshi(
			policyUpdate.newSchema.FeeRate,
		)
		edge.TimeLockDelta = uint16(policyUpdate.newSchema.TimeLockDelta)

		edgesToUpdate = append(edgesToUpdate, edgeWithInfo{
			info: info,
			edge: edge,
		})

		return nil
	})
	if err != nil {
		return nil, err
	}

	// With the set of edges we need to update retrieved, we'll now re-sign
	// them, and insert them into the database.
	var chanUpdates []networkMsg
	for _, edgeInfo := range edgesToUpdate {
		// Now that we've collected all the channels we need to update,
		// we'll Re-sign and update the backing ChannelGraphSource, and
		// retrieve our ChannelUpdate to broadcast.
		_, chanUpdate, err := d.updateChannel(
			edgeInfo.info, edgeInfo.edge,
		)
		if err != nil {
			return nil, err
		}

		// We set ourselves as the source of this message to indicate
		// that we shouldn't skip any peers when sending this message.
		chanUpdates = append(chanUpdates, networkMsg{
			source: d.selfKey,
			msg:    chanUpdate,
		})
	}

	return chanUpdates, nil
}

// processRejectedEdge examines a rejected edge to see if we can extract any
// new announcements from it.  An edge will get rejected if we already added
// the same edge without AuthProof to the graph. If the received announcement
// contains a proof, we can add this proof to our edge.  We can end up in this
// situation in the case where we create a channel, but for some reason fail
// to receive the remote peer's proof, while the remote peer is able to fully
// assemble the proof and craft the ChannelAnnouncement.
func (d *AuthenticatedGossiper) processRejectedEdge(
	chanAnnMsg *lnwire.ChannelAnnouncement,
	proof *channeldb.ChannelAuthProof) ([]networkMsg, error) {

	// First, we'll fetch the state of the channel as we know if from the
	// database.
	chanInfo, e1, e2, err := d.cfg.Router.GetChannelByID(
		chanAnnMsg.ShortChannelID,
	)
	if err != nil {
		return nil, err
	}

	// The edge is in the graph, and has a proof attached, then we'll just
	// reject it as normal.
	if chanInfo.AuthProof != nil {
		return nil, nil
	}

	// Otherwise, this means that the edge is within the graph, but it
	// doesn't yet have a proper proof attached. If we did not receive
	// the proof such that we now can add it, there's nothing more we
	// can do.
	if proof == nil {
		return nil, nil
	}

	// We'll then create then validate the new fully assembled
	// announcement.
	chanAnn, e1Ann, e2Ann, err := CreateChanAnnouncement(
		proof, chanInfo, e1, e2,
	)
	if err != nil {
		return nil, err
	}
	err = routing.ValidateChannelAnn(chanAnn)
	if err != nil {
		err := fmt.Errorf("assembled channel announcement proof "+
			"for shortChanID=%v isn't valid: %v",
			chanAnnMsg.ShortChannelID, err)
		log.Error(err)
		return nil, err
	}

	// If everything checks out, then we'll add the fully assembled proof
	// to the database.
	err = d.cfg.Router.AddProof(chanAnnMsg.ShortChannelID, proof)
	if err != nil {
		err := fmt.Errorf("unable add proof to shortChanID=%v: %v",
			chanAnnMsg.ShortChannelID, err)
		log.Error(err)
		return nil, err
	}

	// As we now have a complete channel announcement for this channel,
	// we'll construct the announcement so they can be broadcast out to all
	// our peers.
	announcements := make([]networkMsg, 0, 3)
	announcements = append(announcements, networkMsg{
		source: d.selfKey,
		msg:    chanAnn,
	})
	if e1Ann != nil {
		announcements = append(announcements, networkMsg{
			source: d.selfKey,
			msg:    e1Ann,
		})
	}
	if e2Ann != nil {
		announcements = append(announcements, networkMsg{
			source: d.selfKey,
			msg:    e2Ann,
		})

	}

	return announcements, nil
}

// processNetworkAnnouncement processes a new network relate authenticated
// channel or node announcement or announcements proofs. If the announcement
// didn't affect the internal state due to either being out of date, invalid,
// or redundant, then nil is returned. Otherwise, the set of announcements will
// be returned which should be broadcasted to the rest of the network.
func (d *AuthenticatedGossiper) processNetworkAnnouncement(
	nMsg *networkMsg) []networkMsg {

	isPremature := func(chanID lnwire.ShortChannelID, delta uint32) bool {
		// TODO(roasbeef) make height delta 6
		//  * or configurable
		bestHeight := atomic.LoadUint32(&d.bestHeight)
		return chanID.BlockHeight+delta > bestHeight
	}

	var announcements []networkMsg

	switch msg := nMsg.msg.(type) {

	// A new node announcement has arrived which either presents new
	// information about a node in one of the channels we know about, or a
	// updating previously advertised information.
	case *lnwire.NodeAnnouncement:
		timestamp := time.Unix(int64(msg.Timestamp), 0)

		if nMsg.isRemote {
			// We'll quickly ask the router if it already has a
			// newer update for this node so we can skip validating
			// signatures if not required.
			if d.cfg.Router.IsStaleNode(msg.NodeID, timestamp) {
				nMsg.err <- nil
				return nil
			}

			if err := routing.ValidateNodeAnn(msg); err != nil {
				err := fmt.Errorf("unable to validate "+
					"node announcement: %v", err)
				log.Error(err)
				nMsg.err <- err
				return nil
			}
		}

		features := lnwire.NewFeatureVector(
			msg.Features, lnwire.GlobalFeatures,
		)
		node := &channeldb.LightningNode{
			HaveNodeAnnouncement: true,
			LastUpdate:           timestamp,
			Addresses:            msg.Addresses,
			PubKeyBytes:          msg.NodeID,
			Alias:                msg.Alias.String(),
			AuthSigBytes:         msg.Signature.ToSignatureBytes(),
			Features:             features,
			Color:                msg.RGBColor,
			ExtraOpaqueData:      msg.ExtraOpaqueData,
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

		// In order to ensure we don't leak unadvertised nodes, we'll
		// make a quick check to ensure this node intends to publicly
		// advertise itself to the network.
		isPublic, err := d.cfg.Router.IsPublicNode(node.PubKeyBytes)
		if err != nil {
			log.Errorf("Unable to determine if node %x is "+
				"advertised: %v", node.PubKeyBytes, err)
			nMsg.err <- err
			return nil
		}

		// If it does, we'll add their announcement to our batch so that
		// it can be broadcast to the rest of our peers.
		if isPublic {
			announcements = append(announcements, networkMsg{
				peer:   nMsg.peer,
				source: nMsg.source,
				msg:    msg,
			})
		}

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
			err := fmt.Errorf("Ignoring ChannelAnnouncement from "+
				"chain=%v, gossiper on chain=%v", msg.ChainHash,
				d.cfg.ChainHash)
			log.Errorf(err.Error())

			d.rejectMtx.Lock()
			d.recentRejects[msg.ShortChannelID.ToUint64()] = struct{}{}
			d.rejectMtx.Unlock()

			nMsg.err <- err
			return nil
		}

		// If the advertised inclusionary block is beyond our knowledge
		// of the chain tip, then we'll put the announcement in limbo
		// to be fully verified once we advance forward in the chain.
		if nMsg.isRemote && isPremature(msg.ShortChannelID, 0) {
			blockHeight := msg.ShortChannelID.BlockHeight
			log.Infof("Announcement for chan_id=(%v), is "+
				"premature: advertises height %v, only "+
				"height %v is known",
				msg.ShortChannelID.ToUint64(),
				msg.ShortChannelID.BlockHeight,
				atomic.LoadUint32(&d.bestHeight))

			d.Lock()
			d.prematureAnnouncements[blockHeight] = append(
				d.prematureAnnouncements[blockHeight],
				nMsg,
			)
			d.Unlock()
			return nil
		}

		// At this point, we'll now ask the router if this is a stale
		// update. If so we can skip all the processing below.
		if d.cfg.Router.IsKnownEdge(msg.ShortChannelID) {
			nMsg.err <- nil
			return nil
		}

		// If this is a remote channel announcement, then we'll validate
		// all the signatures within the proof as it should be well
		// formed.
		var proof *channeldb.ChannelAuthProof
		if nMsg.isRemote {
			if err := routing.ValidateChannelAnn(msg); err != nil {
				err := fmt.Errorf("unable to validate "+
					"announcement: %v", err)
				d.rejectMtx.Lock()
				d.recentRejects[msg.ShortChannelID.ToUint64()] = struct{}{}
				d.rejectMtx.Unlock()

				log.Error(err)
				nMsg.err <- err
				return nil
			}

			// If the proof checks out, then we'll save the proof
			// itself to the database so we can fetch it later when
			// gossiping with other nodes.
			proof = &channeldb.ChannelAuthProof{
				NodeSig1Bytes:    msg.NodeSig1.ToSignatureBytes(),
				NodeSig2Bytes:    msg.NodeSig2.ToSignatureBytes(),
				BitcoinSig1Bytes: msg.BitcoinSig1.ToSignatureBytes(),
				BitcoinSig2Bytes: msg.BitcoinSig2.ToSignatureBytes(),
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
			ChannelID:        msg.ShortChannelID.ToUint64(),
			ChainHash:        msg.ChainHash,
			NodeKey1Bytes:    msg.NodeID1,
			NodeKey2Bytes:    msg.NodeID2,
			BitcoinKey1Bytes: msg.BitcoinKey1,
			BitcoinKey2Bytes: msg.BitcoinKey2,
			AuthProof:        proof,
			Features:         featureBuf.Bytes(),
			ExtraOpaqueData:  msg.ExtraOpaqueData,
		}

		// We will add the edge to the channel router. If the nodes
		// present in this channel are not present in the database, a
		// partial node will be added to represent each node while we
		// wait for a node announcement.
		//
		// Before we add the edge to the database, we obtain
		// the mutex for this channel ID. We do this to ensure
		// no other goroutine has read the database and is now
		// making decisions based on this DB state, before it
		// writes to the DB.
		d.channelMtx.Lock(msg.ShortChannelID.ToUint64())
		defer d.channelMtx.Unlock(msg.ShortChannelID.ToUint64())
		if err := d.cfg.Router.AddEdge(edge); err != nil {
			// If the edge was rejected due to already being known,
			// then it may be that case that this new message has a
			// fresh channel proof, so we'll check.
			if routing.IsError(err, routing.ErrOutdated,
				routing.ErrIgnored) {

				// Attempt to process the rejected message to
				// see if we get any new announcements.
				anns, rErr := d.processRejectedEdge(msg, proof)
				if rErr != nil {
					d.rejectMtx.Lock()
					d.recentRejects[msg.ShortChannelID.ToUint64()] = struct{}{}
					d.rejectMtx.Unlock()
					nMsg.err <- rErr
					return nil
				}

				// If while processing this rejected edge, we
				// realized there's a set of announcements we
				// could extract, then we'll return those
				// directly.
				if len(anns) != 0 {
					nMsg.err <- nil
					return anns
				}

				// Otherwise, this is just a regular rejected
				// edge.
				log.Debugf("Router rejected channel "+
					"edge: %v", err)
			} else {
				log.Tracef("Router rejected channel "+
					"edge: %v", err)
			}

			nMsg.err <- err
			return nil
		}

		// If we earlier received any ChannelUpdates for this channel,
		// we can now process them, as the channel is added to the
		// graph.
		shortChanID := msg.ShortChannelID.ToUint64()
		var channelUpdates []*networkMsg

		d.pChanUpdMtx.Lock()
		for _, cu := range d.prematureChannelUpdates[shortChanID] {
			channelUpdates = append(channelUpdates, cu)
		}

		// Now delete the premature ChannelUpdates, since we added them
		// all to the queue of network messages.
		delete(d.prematureChannelUpdates, shortChanID)
		d.pChanUpdMtx.Unlock()

		// Launch a new goroutine to handle each ChannelUpdate, this to
		// ensure we don't block here, as we can handle only one
		// announcement at a time.
		for _, cu := range channelUpdates {
			d.wg.Add(1)
			go func(nMsg *networkMsg) {
				defer d.wg.Done()

				switch msg := nMsg.msg.(type) {

				// Reprocess the message, making sure we return
				// an error to the original caller in case the
				// gossiper shuts down.
				case *lnwire.ChannelUpdate:
					log.Debugf("Reprocessing"+
						" ChannelUpdate for "+
						"shortChanID=%v",
						msg.ShortChannelID.ToUint64())

					select {
					case d.networkMsgs <- nMsg:
					case <-d.quit:
						nMsg.err <- ErrGossiperShuttingDown
					}

				// We don't expect any other message type than
				// ChannelUpdate to be in this map.
				default:
					log.Errorf("Unsupported message type "+
						"found among ChannelUpdates: "+
						"%T", msg)
				}
			}(cu)
		}

		// Channel announcement was successfully proceeded and know it
		// might be broadcast to other connected nodes if it was
		// announcement with proof (remote).
		if proof != nil {
			announcements = append(announcements, networkMsg{
				peer:   nMsg.peer,
				source: nMsg.source,
				msg:    msg,
			})
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
			err := fmt.Errorf("Ignoring ChannelUpdate from "+
				"chain=%v, gossiper on chain=%v", msg.ChainHash,
				d.cfg.ChainHash)
			log.Errorf(err.Error())

			d.rejectMtx.Lock()
			d.recentRejects[msg.ShortChannelID.ToUint64()] = struct{}{}
			d.rejectMtx.Unlock()

			nMsg.err <- err
			return nil
		}

		blockHeight := msg.ShortChannelID.BlockHeight
		shortChanID := msg.ShortChannelID.ToUint64()

		// If the advertised inclusionary block is beyond our knowledge
		// of the chain tip, then we'll put the announcement in limbo
		// to be fully verified once we advance forward in the chain.
		if nMsg.isRemote && isPremature(msg.ShortChannelID, 0) {
			log.Infof("Update announcement for "+
				"short_chan_id(%v), is premature: advertises "+
				"height %v, only height %v is known",
				shortChanID, blockHeight,
				atomic.LoadUint32(&d.bestHeight))

			d.Lock()
			d.prematureAnnouncements[blockHeight] = append(
				d.prematureAnnouncements[blockHeight],
				nMsg,
			)
			d.Unlock()
			return nil
		}

		// Before we perform any of the expensive checks below, we'll
		// make sure that the router doesn't already have a fresher
		// announcement for this edge.
		timestamp := time.Unix(int64(msg.Timestamp), 0)
		if d.cfg.Router.IsStaleEdgePolicy(
			msg.ShortChannelID, timestamp, msg.Flags,
		) {

			nMsg.err <- nil
			return nil
		}

		// Get the node pub key as far as we don't have it in channel
		// update announcement message. We'll need this to properly
		// verify message signature.
		//
		// We make sure to obtain the mutex for this channel ID
		// before we access the database. This ensures the state
		// we read from the database has not changed between this
		// point and when we call UpdateEdge() later.
		d.channelMtx.Lock(msg.ShortChannelID.ToUint64())
		defer d.channelMtx.Unlock(msg.ShortChannelID.ToUint64())
		chanInfo, _, _, err := d.cfg.Router.GetChannelByID(msg.ShortChannelID)
		if err != nil {
			switch err {
			case channeldb.ErrGraphNotFound:
				fallthrough
			case channeldb.ErrGraphNoEdgesFound:
				fallthrough
			case channeldb.ErrEdgeNotFound:
				// If the edge corresponding to this
				// ChannelUpdate was not found in the graph,
				// this might be a channel in the process of
				// being opened, and we haven't processed our
				// own ChannelAnnouncement yet, hence it is not
				// found in the graph. This usually gets
				// resolved after the channel proofs are
				// exchanged and the channel is broadcasted to
				// the rest of the network, but in case this
				// is a private channel this won't ever happen.
				// Because of this, we temporarily add it to a
				// map, and reprocess it after our own
				// ChannelAnnouncement has been processed.
				d.pChanUpdMtx.Lock()
				d.prematureChannelUpdates[shortChanID] = append(
					d.prematureChannelUpdates[shortChanID],
					nMsg,
				)
				d.pChanUpdMtx.Unlock()

				log.Debugf("Got ChannelUpdate for edge not "+
					"found in graph(shortChanID=%v), "+
					"saving for reprocessing later",
					shortChanID)

				// NOTE: We don't return anything on the error
				// channel for this message, as we expect that
				// will be done when this ChannelUpdate is
				// later reprocessed.
				return nil

			default:
				err := fmt.Errorf("unable to validate "+
					"channel update short_chan_id=%v: %v",
					shortChanID, err)
				log.Error(err)
				nMsg.err <- err

				d.rejectMtx.Lock()
				d.recentRejects[msg.ShortChannelID.ToUint64()] = struct{}{}
				d.rejectMtx.Unlock()
				return nil
			}
		}

		// The least-significant bit in the flag on the channel update
		// announcement tells us "which" side of the channels directed
		// edge is being updated.
		var pubKey *btcec.PublicKey
		switch {
		case msg.Flags&lnwire.ChanUpdateDirection == 0:
			pubKey, _ = chanInfo.NodeKey1()
		case msg.Flags&lnwire.ChanUpdateDirection == 1:
			pubKey, _ = chanInfo.NodeKey2()
		}

		// Validate the channel announcement with the expected public
		// key, In the case of an invalid channel , we'll return an
		// error to the caller and exit early.
		if err := routing.ValidateChannelUpdateAnn(pubKey, msg); err != nil {
			rErr := fmt.Errorf("unable to validate channel "+
				"update announcement for short_chan_id=%v: %v",
				spew.Sdump(msg.ShortChannelID), err)

			log.Error(rErr)
			nMsg.err <- rErr
			return nil
		}

		update := &channeldb.ChannelEdgePolicy{
			SigBytes:                  msg.Signature.ToSignatureBytes(),
			ChannelID:                 shortChanID,
			LastUpdate:                timestamp,
			Flags:                     msg.Flags,
			TimeLockDelta:             msg.TimeLockDelta,
			MinHTLC:                   msg.HtlcMinimumMsat,
			FeeBaseMSat:               lnwire.MilliSatoshi(msg.BaseFee),
			FeeProportionalMillionths: lnwire.MilliSatoshi(msg.FeeRate),
			ExtraOpaqueData:           msg.ExtraOpaqueData,
		}

		if err := d.cfg.Router.UpdateEdge(update); err != nil {
			if routing.IsError(err, routing.ErrOutdated,
				routing.ErrIgnored) {
				log.Debug(err)
			} else {
				d.rejectMtx.Lock()
				d.recentRejects[msg.ShortChannelID.ToUint64()] = struct{}{}
				d.rejectMtx.Unlock()
				log.Error(err)
			}

			nMsg.err <- err
			return nil
		}

		// If this is a local ChannelUpdate without an AuthProof, it
		// means it is an update to a channel that is not (yet)
		// supposed to be announced to the greater network. However,
		// our channel counter party will need to be given the update,
		// so we'll try sending the update directly to the remote peer.
		if !nMsg.isRemote && chanInfo.AuthProof == nil {
			// Get our peer's public key.
			var remotePub *btcec.PublicKey
			switch {
			case msg.Flags&lnwire.ChanUpdateDirection == 0:
				remotePub, _ = chanInfo.NodeKey2()
			case msg.Flags&lnwire.ChanUpdateDirection == 1:
				remotePub, _ = chanInfo.NodeKey1()
			}

			sPeer, err := d.cfg.FindPeer(remotePub)
			if err != nil {
				log.Errorf("unable to send channel update -- "+
					"could not find peer %x: %v",
					remotePub.SerializeCompressed(),
					err)
			} else {
				// Send ChannelUpdate directly to remotePeer.
				// TODO(halseth): make reliable send?
				err = sPeer.SendMessage(false, msg)
				if err != nil {
					log.Errorf("unable to send channel "+
						"update message to peer %x: %v",
						remotePub.SerializeCompressed(),
						err)
				}
			}
		}

		// Channel update announcement was successfully processed and
		// now it can be broadcast to the rest of the network. However,
		// we'll only broadcast the channel update announcement if it
		// has an attached authentication proof.
		if chanInfo.AuthProof != nil {
			announcements = append(announcements, networkMsg{
				peer:   nMsg.peer,
				source: nMsg.source,
				msg:    msg,
			})
		}

		nMsg.err <- nil
		return announcements

	// A new signature announcement has been received. This indicates
	// willingness of nodes involved in the funding of a channel to
	// announce this new channel to the rest of the world.
	case *lnwire.AnnounceSignatures:
		needBlockHeight := msg.ShortChannelID.BlockHeight +
			d.cfg.ProofMatureDelta
		shortChanID := msg.ShortChannelID.ToUint64()

		prefix := "local"
		if nMsg.isRemote {
			prefix = "remote"
		}

		log.Infof("Received new %v channel announcement: %v", prefix,
			spew.Sdump(msg))

		// By the specification, channel announcement proofs should be
		// sent after some number of confirmations after channel was
		// registered in bitcoin blockchain. Therefore, we check if the
		// proof is premature.  If so we'll halt processing until the
		// expected announcement height.  This allows us to be tolerant
		// to other clients if this constraint was changed.
		if isPremature(msg.ShortChannelID, d.cfg.ProofMatureDelta) {
			d.Lock()
			d.prematureAnnouncements[needBlockHeight] = append(
				d.prematureAnnouncements[needBlockHeight],
				nMsg,
			)
			d.Unlock()
			log.Infof("Premature proof announcement, "+
				"current block height lower than needed: %v <"+
				" %v, add announcement to reprocessing batch",
				atomic.LoadUint32(&d.bestHeight), needBlockHeight)
			return nil
		}

		// Ensure that we know of a channel with the target channel ID
		// before proceeding further.
		//
		// We must acquire the mutex for this channel ID before getting
		// the channel from the database, to ensure what we read does
		// not change before we call AddProof() later.
		d.channelMtx.Lock(msg.ShortChannelID.ToUint64())
		defer d.channelMtx.Unlock(msg.ShortChannelID.ToUint64())

		chanInfo, e1, e2, err := d.cfg.Router.GetChannelByID(
			msg.ShortChannelID)
		if err != nil {
			// TODO(andrew.shvv) this is dangerous because remote
			// node might rewrite the waiting proof.
			proof := channeldb.NewWaitingProof(nMsg.isRemote, msg)
			if err := d.waitingProofs.Add(proof); err != nil {
				err := fmt.Errorf("unable to store "+
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

		nodeID := nMsg.source.SerializeCompressed()
		isFirstNode := bytes.Equal(nodeID, chanInfo.NodeKey1Bytes[:])
		isSecondNode := bytes.Equal(nodeID, chanInfo.NodeKey2Bytes[:])

		// Ensure that channel that was retrieved belongs to the peer
		// which sent the proof announcement.
		if !(isFirstNode || isSecondNode) {
			err := fmt.Errorf("channel that was received not "+
				"belongs to the peer which sent the proof, "+
				"short_chan_id=%v", shortChanID)
			log.Error(err)
			nMsg.err <- err
			return nil
		}

		// If proof was sent by a local sub-system, then we'll
		// send the announcement signature to the remote node
		// so they can also reconstruct the full channel
		// announcement.
		if !nMsg.isRemote {
			var remotePeer *btcec.PublicKey
			if isFirstNode {
				remotePeer, _ = chanInfo.NodeKey2()
			} else {
				remotePeer, _ = chanInfo.NodeKey1()
			}
			// Since the remote peer might not be online
			// we'll call a method that will attempt to
			// deliver the proof when it comes online.
			err := d.sendAnnSigReliably(msg, remotePeer)
			if err != nil {
				err := fmt.Errorf("unable to send reliably "+
					"to remote for short_chan_id=%v: %v",
					shortChanID, err)
				log.Error(err)
				nMsg.err <- err
				return nil
			}
		}

		// Check if we already have the full proof for this channel.
		if chanInfo.AuthProof != nil {
			// If we already have the fully assembled proof, then
			// the peer sending us their proof has probably not
			// received our local proof yet. So be kind and send
			// them the full proof.
			if nMsg.isRemote {
				peerID := nMsg.source.SerializeCompressed()
				log.Debugf("Got AnnounceSignatures for " +
					"channel with full proof.")

				d.wg.Add(1)
				go func() {
					defer d.wg.Done()
					log.Debugf("Received half proof for "+
						"channel %v with existing "+
						"full proof. Sending full "+
						"proof to peer=%x",
						msg.ChannelID,
						peerID)

					chanAnn, _, _, err := CreateChanAnnouncement(
						chanInfo.AuthProof, chanInfo,
						e1, e2,
					)
					if err != nil {
						log.Errorf("unable to gen "+
							"ann: %v", err)
						return
					}
					err = nMsg.peer.SendMessage(
						false, chanAnn,
					)
					if err != nil {
						log.Errorf("Failed sending "+
							"full proof to "+
							"peer=%x: %v",
							peerID, err)
						return
					}
					log.Debugf("Full proof sent to peer=%x"+
						" for chanID=%v", peerID,
						msg.ChannelID)
				}()
			}

			log.Debugf("Already have proof for channel "+
				"with chanID=%v", msg.ChannelID)
			nMsg.err <- nil
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
			err := fmt.Errorf("unable to get "+
				"the opposite proof for short_chan_id=%v: %v",
				shortChanID, err)
			log.Error(err)
			nMsg.err <- err
			return nil
		}

		if err == channeldb.ErrWaitingProofNotFound {
			if err := d.waitingProofs.Add(proof); err != nil {
				err := fmt.Errorf("unable to store "+
					"the proof for short_chan_id=%v: %v",
					shortChanID, err)
				log.Error(err)
				nMsg.err <- err
				return nil
			}

			log.Infof("1/2 of channel ann proof received for "+
				"short_chan_id=%v, waiting for other half",
				shortChanID)

			nMsg.err <- nil
			return nil
		}

		// We now have both halves of the channel announcement proof,
		// then we'll reconstruct the initial announcement so we can
		// validate it shortly below.
		var dbProof channeldb.ChannelAuthProof
		if isFirstNode {
			dbProof.NodeSig1Bytes = msg.NodeSignature.ToSignatureBytes()
			dbProof.NodeSig2Bytes = oppositeProof.NodeSignature.ToSignatureBytes()
			dbProof.BitcoinSig1Bytes = msg.BitcoinSignature.ToSignatureBytes()
			dbProof.BitcoinSig2Bytes = oppositeProof.BitcoinSignature.ToSignatureBytes()
		} else {
			dbProof.NodeSig1Bytes = oppositeProof.NodeSignature.ToSignatureBytes()
			dbProof.NodeSig2Bytes = msg.NodeSignature.ToSignatureBytes()
			dbProof.BitcoinSig1Bytes = oppositeProof.BitcoinSignature.ToSignatureBytes()
			dbProof.BitcoinSig2Bytes = msg.BitcoinSignature.ToSignatureBytes()
		}
		chanAnn, e1Ann, e2Ann, err := CreateChanAnnouncement(
			&dbProof, chanInfo, e1, e2,
		)
		if err != nil {
			log.Error(err)
			nMsg.err <- err
			return nil
		}

		// With all the necessary components assembled validate the
		// full channel announcement proof.
		if err := routing.ValidateChannelAnn(chanAnn); err != nil {
			err := fmt.Errorf("channel  announcement proof "+
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
			err := fmt.Errorf("unable add proof to the "+
				"channel chanID=%v: %v", msg.ChannelID, err)
			log.Error(err)
			nMsg.err <- err
			return nil
		}

		err = d.waitingProofs.Remove(proof.OppositeKey())
		if err != nil {
			err := fmt.Errorf("unable remove opposite proof "+
				"for the channel with chanID=%v: %v",
				msg.ChannelID, err)
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
		announcements = append(announcements, networkMsg{
			peer:   nMsg.peer,
			source: nMsg.source,
			msg:    chanAnn,
		})
		if e1Ann != nil {
			announcements = append(announcements, networkMsg{
				peer:   nMsg.peer,
				source: nMsg.source,
				msg:    e1Ann,
			})
		}
		if e2Ann != nil {
			announcements = append(announcements, networkMsg{
				peer:   nMsg.peer,
				source: nMsg.source,
				msg:    e2Ann,
			})
		}

		nMsg.err <- nil
		return announcements

	default:
		nMsg.err <- errors.New("wrong type of the announcement")
		return nil
	}
}

// sendAnnSigReliably will try to send the provided local AnnounceSignatures
// to the remote peer, waiting for it to come online if necessary. This
// method returns after adding the message to persistent storage, such
// that the caller knows that the message will be delivered at one point.
func (d *AuthenticatedGossiper) sendAnnSigReliably(
	msg *lnwire.AnnounceSignatures, remotePeer *btcec.PublicKey) error {

	// We first add this message to the database, such that in case
	// we do not succeed in sending it to the peer, we'll fetch it
	// from the DB next time we start, and retry. We use the peer ID
	// + shortChannelID as key, as there possibly is more than one
	// channel opening in progress to the same peer.
	var key [41]byte
	copy(key[:33], remotePeer.SerializeCompressed())
	binary.BigEndian.PutUint64(key[33:], msg.ShortChannelID.ToUint64())

	err := d.cfg.DB.Update(func(tx *bolt.Tx) error {
		bucket, err := tx.CreateBucketIfNotExists(messageStoreKey)
		if err != nil {
			return err
		}

		// Encode the AnnounceSignatures message.
		var b bytes.Buffer
		if err := msg.Encode(&b, 0); err != nil {
			return err
		}

		// Add the encoded message to the database using the peer
		// + shortChanID as key.
		return bucket.Put(key[:], b.Bytes())

	})
	if err != nil {
		return err
	}

	// We have succeeded adding the message to the database. We now launch
	// a goroutine that will keep on trying sending the message to the
	// remote peer until it succeeds, or the gossiper shuts down. In case
	// of success, the message will be removed from the database.
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		for {
			log.Debugf("Sending AnnounceSignatures for channel "+
				"%v to remote peer %x", msg.ChannelID,
				remotePeer.SerializeCompressed())
			err := d.cfg.SendToPeer(remotePeer, msg)
			if err == nil {
				// Sending succeeded, we can
				// continue the flow.
				break
			}

			log.Errorf("unable to send AnnounceSignatures message "+
				"to peer(%x): %v. Will retry when online.",
				remotePeer.SerializeCompressed(), err)

			peerChan := make(chan lnpeer.Peer, 1)
			d.cfg.NotifyWhenOnline(remotePeer, peerChan)

			select {
			case <-peerChan:
				// Retry sending.
				log.Infof("Peer %x reconnected. Retry sending"+
					" AnnounceSignatures.",
					remotePeer.SerializeCompressed())

			case <-d.quit:
				log.Infof("Gossiper shutting down, did not " +
					"send AnnounceSignatures.")
				return
			}
		}

		log.Infof("Sent channel announcement proof to remote peer: %x",
			remotePeer.SerializeCompressed())
	}()

	// This method returns after the message has been added to the database,
	// such that the caller don't have to wait until the message is actually
	// delivered, but can be assured that it will be delivered eventually
	// when this method returns.
	return nil
}

// updateChannel creates a new fully signed update for the channel, and updates
// the underlying graph with the new state.
func (d *AuthenticatedGossiper) updateChannel(info *channeldb.ChannelEdgeInfo,
	edge *channeldb.ChannelEdgePolicy) (*lnwire.ChannelAnnouncement,
	*lnwire.ChannelUpdate, error) {

	var err error

	// Make sure timestamp is always increased, such that our update gets
	// propagated.
	timestamp := time.Now().Unix()
	if timestamp <= edge.LastUpdate.Unix() {
		timestamp = edge.LastUpdate.Unix() + 1
	}
	edge.LastUpdate = time.Unix(timestamp, 0)
	chanUpdate := &lnwire.ChannelUpdate{
		ChainHash:       info.ChainHash,
		ShortChannelID:  lnwire.NewShortChanIDFromInt(edge.ChannelID),
		Timestamp:       uint32(timestamp),
		Flags:           edge.Flags,
		TimeLockDelta:   edge.TimeLockDelta,
		HtlcMinimumMsat: edge.MinHTLC,
		BaseFee:         uint32(edge.FeeBaseMSat),
		FeeRate:         uint32(edge.FeeProportionalMillionths),
		ExtraOpaqueData: edge.ExtraOpaqueData,
	}
	chanUpdate.Signature, err = lnwire.NewSigFromRawSignature(edge.SigBytes)
	if err != nil {
		return nil, nil, err
	}

	// With the update applied, we'll generate a new signature over a
	// digest of the channel announcement itself.
	sig, err := SignAnnouncement(d.cfg.AnnSigner, d.selfKey, chanUpdate)
	if err != nil {
		return nil, nil, err
	}

	// Next, we'll set the new signature in place, and update the reference
	// in the backing slice.
	edge.SigBytes = sig.Serialize()
	chanUpdate.Signature, err = lnwire.NewSigFromSignature(sig)
	if err != nil {
		return nil, nil, err
	}

	// To ensure that our signature is valid, we'll verify it ourself
	// before committing it to the slice returned.
	err = routing.ValidateChannelUpdateAnn(d.selfKey, chanUpdate)
	if err != nil {
		return nil, nil, fmt.Errorf("generated invalid channel "+
			"update sig: %v", err)
	}

	// Finally, we'll write the new edge policy to disk.
	if err := d.cfg.Router.UpdateEdge(edge); err != nil {
		return nil, nil, err
	}

	// We'll also create the original channel announcement so the two can
	// be broadcast along side each other (if necessary), but only if we
	// have a full channel announcement for this channel.
	var chanAnn *lnwire.ChannelAnnouncement
	if info.AuthProof != nil {
		chanID := lnwire.NewShortChanIDFromInt(info.ChannelID)
		chanAnn = &lnwire.ChannelAnnouncement{
			ShortChannelID:  chanID,
			NodeID1:         info.NodeKey1Bytes,
			NodeID2:         info.NodeKey2Bytes,
			ChainHash:       info.ChainHash,
			BitcoinKey1:     info.BitcoinKey1Bytes,
			Features:        lnwire.NewRawFeatureVector(),
			BitcoinKey2:     info.BitcoinKey2Bytes,
			ExtraOpaqueData: edge.ExtraOpaqueData,
		}
		chanAnn.NodeSig1, err = lnwire.NewSigFromRawSignature(
			info.AuthProof.NodeSig1Bytes,
		)
		if err != nil {
			return nil, nil, err
		}
		chanAnn.NodeSig2, err = lnwire.NewSigFromRawSignature(
			info.AuthProof.NodeSig2Bytes,
		)
		if err != nil {
			return nil, nil, err
		}
		chanAnn.BitcoinSig1, err = lnwire.NewSigFromRawSignature(
			info.AuthProof.BitcoinSig1Bytes,
		)
		if err != nil {
			return nil, nil, err
		}
		chanAnn.BitcoinSig2, err = lnwire.NewSigFromRawSignature(
			info.AuthProof.BitcoinSig2Bytes,
		)
		if err != nil {
			return nil, nil, err
		}
	}

	return chanAnn, chanUpdate, err
}
