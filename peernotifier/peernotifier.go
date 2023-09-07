package peernotifier

import (
	"sync"

	"github.com/lightningnetwork/lnd/subscribe"
)

// PeerNotifier is a subsystem which observes peer offline and online events.
// It takes subscriptions for its events, and whenever it observes a new event
// it notifies its subscribers over the proper channel.
type PeerNotifier struct {
	started sync.Once
	stopped sync.Once

	ntfnServer *subscribe.Server
}

// PeerOnlineEvent represents a new event where a peer comes online.
type PeerOnlineEvent struct {
	// PubKey is the peer's compressed public key.
	PubKey [33]byte
}

// PeerOfflineEvent represents a new event where a peer goes offline.
type PeerOfflineEvent struct {
	// PubKey is the peer's compressed public key.
	PubKey [33]byte
}

// New creates a new peer notifier which notifies clients of peer online
// and offline events.
func New() *PeerNotifier {
	return &PeerNotifier{
		ntfnServer: subscribe.NewServer(),
	}
}

// Start starts the PeerNotifier's subscription server.
func (p *PeerNotifier) Start() error {
	var err error

	p.started.Do(func() {
		log.Info("PeerNotifier starting")
		err = p.ntfnServer.Start()
	})

	return err
}

// Stop signals the notifier for a graceful shutdown.
func (p *PeerNotifier) Stop() error {
	var err error
	p.stopped.Do(func() {
		log.Info("PeerNotifier shutting down...")
		defer log.Debug("PeerNotifier shutdown complete")

		err = p.ntfnServer.Stop()
	})
	return err
}

// SubscribePeerEvents returns a subscribe.Client that will receive updates
// any time the Server is informed of a peer event.
func (p *PeerNotifier) SubscribePeerEvents() (*subscribe.Client, error) {
	return p.ntfnServer.Subscribe()
}

// NotifyPeerOnline sends a peer online event to all clients subscribed to the
// peer notifier.
func (p *PeerNotifier) NotifyPeerOnline(pubKey [33]byte) {
	event := PeerOnlineEvent{PubKey: pubKey}

	log.Debugf("PeerNotifier notifying peer: %x online", pubKey)

	if err := p.ntfnServer.SendUpdate(event); err != nil {
		log.Warnf("Unable to send peer online update: %v", err)
	}
}

// NotifyPeerOffline sends a peer offline event to all the clients subscribed
// to the peer notifier.
func (p *PeerNotifier) NotifyPeerOffline(pubKey [33]byte) {
	event := PeerOfflineEvent{PubKey: pubKey}

	log.Debugf("PeerNotifier notifying peer: %x offline", pubKey)

	if err := p.ntfnServer.SendUpdate(event); err != nil {
		log.Warnf("Unable to send peer offline update: %v", err)
	}
}
