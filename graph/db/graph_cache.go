package graphdb

import (
	"fmt"
	"sync"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

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

	// OutPolicySet is a boolean that indicates whether the node has an
	// outgoing policy set. For pathfinding only the existence of the policy
	// is important to know, not the actual content.
	OutPolicySet bool

	// InPolicy is the incoming policy *from* the other node to this node.
	// In path finding, we're walking backward from the destination to the
	// source, so we're always interested in the edge that arrives to us
	// from the other node.
	InPolicy *models.CachedEdgePolicy

	// Inbound fees of this node.
	InboundFee lnwire.Fee
}

// DeepCopy creates a deep copy of the channel, including the incoming policy.
func (c *DirectedChannel) DeepCopy() *DirectedChannel {
	channelCopy := *c

	if channelCopy.InPolicy != nil {
		inPolicyCopy := *channelCopy.InPolicy
		channelCopy.InPolicy = &inPolicyCopy

		// The fields for the ToNode can be overwritten by the path
		// finding algorithm, which is why we need a deep copy in the
		// first place. So we always start out with nil values, just to
		// be sure they don't contain any old data.
		channelCopy.InPolicy.ToNodePubKey = nil
		channelCopy.InPolicy.ToNodeFeatures = nil
	}

	return &channelCopy
}

// GraphCache is a type that holds a minimal set of information of the public
// channel graph that can be used for pathfinding.
type GraphCache struct {
	nodeChannels map[route.Vertex]map[uint64]*DirectedChannel
	nodeFeatures map[route.Vertex]*lnwire.FeatureVector

	mtx sync.RWMutex
}

// NewGraphCache creates a new graphCache.
func NewGraphCache(preAllocNumNodes int) *GraphCache {
	return &GraphCache{
		nodeChannels: make(
			map[route.Vertex]map[uint64]*DirectedChannel,
			// A channel connects two nodes, so we can look it up
			// from both sides, meaning we get double the number of
			// entries.
			preAllocNumNodes*2,
		),
		nodeFeatures: make(
			map[route.Vertex]*lnwire.FeatureVector,
			preAllocNumNodes,
		),
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

// AddNodeFeatures adds a graph node and its features to the cache.
func (c *GraphCache) AddNodeFeatures(node route.Vertex,
	features *lnwire.FeatureVector) {

	c.mtx.Lock()
	defer c.mtx.Unlock()

	c.nodeFeatures[node] = features
}

// AddChannel adds a non-directed channel, meaning that the order of policy 1
// and policy 2 does not matter, the directionality is extracted from the info
// and policy flags automatically. The policy will be set as the outgoing policy
// on one node and the incoming policy on the peer's side.
func (c *GraphCache) AddChannel(info *models.CachedEdgeInfo,
	policy1, policy2 *models.CachedEdgePolicy) {

	if info == nil {
		return
	}

	// Create the edge entry for both nodes. We always add the channel
	// structure to the cache, even if both policies are currently disabled,
	// so that later policy updates can find and update the channel entry.
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

	// Skip adding policies if both are disabled, as the channel is
	// currently unusable for routing. However, we still add the channel
	// structure above so that policy updates can later enable it.
	if policy1 != nil && policy1.IsDisabled() &&
		policy2 != nil && policy2.IsDisabled() {

		log.Debugf("Skipping policies for channel %v: both "+
			"policies are disabled (channel structure still "+
			"cached for future updates)", info.ChannelID)

		return
	}

	// The policy's node is always the to_node. So if policy 1 has to_node
	// of node 2 then we have the policy 1 as seen from node 1.
	if policy1 != nil {
		fromNode, toNode := info.NodeKey1Bytes, info.NodeKey2Bytes
		if !policy1.IsNode1() {
			fromNode, toNode = toNode, fromNode
		}
		c.UpdatePolicy(policy1, fromNode, toNode)
	}
	if policy2 != nil {
		fromNode, toNode := info.NodeKey2Bytes, info.NodeKey1Bytes
		if policy2.IsNode1() {
			fromNode, toNode = toNode, fromNode
		}
		c.UpdatePolicy(policy2, fromNode, toNode)
	}
}

// updateOrAddEdge makes sure the edge information for a node is either updated
// if it already exists or is added to that node's list of channels.
func (c *GraphCache) updateOrAddEdge(node route.Vertex, edge *DirectedChannel) {
	if len(c.nodeChannels[node]) == 0 {
		c.nodeChannels[node] = make(map[uint64]*DirectedChannel)
	}

	c.nodeChannels[node][edge.ChannelID] = edge
}

// UpdatePolicy updates a single policy on both the from and to node. The order
// of the from and to node is not strictly important. But we assume that a
// channel edge was added beforehand so that the directed channel struct already
// exists in the cache.
func (c *GraphCache) UpdatePolicy(policy *models.CachedEdgePolicy, fromNode,
	toNode route.Vertex) {

	c.mtx.Lock()
	defer c.mtx.Unlock()

	updatePolicy := func(nodeKey route.Vertex) {
		if len(c.nodeChannels[nodeKey]) == 0 {
			log.Warnf("Node=%v not found in graph cache", nodeKey)

			return
		}

		channel, ok := c.nodeChannels[nodeKey][policy.ChannelID]
		if !ok {
			log.Warnf("Channel=%v not found in graph cache",
				policy.ChannelID)

			return
		}

		// Edge 1 is defined as the policy for the direction of node1 to
		// node2.
		switch {
		// This is node 1, and it is edge 1, so this is the outgoing
		// policy for node 1.
		case channel.IsNode1 && policy.IsNode1():
			channel.OutPolicySet = true
			policy.InboundFee.WhenSome(func(fee lnwire.Fee) {
				channel.InboundFee = fee
			})

		// This is node 2, and it is edge 2, so this is the outgoing
		// policy for node 2.
		case !channel.IsNode1 && !policy.IsNode1():
			channel.OutPolicySet = true
			policy.InboundFee.WhenSome(func(fee lnwire.Fee) {
				channel.InboundFee = fee
			})

		// The other two cases left mean it's the inbound policy for the
		// node.
		default:
			channel.InPolicy = policy
		}
	}

	updatePolicy(fromNode)
	updatePolicy(toNode)
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
	if len(c.nodeChannels[node]) == 0 {
		return
	}

	delete(c.nodeChannels[node], chanID)
}

// getChannels returns a copy of the passed node's channels or nil if there
// isn't any.
func (c *GraphCache) getChannels(node route.Vertex) []*DirectedChannel {
	c.mtx.RLock()
	defer c.mtx.RUnlock()

	channels, ok := c.nodeChannels[node]
	if !ok {
		return nil
	}

	features, ok := c.nodeFeatures[node]
	if !ok {
		// If the features were set to nil explicitly, that's fine here.
		// The router will overwrite the features of the destination
		// node with those found in the invoice if necessary. But if we
		// didn't yet get a node announcement we want to mimic the
		// behavior of the old DB based code that would always set an
		// empty feature vector instead of leaving it nil.
		features = lnwire.EmptyFeatureVector()
	}

	toNodeCallback := func() route.Vertex {
		return node
	}

	i := 0
	channelsCopy := make([]*DirectedChannel, len(channels))
	for _, channel := range channels {
		// We need to copy the channel and policy to avoid it being
		// updated in the cache if the path finding algorithm sets
		// fields on it (currently only the ToNodeFeatures of the
		// policy).
		channelCopy := channel.DeepCopy()
		if channelCopy.InPolicy != nil {
			channelCopy.InPolicy.ToNodePubKey = toNodeCallback
			channelCopy.InPolicy.ToNodeFeatures = features
		}

		channelsCopy[i] = channelCopy
		i++
	}

	return channelsCopy
}

// ForEachChannel invokes the given callback for each channel of the given node.
func (c *GraphCache) ForEachChannel(node route.Vertex,
	cb func(channel *DirectedChannel) error) error {

	// Obtain a copy of the node's channels. We need do this in order to
	// avoid deadlocks caused by interaction with the graph cache, channel
	// state and the graph database from multiple goroutines. This snapshot
	// is only used for path finding where being stale is acceptable since
	// the real world graph and our representation may always become
	// slightly out of sync for a short time and the actual channel state
	// is stored separately.
	channels := c.getChannels(node)
	for _, channel := range channels {
		if err := cb(channel); err != nil {
			return err
		}
	}

	return nil
}

// ForEachNode iterates over the adjacency list of the graph, executing the
// call back for each node and the set of channels that emanate from the given
// node.
//
// NOTE: This method should be considered _read only_, the channels or nodes
// passed in MUST NOT be modified.
func (c *GraphCache) ForEachNode(cb func(node route.Vertex,
	channels map[uint64]*DirectedChannel) error) error {

	c.mtx.RLock()
	defer c.mtx.RUnlock()

	for node, channels := range c.nodeChannels {
		// We don't make a copy here since this is a read-only RPC
		// call. We also don't need the node features either for this
		// call.
		if err := cb(node, channels); err != nil {
			return err
		}
	}

	return nil
}

// GetFeatures returns the features of the node with the given ID. If no
// features are known for the node, an empty feature vector is returned.
func (c *GraphCache) GetFeatures(node route.Vertex) *lnwire.FeatureVector {
	c.mtx.RLock()
	defer c.mtx.RUnlock()

	features, ok := c.nodeFeatures[node]
	if !ok || features == nil {
		// The router expects the features to never be nil, so we return
		// an empty feature set instead.
		return lnwire.EmptyFeatureVector()
	}

	return features
}
