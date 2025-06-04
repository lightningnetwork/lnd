package discovery

import (
	"context"
	"sync"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnpeer"
	"github.com/lightningnetwork/lnd/lnwire"
)

// reliableSenderCfg contains all of necessary items for the reliableSender to
// carry out its duties.
type reliableSenderCfg struct {
	// NotifyWhenOnline is a function that allows the gossiper to be
	// notified when a certain peer comes online, allowing it to
	// retry sending a peer message.
	//
	// NOTE: The peerChan channel must be buffered.
	NotifyWhenOnline func(peerPubKey [33]byte, peerChan chan<- lnpeer.Peer)

	// NotifyWhenOffline is a function that allows the gossiper to be
	// notified when a certain peer disconnects, allowing it to request a
	// notification for when it reconnects.
	NotifyWhenOffline func(peerPubKey [33]byte) <-chan struct{}

	// MessageStore is a persistent storage of gossip messages which we will
	// use to determine which messages need to be resent for a given peer.
	MessageStore GossipMessageStore

	// IsMsgStale determines whether a message retrieved from the backing
	// MessageStore is seen as stale by the current graph.
	IsMsgStale func(context.Context, lnwire.Message) bool
}

// peerManager contains the set of channels required for the peerHandler to
// properly carry out its duties.
type peerManager struct {
	// msgs is the channel through which messages will be streamed to the
	// handler in order to send the message to the peer while they're
	// online.
	msgs chan lnwire.Message

	// done is a channel that will be closed to signal that the handler for
	// the given peer has been torn down for whatever reason.
	done chan struct{}
}

// reliableSender is a small subsystem of the gossiper used to reliably send
// gossip messages to peers.
type reliableSender struct {
	start sync.Once
	stop  sync.Once

	cfg reliableSenderCfg

	// activePeers keeps track of whether a peerHandler exists for a given
	// peer. A peerHandler is tasked with handling requests for messages
	// that should be reliably sent to peers while also taking into account
	// the peer's connection lifecycle.
	activePeers    map[[33]byte]peerManager
	activePeersMtx sync.Mutex

	wg     sync.WaitGroup
	quit   chan struct{}
	cancel fn.Option[context.CancelFunc]
}

// newReliableSender returns a new reliableSender backed by the given config.
func newReliableSender(cfg *reliableSenderCfg) *reliableSender {
	return &reliableSender{
		cfg:         *cfg,
		activePeers: make(map[[33]byte]peerManager),
		quit:        make(chan struct{}),
	}
}

// Start spawns message handlers for any peers with pending messages.
func (s *reliableSender) Start() error {
	var err error
	s.start.Do(func() {
		ctx, cancel := context.WithCancel(context.Background())
		s.cancel = fn.Some(cancel)

		err = s.resendPendingMsgs(ctx)
	})
	return err
}

// Stop halts the reliable sender from sending messages to peers.
func (s *reliableSender) Stop() {
	s.stop.Do(func() {
		log.Debugf("reliableSender is stopping")
		defer log.Debugf("reliableSender stopped")

		s.cancel.WhenSome(func(fn context.CancelFunc) { fn() })
		close(s.quit)
		s.wg.Wait()
	})
}

// sendMessage constructs a request to send a message reliably to a peer. In the
// event that the peer is currently offline, this will only write the message to
// disk. Once the peer reconnects, this message, along with any others pending,
// will be sent to the peer.
func (s *reliableSender) sendMessage(ctx context.Context, msg lnwire.Message,
	peerPubKey [33]byte) error {

	// We'll start by persisting the message to disk. This allows us to
	// resend the message upon restarts and peer reconnections.
	if err := s.cfg.MessageStore.AddMessage(msg, peerPubKey); err != nil {
		return err
	}

	// Then, we'll spawn a peerHandler for this peer to handle resending its
	// pending messages while taking into account its connection lifecycle.
spawnHandler:
	msgHandler, ok := s.spawnPeerHandler(ctx, peerPubKey)

	// If the handler wasn't previously active, we can exit now as we know
	// that the message will be sent once the peer online notification is
	// received. This prevents us from potentially sending the message
	// twice.
	if !ok {
		return nil
	}

	// Otherwise, we'll attempt to stream the message to the handler.
	// There's a subtle race condition where the handler can be torn down
	// due to all of the messages sent being stale, so we'll handle this
	// gracefully by spawning another one to prevent blocking.
	select {
	case msgHandler.msgs <- msg:
	case <-msgHandler.done:
		goto spawnHandler
	case <-s.quit:
		return ErrGossiperShuttingDown
	}

	return nil
}

// spawnPeerMsgHandler spawns a peerHandler for the given peer if there isn't
// one already active. The boolean returned signals whether there was already
// one active or not.
func (s *reliableSender) spawnPeerHandler(ctx context.Context,
	peerPubKey [33]byte) (peerManager, bool) {

	s.activePeersMtx.Lock()
	msgHandler, ok := s.activePeers[peerPubKey]
	if !ok {
		msgHandler = peerManager{
			msgs: make(chan lnwire.Message),
			done: make(chan struct{}),
		}
		s.activePeers[peerPubKey] = msgHandler
	}
	s.activePeersMtx.Unlock()

	// If this is a newly initiated peerManager, we will create a
	// peerHandler.
	if !ok {
		s.wg.Add(1)
		go s.peerHandler(ctx, msgHandler, peerPubKey)
	}

	return msgHandler, ok
}

// peerHandler is responsible for handling our reliable message send requests
// for a given peer while also taking into account the peer's connection
// lifecycle. Any messages that are attempted to be sent while the peer is
// offline will be queued and sent once the peer reconnects.
//
// NOTE: This must be run as a goroutine.
func (s *reliableSender) peerHandler(ctx context.Context, peerMgr peerManager,
	peerPubKey [33]byte) {

	defer s.wg.Done()

	// We'll start by requesting a notification for when the peer
	// reconnects.
	peerChan := make(chan lnpeer.Peer, 1)

waitUntilOnline:
	log.Debugf("Requesting online notification for peer=%x", peerPubKey)

	s.cfg.NotifyWhenOnline(peerPubKey, peerChan)

	var peer lnpeer.Peer
out:
	for {
		select {
		// While we're waiting, we'll also consume any messages that
		// must be sent to prevent blocking the caller. These can be
		// ignored for now since the peer is currently offline. Once
		// they reconnect, the messages will be sent since they should
		// have been persisted to disk.
		case msg := <-peerMgr.msgs:
			// Retrieve the short channel ID for which this message
			// applies for logging purposes. The error can be
			// ignored as the store can only contain messages which
			// have a ShortChannelID field.
			shortChanID, _ := msgShortChanID(msg)
			log.Debugf("Received request to send %v message for "+
				"channel=%v while peer=%x is offline",
				msg.MsgType(), shortChanID, peerPubKey)

		case peer = <-peerChan:
			break out

		case <-s.quit:
			return
		}
	}

	log.Debugf("Peer=%x is now online, proceeding to send pending messages",
		peerPubKey)

	// Once we detect the peer has reconnected, we'll also request a
	// notification for when they disconnect. We'll use this to make sure
	// they haven't disconnected (in the case of a flappy peer, etc.) by the
	// time we attempt to send them the pending messages.
	log.Debugf("Requesting offline notification for peer=%x", peerPubKey)

	offlineChan := s.cfg.NotifyWhenOffline(peerPubKey)

	pendingMsgs, err := s.cfg.MessageStore.MessagesForPeer(peerPubKey)
	if err != nil {
		log.Errorf("Unable to retrieve pending messages for peer %x: %v",
			peerPubKey, err)
		return
	}

	// With the peer online, we can now proceed to send our pending messages
	// for them.
	for _, msg := range pendingMsgs {
		// Retrieve the short channel ID for which this message applies
		// for logging purposes. The error can be ignored as the store
		// can only contain messages which have a ShortChannelID field.
		shortChanID, _ := msgShortChanID(msg)

		// Ensure the peer is still online right before sending the
		// message.
		select {
		case <-offlineChan:
			goto waitUntilOnline
		default:
		}

		if err := peer.SendMessage(false, msg); err != nil {
			log.Errorf("Unable to send %v message for channel=%v "+
				"to %x: %v", msg.MsgType(), shortChanID,
				peerPubKey, err)
			goto waitUntilOnline
		}

		log.Debugf("Successfully sent %v message for channel=%v with "+
			"peer=%x upon reconnection", msg.MsgType(), shortChanID,
			peerPubKey)

		// Now that the message has at least been sent once, we can
		// check whether it's stale. This guarantees that
		// AnnounceSignatures are sent at least once if we happen to
		// already have signatures for both parties.
		if s.cfg.IsMsgStale(ctx, msg) {
			err := s.cfg.MessageStore.DeleteMessage(msg, peerPubKey)
			if err != nil {
				log.Errorf("Unable to remove stale %v message "+
					"for channel=%v with peer %x: %v",
					msg.MsgType(), shortChanID, peerPubKey,
					err)
				continue
			}

			log.Debugf("Removed stale %v message for channel=%v "+
				"with peer=%x", msg.MsgType(), shortChanID,
				peerPubKey)
		}
	}

	// If all of our messages were stale, then there's no need for this
	// handler to continue running, so we can exit now.
	pendingMsgs, err = s.cfg.MessageStore.MessagesForPeer(peerPubKey)
	if err != nil {
		log.Errorf("Unable to retrieve pending messages for peer %x: %v",
			peerPubKey, err)
		return
	}

	if len(pendingMsgs) == 0 {
		log.Debugf("No pending messages left for peer=%x", peerPubKey)

		s.activePeersMtx.Lock()
		delete(s.activePeers, peerPubKey)
		s.activePeersMtx.Unlock()

		close(peerMgr.done)

		return
	}

	// Once the pending messages are sent, we can continue to send any
	// future messages while the peer remains connected.
	for {
		select {
		case msg := <-peerMgr.msgs:
			// Retrieve the short channel ID for which this message
			// applies for logging purposes. The error can be
			// ignored as the store can only contain messages which
			// have a ShortChannelID field.
			shortChanID, _ := msgShortChanID(msg)

			if err := peer.SendMessage(false, msg); err != nil {
				log.Errorf("Unable to send %v message for "+
					"channel=%v to %x: %v", msg.MsgType(),
					shortChanID, peerPubKey, err)
			}

			log.Debugf("Successfully sent %v message for "+
				"channel=%v with peer=%x", msg.MsgType(),
				shortChanID, peerPubKey)

		case <-offlineChan:
			goto waitUntilOnline

		case <-s.quit:
			return
		}
	}
}

// resendPendingMsgs retrieves and sends all of the messages within the message
// store that should be reliably sent to their respective peers.
func (s *reliableSender) resendPendingMsgs(ctx context.Context) error {
	// Fetch all of the peers for which we have pending messages for and
	// spawn a peerMsgHandler for each. Once the peer is seen as online, all
	// of the pending messages will be sent.
	peers, err := s.cfg.MessageStore.Peers()
	if err != nil {
		return err
	}

	for peer := range peers {
		s.spawnPeerHandler(ctx, peer)
	}

	return nil
}
