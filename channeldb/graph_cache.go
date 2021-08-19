package channeldb

import (
	"fmt"
	"sync"

	"github.com/btcsuite/btcutil"

	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

// GraphNode is an interface for all the information the cache needs to know
// about a lightning node.
type GraphNode interface {
	// PubKey is the node's public identity key.
	PubKey() route.Vertex

	// Features returns the node's p2p features.
	Features() *lnwire.FeatureVector

	// ForEachChannel iterates through all channels of a given node,
	// executing the passed callback with an edge info structure and the
	// policies of each end of the channel. The first edge policy is the
	// outgoing edge *to* the connecting node, while the second is the
	// incoming edge *from* the connecting node. If the callback returns an
	// error, then the iteration is halted with the error propagated back up
	// to the caller.
	ForEachChannel(kvdb.RTx,
		func(kvdb.RTx, *ChannelEdgeInfo, *ChannelEdgePolicy,
			*ChannelEdgePolicy) error) error
}

// DirectedChannel is a type that stores the channel information as seen from
// one side of the channel.
type DirectedChannel struct {
	// ChannelID is the unique identifier of this channel.
	ChannelID uint64

	// IsNode1 indicates if this is the node with the smaller public key.
	IsNode1 bool

	// OtherNode is the public key of the node on the other end of this
	// channel.
	OtherNode route.Vertex

	// Capacity is the announced capacity of this channel in satoshis.
	Capacity btcutil.Amount

	// OutPolicy is the outgoing policy from this node *to* the other node.
	OutPolicy *ChannelEdgePolicy

	// InPolicy is the incoming policy *from* the other node to this node.
	InPolicy *ChannelEdgePolicy
}

// GraphCache is a type that holds a minimal set of information of the public
// channel graph that can be used for pathfinding.
type GraphCache struct {
	nodeChannels map[route.Vertex][]*DirectedChannel
	nodeFeatures map[route.Vertex]*lnwire.FeatureVector

	mtx sync.RWMutex
}

// NewGraphCache creates a new graphCache.
func NewGraphCache() *GraphCache {
	return &GraphCache{
		nodeChannels: make(map[route.Vertex][]*DirectedChannel),
		nodeFeatures: make(map[route.Vertex]*lnwire.FeatureVector),
	}
}

// Stats returns statistics about the current cache size.
func (c *GraphCache) Stats() string {
	c.mtx.RLock()
	defer c.mtx.RUnlock()

	numChannels := 0
	for node := range c.nodeChannels {
		numChannels += len(c.nodeChannels[node])
	}
	return fmt.Sprintf("num_node_features=%d, num_nodes=%d, "+
		"num_channels=%d", len(c.nodeFeatures), len(c.nodeChannels),
		numChannels)
}

// AddNode adds a graph node, including all the (directed) channels of that
// node.
func (c *GraphCache) AddNode(tx kvdb.RTx, node GraphNode) error {
	nodePubKey := node.PubKey()

	// Only hold the lock for a short time. The `ForEachChannel()` below is
	// possibly slow as it has to go to the backend, so we can unlock
	// between the calls. And the AddChannel() method will acquire its own
	// lock anyway.
	c.mtx.Lock()
	c.nodeFeatures[nodePubKey] = node.Features()
	c.nodeChannels[nodePubKey] = nil
	c.mtx.Unlock()

	return node.ForEachChannel(
		tx, func(tx kvdb.RTx, info *ChannelEdgeInfo,
			outPolicy *ChannelEdgePolicy,
			inPolicy *ChannelEdgePolicy) error {

			c.AddChannel(info, outPolicy, inPolicy)

			return nil
		},
	)
}

// AddChannel adds a non-directed channel, meaning that the order of policy 1
// and policy 2 does not matter, the directionality is extracted from the info
// and policy flags automatically. The policy will be set as the outgoing policy
// on one node and the incoming policy on the peer's side.
func (c *GraphCache) AddChannel(info *ChannelEdgeInfo,
	policy1 *ChannelEdgePolicy, policy2 *ChannelEdgePolicy) {

	if info == nil {
		return
	}

	if policy1 != nil && policy1.IsDisabled() &&
		policy2 != nil && policy2.IsDisabled() {

		return
	}

	// Create the edge entry for both nodes.
	c.mtx.Lock()
	c.updateOrAddEdge(info.NodeKey1Bytes, &DirectedChannel{
		ChannelID: info.ChannelID,
		IsNode1:   true,
		OtherNode: info.NodeKey2Bytes,
		Capacity:  info.Capacity,
	})
	c.updateOrAddEdge(info.NodeKey2Bytes, &DirectedChannel{
		ChannelID: info.ChannelID,
		IsNode1:   false,
		OtherNode: info.NodeKey1Bytes,
		Capacity:  info.Capacity,
	})
	c.mtx.Unlock()

	// The policy's node is always the to_node. So if policy 1 has to_node
	// of node 2 then we have the policy 1 as seen from node 1.
	if policy1 != nil {
		fromNode, toNode := info.NodeKey1Bytes, info.NodeKey2Bytes
		if policy1.Node.PubKeyBytes != info.NodeKey2Bytes {
			fromNode, toNode = toNode, fromNode
		}
		isEdge1 := policy1.ChannelFlags&lnwire.ChanUpdateDirection == 0
		c.UpdatePolicy(policy1, fromNode, toNode, isEdge1)
	}
	if policy2 != nil {
		fromNode, toNode := info.NodeKey2Bytes, info.NodeKey1Bytes
		if policy2.Node.PubKeyBytes != info.NodeKey1Bytes {
			fromNode, toNode = toNode, fromNode
		}
		isEdge1 := policy2.ChannelFlags&lnwire.ChanUpdateDirection == 0
		c.UpdatePolicy(policy2, fromNode, toNode, isEdge1)
	}
}

// updateOrAddEdge makes sure the edge information for a node is either updated
// if it already exists or is added to that node's list of channels.
func (c *GraphCache) updateOrAddEdge(node route.Vertex, edge *DirectedChannel) {
	for idx := range c.nodeChannels[node] {
		// Do we already have the channel in question?
		if c.nodeChannels[node][idx].ChannelID == edge.ChannelID {
			c.nodeChannels[node][idx] = edge
			return
		}
	}

	// We didn't find the channel, let's add it.
	c.nodeChannels[node] = append(c.nodeChannels[node], edge)
}

// UpdatePolicy updates a single policy on both the from and to node. The order
// of the from and to node is not strictly important. But we assume that a
// channel edge was added beforehand so that the directed channel struct already
// exists in the cache.
func (c *GraphCache) UpdatePolicy(policy *ChannelEdgePolicy, fromNode,
	toNode route.Vertex, edge1 bool) {

	// If a policy's node is nil, we can't cache it yet as that would lead
	// to problems in pathfinding.
	if policy.Node == nil {
		// TODO(guggero): Fix this problem!
		log.Warnf("Cannot cache policy because of missing node (from "+
			"%x to %x)", fromNode[:], toNode[:])
		return
	}

	c.mtx.Lock()
	defer c.mtx.Unlock()

	updatePolicy := func(channels []*DirectedChannel) {
		for _, channel := range channels {
			// Skip if this isn't the channel in question.
			if channel.ChannelID != policy.ChannelID {
				continue
			}

			// Edge 1 is defined as the policy for the direction of
			// node1 to node2.
			switch {
			// This is node 1, and it is edge 1, so this is the
			// outgoing policy for node 1.
			case channel.IsNode1 && edge1:
				channel.OutPolicy = policy

			// This is node 2, and it is edge 2, so this is the
			// outgoing policy for node 2.
			case !channel.IsNode1 && !edge1:
				channel.OutPolicy = policy

			// The other two cases left mean it's the inbound policy
			// for the node.
			default:
				channel.InPolicy = policy
			}

			// We only expect to find one channel, so we might as
			// well return now.
			return
		}
	}

	updatePolicy(c.nodeChannels[fromNode])
	updatePolicy(c.nodeChannels[toNode])
}

// RemoveNode completely removes a node and all its channels (including the
// peer's side).
func (c *GraphCache) RemoveNode(node route.Vertex) {
	c.mtx.Lock()
	defer c.mtx.Unlock()

	delete(c.nodeFeatures, node)

	// First remove all channels from the other nodes' lists.
	for _, channel := range c.nodeChannels[node] {
		c.removeChannelIfFound(channel.OtherNode, channel.ChannelID)
	}

	// Then remove our whole node completely.
	delete(c.nodeChannels, node)
}

// RemoveChannel removes a single channel between two nodes.
func (c *GraphCache) RemoveChannel(node1, node2 route.Vertex, chanID uint64) {
	c.mtx.Lock()
	defer c.mtx.Unlock()

	// Remove that one channel from both sides.
	c.removeChannelIfFound(node1, chanID)
	c.removeChannelIfFound(node2, chanID)
}

// removeChannelIfFound removes a single channel from one side.
func (c *GraphCache) removeChannelIfFound(node route.Vertex, chanID uint64) {
	for i := 0; i < len(c.nodeChannels[node]); i++ {
		if c.nodeChannels[node][i].ChannelID == chanID {
			// We found the channel. Shift everything to the left
			// by one then return since we only expect to find a
			// single channel.
			c.nodeChannels[node] = append(
				c.nodeChannels[node][:i],
				c.nodeChannels[node][i+1:]...,
			)
			return
		}
	}
}

// UpdateChannel updates the channel edge information for a specific edge.
func (c *GraphCache) UpdateChannel(info *ChannelEdgeInfo) {
	c.mtx.Lock()
	defer c.mtx.Unlock()

	for _, channel := range c.nodeChannels[info.NodeKey1Bytes] {
		if channel.ChannelID == info.ChannelID {
			channel.Capacity = info.Capacity
			channel.OtherNode = info.NodeKey2Bytes
			break
		}
	}
	for _, channel := range c.nodeChannels[info.NodeKey2Bytes] {
		if channel.ChannelID == info.ChannelID {
			channel.Capacity = info.Capacity
			channel.OtherNode = info.NodeKey1Bytes
			break
		}
	}
}

// ForEachChannel invokes the given callback for each channel of the given node.
func (c *GraphCache) ForEachChannel(node route.Vertex,
	cb func(channel *DirectedChannel) error) error {

	c.mtx.RLock()
	defer c.mtx.RUnlock()

	channels, ok := c.nodeChannels[node]
	if !ok {
		return nil
	}

	for _, channel := range channels {
		if err := cb(channel); err != nil {
			return err
		}
	}

	return nil
}

// GetFeatures returns the features of the node with the given ID.
func (c *GraphCache) GetFeatures(node route.Vertex) *lnwire.FeatureVector {
	c.mtx.RLock()
	defer c.mtx.RUnlock()

	features, ok := c.nodeFeatures[node]
	if !ok {
		// The router expects the features to never be nil, so we return
		// an empty feature set instead.
		return lnwire.EmptyFeatureVector()
	}

	return features
}
