package channeldb

import (
	"fmt"
	"sync"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

// GraphCacheNode is an interface for all the information the cache needs to know
// about a lightning node.
type GraphCacheNode interface {
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

// CachedEdgePolicy is a struct that only caches the information of a
// ChannelEdgePolicy that we actually use for pathfinding and therefore need to
// store in the cache.
type CachedEdgePolicy struct {
	// ChannelID is the unique channel ID for the channel. The first 3
	// bytes are the block height, the next 3 the index within the block,
	// and the last 2 bytes are the output index for the channel.
	ChannelID uint64

	// MessageFlags is a bitfield which indicates the presence of optional
	// fields (like max_htlc) in the policy.
	MessageFlags lnwire.ChanUpdateMsgFlags

	// ChannelFlags is a bitfield which signals the capabilities of the
	// channel as well as the directed edge this update applies to.
	ChannelFlags lnwire.ChanUpdateChanFlags

	// TimeLockDelta is the number of blocks this node will subtract from
	// the expiry of an incoming HTLC. This value expresses the time buffer
	// the node would like to HTLC exchanges.
	TimeLockDelta uint16

	// MinHTLC is the smallest value HTLC this node will forward, expressed
	// in millisatoshi.
	MinHTLC lnwire.MilliSatoshi

	// MaxHTLC is the largest value HTLC this node will forward, expressed
	// in millisatoshi.
	MaxHTLC lnwire.MilliSatoshi

	// FeeBaseMSat is the base HTLC fee that will be charged for forwarding
	// ANY HTLC, expressed in mSAT's.
	FeeBaseMSat lnwire.MilliSatoshi

	// FeeProportionalMillionths is the rate that the node will charge for
	// HTLCs for each millionth of a satoshi forwarded.
	FeeProportionalMillionths lnwire.MilliSatoshi

	// ToNodePubKey is a function that returns the to node of a policy.
	// Since we only ever store the inbound policy, this is always the node
	// that we query the channels for in ForEachChannel(). Therefore, we can
	// save a lot of space by not storing this information in the memory and
	// instead just set this function when we copy the policy from cache in
	// ForEachChannel().
	ToNodePubKey func() route.Vertex

	// ToNodeFeatures are the to node's features. They are never set while
	// the edge is in the cache, only on the copy that is returned in
	// ForEachChannel().
	ToNodeFeatures *lnwire.FeatureVector
}

// ComputeFee computes the fee to forward an HTLC of `amt` milli-satoshis over
// the passed active payment channel. This value is currently computed as
// specified in BOLT07, but will likely change in the near future.
func (c *CachedEdgePolicy) ComputeFee(
	amt lnwire.MilliSatoshi) lnwire.MilliSatoshi {

	return c.FeeBaseMSat + (amt*c.FeeProportionalMillionths)/feeRateParts
}

// ComputeFeeFromIncoming computes the fee to forward an HTLC given the incoming
// amount.
func (c *CachedEdgePolicy) ComputeFeeFromIncoming(
	incomingAmt lnwire.MilliSatoshi) lnwire.MilliSatoshi {

	return incomingAmt - divideCeil(
		feeRateParts*(incomingAmt-c.FeeBaseMSat),
		feeRateParts+c.FeeProportionalMillionths,
	)
}

// NewCachedPolicy turns a full policy into a minimal one that can be cached.
func NewCachedPolicy(policy *ChannelEdgePolicy) *CachedEdgePolicy {
	return &CachedEdgePolicy{
		ChannelID:                 policy.ChannelID,
		MessageFlags:              policy.MessageFlags,
		ChannelFlags:              policy.ChannelFlags,
		TimeLockDelta:             policy.TimeLockDelta,
		MinHTLC:                   policy.MinHTLC,
		MaxHTLC:                   policy.MaxHTLC,
		FeeBaseMSat:               policy.FeeBaseMSat,
		FeeProportionalMillionths: policy.FeeProportionalMillionths,
	}
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

	// OutPolicySet is a boolean that indicates whether the node has an
	// outgoing policy set. For pathfinding only the existence of the policy
	// is important to know, not the actual content.
	OutPolicySet bool

	// InPolicy is the incoming policy *from* the other node to this node.
	// In path finding, we're walking backward from the destination to the
	// source, so we're always interested in the edge that arrives to us
	// from the other node.
	InPolicy *CachedEdgePolicy
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
func (c *GraphCache) AddNodeFeatures(node GraphCacheNode) {
	nodePubKey := node.PubKey()

	// Only hold the lock for a short time. The `ForEachChannel()` below is
	// possibly slow as it has to go to the backend, so we can unlock
	// between the calls. And the AddChannel() method will acquire its own
	// lock anyway.
	c.mtx.Lock()
	c.nodeFeatures[nodePubKey] = node.Features()
	c.mtx.Unlock()
}

// AddNode adds a graph node, including all the (directed) channels of that
// node.
func (c *GraphCache) AddNode(tx kvdb.RTx, node GraphCacheNode) error {
	c.AddNodeFeatures(node)

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
	if len(c.nodeChannels[node]) == 0 {
		c.nodeChannels[node] = make(map[uint64]*DirectedChannel)
	}

	c.nodeChannels[node][edge.ChannelID] = edge
}

// UpdatePolicy updates a single policy on both the from and to node. The order
// of the from and to node is not strictly important. But we assume that a
// channel edge was added beforehand so that the directed channel struct already
// exists in the cache.
func (c *GraphCache) UpdatePolicy(policy *ChannelEdgePolicy, fromNode,
	toNode route.Vertex, edge1 bool) {

	c.mtx.Lock()
	defer c.mtx.Unlock()

	updatePolicy := func(nodeKey route.Vertex) {
		if len(c.nodeChannels[nodeKey]) == 0 {
			return
		}

		channel, ok := c.nodeChannels[nodeKey][policy.ChannelID]
		if !ok {
			return
		}

		// Edge 1 is defined as the policy for the direction of node1 to
		// node2.
		switch {
		// This is node 1, and it is edge 1, so this is the outgoing
		// policy for node 1.
		case channel.IsNode1 && edge1:
			channel.OutPolicySet = true

		// This is node 2, and it is edge 2, so this is the outgoing
		// policy for node 2.
		case !channel.IsNode1 && !edge1:
			channel.OutPolicySet = true

		// The other two cases left mean it's the inbound policy for the
		// node.
		default:
			channel.InPolicy = NewCachedPolicy(policy)
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

// UpdateChannel updates the channel edge information for a specific edge. We
// expect the edge to already exist and be known. If it does not yet exist, this
// call is a no-op.
func (c *GraphCache) UpdateChannel(info *ChannelEdgeInfo) {
	c.mtx.Lock()
	defer c.mtx.Unlock()

	if len(c.nodeChannels[info.NodeKey1Bytes]) == 0 ||
		len(c.nodeChannels[info.NodeKey2Bytes]) == 0 {

		return
	}

	channel, ok := c.nodeChannels[info.NodeKey1Bytes][info.ChannelID]
	if ok {
		// We only expect to be called when the channel is already
		// known.
		channel.Capacity = info.Capacity
		channel.OtherNode = info.NodeKey2Bytes
	}

	channel, ok = c.nodeChannels[info.NodeKey2Bytes][info.ChannelID]
	if ok {
		channel.Capacity = info.Capacity
		channel.OtherNode = info.NodeKey1Bytes
	}
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
