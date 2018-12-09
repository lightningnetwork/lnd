package routing

import (
	"fmt"
	"net"
	"sync"
	"sync/atomic"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/davecgh/go-spew/spew"
	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
)

// TopologyClient represents an intent to receive notifications from the
// channel router regarding changes to the topology of the channel graph. The
// TopologyChanges channel will be sent upon with new updates to the channel
// graph in real-time as they're encountered.
type TopologyClient struct {
	// TopologyChanges is a receive only channel that new channel graph
	// updates will be sent over.
	//
	// TODO(roasbeef): chan for each update type instead?
	TopologyChanges <-chan *TopologyChange

	// Cancel is a function closure that should be executed when the client
	// wishes to cancel their notification intent. Doing so allows the
	// ChannelRouter to free up resources.
	Cancel func()
}

// topologyClientUpdate is a message sent to the channel router to either
// register a new topology client or re-register an existing client.
type topologyClientUpdate struct {
	// cancel indicates if the update to the client is cancelling an
	// existing client's notifications. If not then this update will be to
	// register a new set of notifications.
	cancel bool

	// clientID is the unique identifier for this client. Any further
	// updates (deleting or adding) to this notification client will be
	// dispatched according to the target clientID.
	clientID uint64

	// ntfnChan is a *send-only* channel in which notifications should be
	// sent over from router -> client.
	ntfnChan chan<- *TopologyChange
}

// SubscribeTopology returns a new topology client which can be used by the
// caller to receive notifications whenever a change in the channel graph
// topology occurs. Changes that will be sent at notifications include: new
// nodes appearing, node updating their attributes, new channels, channels
// closing, and updates in the routing policies of a channel's directed edges.
func (r *ChannelRouter) SubscribeTopology() (*TopologyClient, error) {
	// We'll first atomically obtain the next ID for this client from the
	// incrementing client ID counter.
	clientID := atomic.AddUint64(&r.ntfnClientCounter, 1)

	log.Debugf("New graph topology client subscription, client %v",
		clientID)

	ntfnChan := make(chan *TopologyChange, 10)

	select {
	case r.ntfnClientUpdates <- &topologyClientUpdate{
		cancel:   false,
		clientID: clientID,
		ntfnChan: ntfnChan,
	}:
	case <-r.quit:
		return nil, errors.New("ChannelRouter shutting down")
	}

	return &TopologyClient{
		TopologyChanges: ntfnChan,
		Cancel: func() {
			select {
			case r.ntfnClientUpdates <- &topologyClientUpdate{
				cancel:   true,
				clientID: clientID,
			}:
			case <-r.quit:
				return
			}
		},
	}, nil
}

// topologyClient is a data-structure use by the channel router to couple the
// client's notification channel along with a special "exit" channel that can
// be used to cancel all lingering goroutines blocked on a send to the
// notification channel.
type topologyClient struct {
	// ntfnChan is a send-only channel that's used to propagate
	// notification s from the channel router to an instance of a
	// topologyClient client.
	ntfnChan chan<- *TopologyChange

	// exit is a channel that is used internally by the channel router to
	// cancel any active un-consumed goroutine notifications.
	exit chan struct{}

	wg sync.WaitGroup
}

// notifyTopologyChange notifies all registered clients of a new change in
// graph topology in a non-blocking.
func (r *ChannelRouter) notifyTopologyChange(topologyDiff *TopologyChange) {
	r.RLock()
	numClients := len(r.topologyClients)
	r.RUnlock()

	// Do not reacquire the lock twice unnecessarily.
	if numClients == 0 {
		return
	}

	log.Tracef("Sending topology notification to %v clients %v",
		numClients,
		newLogClosure(func() string {
			return spew.Sdump(topologyDiff)
		}),
	)

	r.RLock()
	for _, client := range r.topologyClients {
		client.wg.Add(1)

		go func(c *topologyClient) {
			defer c.wg.Done()

			select {

			// In this case we'll try to send the notification
			// directly to the upstream client consumer.
			case c.ntfnChan <- topologyDiff:

			// If the client cancels the notifications, then we'll
			// exit early.
			case <-c.exit:

			// Similarly, if the ChannelRouter itself exists early,
			// then we'll also exit ourselves.
			case <-r.quit:

			}
		}(client)
	}
	r.RUnlock()
}

// TopologyChange represents a new set of modifications to the channel graph.
// Topology changes will be dispatched in real-time as the ChannelGraph
// validates and process modifications to the authenticated channel graph.
type TopologyChange struct {
	// NodeUpdates is a slice of nodes which are either new to the channel
	// graph, or have had their attributes updated in an authenticated
	// manner.
	NodeUpdates []*NetworkNodeUpdate

	// ChanelEdgeUpdates is a slice of channel edges which are either newly
	// opened and authenticated, or have had their routing policies
	// updated.
	ChannelEdgeUpdates []*ChannelEdgeUpdate

	// ClosedChannels contains a slice of close channel summaries which
	// described which block a channel was closed at, and also carry
	// supplemental information such as the capacity of the former channel.
	ClosedChannels []*ClosedChanSummary
}

// isEmpty returns true if the TopologyChange is empty. A TopologyChange is
// considered empty, if it contains no *new* updates of any type.
func (t *TopologyChange) isEmpty() bool {
	return len(t.NodeUpdates) == 0 && len(t.ChannelEdgeUpdates) == 0 &&
		len(t.ClosedChannels) == 0
}

// ClosedChanSummary is a summary of a channel that was detected as being
// closed by monitoring the blockchain. Once a channel's funding point has been
// spent, the channel will automatically be marked as closed by the
// ChainNotifier.
//
// TODO(roasbeef): add nodes involved?
type ClosedChanSummary struct {
	// ChanID is the short-channel ID which uniquely identifies the
	// channel.
	ChanID uint64

	// Capacity was the total capacity of the channel before it was closed.
	Capacity btcutil.Amount

	// ClosedHeight is the height in the chain that the channel was closed
	// at.
	ClosedHeight uint32

	// ChanPoint is the funding point, or the multi-sig utxo which
	// previously represented the channel.
	ChanPoint wire.OutPoint
}

// createCloseSummaries takes in a slice of channels closed at the target block
// height and creates a slice of summaries which of each channel closure.
func createCloseSummaries(blockHeight uint32,
	closedChans ...*channeldb.ChannelEdgeInfo) []*ClosedChanSummary {

	closeSummaries := make([]*ClosedChanSummary, len(closedChans))
	for i, closedChan := range closedChans {
		closeSummaries[i] = &ClosedChanSummary{
			ChanID:       closedChan.ChannelID,
			Capacity:     closedChan.Capacity,
			ClosedHeight: blockHeight,
			ChanPoint:    closedChan.ChannelPoint,
		}
	}

	return closeSummaries
}

// NetworkNodeUpdate is an update for a  node within the Lightning Network. A
// NetworkNodeUpdate is sent out either when a new node joins the network, or a
// node broadcasts a new update with a newer time stamp that supersedes its
// old update. All updates are properly authenticated.
type NetworkNodeUpdate struct {
	// Addresses is a slice of all the node's known addresses.
	Addresses []net.Addr

	// IdentityKey is the identity public key of the target node. This is
	// used to encrypt onion blobs as well as to authenticate any new
	// updates.
	IdentityKey *btcec.PublicKey

	// GlobalFeatures is a set of opaque bytes that describe the set of
	// features supported by the node.
	GlobalFeatures []byte

	// Alias is the alias or nick name of the node.
	Alias string
}

// ChannelEdgeUpdate is an update for a new channel within the ChannelGraph.
// This update is sent out once a new authenticated channel edge is discovered
// within the network. These updates are directional, so if a channel is fully
// public, then there will be two updates sent out: one for each direction
// within the channel. Each update will carry that particular routing edge
// policy for the channel direction.
//
// An edge is a channel in the direction of AdvertisingNode -> ConnectingNode.
type ChannelEdgeUpdate struct {
	// ChanID is the unique short channel ID for the channel. This encodes
	// where in the blockchain the channel's funding transaction was
	// originally confirmed.
	ChanID uint64

	// ChanPoint is the outpoint which represents the multi-sig funding
	// output for the channel.
	ChanPoint wire.OutPoint

	// Capacity is the capacity of the newly created channel.
	Capacity btcutil.Amount

	// MinHTLC is the minimum HTLC amount that this channel will forward.
	MinHTLC lnwire.MilliSatoshi

	// MaxHTLC is the maximum HTLC amount that this channel will forward.
	MaxHTLC lnwire.MilliSatoshi

	// BaseFee is the base fee that will charged for all HTLC's forwarded
	// across the this channel direction.
	BaseFee lnwire.MilliSatoshi

	// FeeRate is the fee rate that will be shared for all HTLC's forwarded
	// across this channel direction.
	FeeRate lnwire.MilliSatoshi

	// TimeLockDelta is the time-lock expressed in blocks that will be
	// added to outgoing HTLC's from incoming HTLC's. This value is the
	// difference of the incoming and outgoing HTLC's time-locks routed
	// through this hop.
	TimeLockDelta uint16

	// AdvertisingNode is the node that's advertising this edge.
	AdvertisingNode *btcec.PublicKey

	// ConnectingNode is the node that the advertising node connects to.
	ConnectingNode *btcec.PublicKey

	// Disabled, if true, signals that the channel is unavailable to relay
	// payments.
	Disabled bool
}

// appendTopologyChange appends the passed update message to the passed
// TopologyChange, properly identifying which type of update the message
// constitutes. This function will also fetch any required auxiliary
// information required to create the topology change update from the graph
// database.
func addToTopologyChange(graph *channeldb.ChannelGraph, update *TopologyChange,
	msg interface{}) error {

	switch m := msg.(type) {

	// Any node announcement maps directly to a NetworkNodeUpdate struct.
	// No further data munging or db queries are required.
	case *channeldb.LightningNode:
		pubKey, err := m.PubKey()
		if err != nil {
			return err
		}
		nodeUpdate := &NetworkNodeUpdate{
			Addresses:   m.Addresses,
			IdentityKey: pubKey,
			Alias:       m.Alias,
		}
		nodeUpdate.IdentityKey.Curve = nil

		update.NodeUpdates = append(update.NodeUpdates, nodeUpdate)
		return nil

	// We ignore initial channel announcements as we'll only send out
	// updates once the individual edges themselves have been updated.
	case *channeldb.ChannelEdgeInfo:
		return nil

	// Any new ChannelUpdateAnnouncements will generate a corresponding
	// ChannelEdgeUpdate notification.
	case *channeldb.ChannelEdgePolicy:
		// We'll need to fetch the edge's information from the database
		// in order to get the information concerning which nodes are
		// being connected.
		edgeInfo, _, _, err := graph.FetchChannelEdgesByID(m.ChannelID)
		if err != nil {
			return errors.Errorf("unable fetch channel edge: %v",
				err)
		}

		// If the flag is one, then the advertising node is actually
		// the second node.
		sourceNode := edgeInfo.NodeKey1
		connectingNode := edgeInfo.NodeKey2
		if m.ChannelFlags&lnwire.ChanUpdateDirection == 1 {
			sourceNode = edgeInfo.NodeKey2
			connectingNode = edgeInfo.NodeKey1
		}

		aNode, err := sourceNode()
		if err != nil {
			return err
		}
		cNode, err := connectingNode()
		if err != nil {
			return err
		}

		edgeUpdate := &ChannelEdgeUpdate{
			ChanID:          m.ChannelID,
			ChanPoint:       edgeInfo.ChannelPoint,
			TimeLockDelta:   m.TimeLockDelta,
			Capacity:        edgeInfo.Capacity,
			MinHTLC:         m.MinHTLC,
			MaxHTLC:         m.MaxHTLC,
			BaseFee:         m.FeeBaseMSat,
			FeeRate:         m.FeeProportionalMillionths,
			AdvertisingNode: aNode,
			ConnectingNode:  cNode,
			Disabled:        m.ChannelFlags&lnwire.ChanUpdateDisabled != 0,
		}
		edgeUpdate.AdvertisingNode.Curve = nil
		edgeUpdate.ConnectingNode.Curve = nil

		// TODO(roasbeef): add bit to toggle
		update.ChannelEdgeUpdates = append(update.ChannelEdgeUpdates,
			edgeUpdate)
		return nil

	default:
		return fmt.Errorf("Unable to add to topology change, "+
			"unknown message type %T", msg)
	}
}
