package graphdb

import (
	"fmt"
	"sync"

	"github.com/btcsuite/btcd/btcutil/v2"
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

	// version is the gossip version that announced the channel this entry
	// was built from. The cache keys channels by SCID alone, so entries
	// for the same channel on two gossip versions collide, and this field
	// is what ranks them. Entries built for the no-cache read paths leave
	// it unset, because only the cache ranks entries.
	version lnwire.GossipVersion

	// hasPolicies reports whether the version this entry was built from
	// has a policy, which ranks the entry along with its version. It is
	// tracked apart from OutPolicySet and InPolicy, since the cache leaves
	// out the policies of a channel that are both disabled, while the
	// preferred lookup tables still count them.
	hasPolicies bool
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

// nodeFeatureSet holds the feature vector each gossip version announced for a
// node, together with the vector the cache serves for it.
type nodeFeatureSet struct {
	v1, v2 *lnwire.FeatureVector

	// preferred is the vector that preferredFeatures selects from v1 and
	// v2. It is recomputed whenever either of them changes, so that the
	// result doesn't depend on the order the announcements arrived in.
	preferred *lnwire.FeatureVector
}

// preferredFeatures selects the features to advertise for a node that may be
// announced on both gossip versions. The v2 features win, unless they are
// empty and the node also has a v1 announcement, since an empty v2 vector
// would otherwise hide the node's v1 features. A v2-only node keeps its empty
// vector, so withdrawing its last feature bit clears its features.
func preferredFeatures(v1, v2 *lnwire.FeatureVector) *lnwire.FeatureVector {
	if v1 != nil && (v2 == nil || v2.IsEmpty()) {
		return v1
	}

	return v2
}

// GraphCache is a type that holds a minimal set of information of the public
// channel graph that can be used for pathfinding.
type GraphCache struct {
	nodeChannels map[route.Vertex]map[uint64]*DirectedChannel
	nodeFeatures map[route.Vertex]*nodeFeatureSet

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
			map[route.Vertex]*nodeFeatureSet, preAllocNumNodes,
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

// AddNodeFeatures adds a graph node and the features it announced on the
// given gossip version to the cache. The features the cache serves for the
// node are reselected from both versions, see preferredFeatures.
func (c *GraphCache) AddNodeFeatures(node route.Vertex,
	v lnwire.GossipVersion, features *lnwire.FeatureVector) {

	c.mtx.Lock()
	defer c.mtx.Unlock()

	set, ok := c.nodeFeatures[node]
	if !ok {
		set = &nodeFeatureSet{}
		c.nodeFeatures[node] = set
	}

	if v == lnwire.GossipVersion2 {
		set.v2 = features
	} else {
		set.v1 = features
	}

	set.preferred = preferredFeatures(set.v1, set.v2)
}

// ReplaceChannel caches the given channel in place of any entry for its SCID,
// whichever gossip version that entry was built from. It is used when the
// version that the Store prefers for a channel changes, so that the edge and
// both policies of the new version replace the old ones as a whole, instead of
// leaving a policy of the old version behind.
func (c *GraphCache) ReplaceChannel(info *models.CachedEdgeInfo,
	policy1, policy2 *models.CachedEdgePolicy) {

	if info == nil {
		return
	}

	c.mtx.Lock()
	defer c.mtx.Unlock()

	c.removeChannelIfFound(info.NodeKey1Bytes, info.ChannelID)
	c.removeChannelIfFound(info.NodeKey2Bytes, info.ChannelID)
	c.addChannel(info, policy1, policy2)
}

// AddChannel adds a non-directed channel, meaning that the order of policy 1
// and policy 2 does not matter, the directionality is extracted from the info
// and policy flags automatically. The policy will be set as the outgoing policy
// on one node and the incoming policy on the peer's side.
func (c *GraphCache) AddChannel(info *models.CachedEdgeInfo,
	policy1, policy2 *models.CachedEdgePolicy) {

	c.mtx.Lock()
	defer c.mtx.Unlock()

	c.addChannel(info, policy1, policy2)
}

// addChannel implements AddChannel.
//
// NOTE: The write lock must be held when calling this function.
func (c *GraphCache) addChannel(info *models.CachedEdgeInfo,
	policy1, policy2 *models.CachedEdgePolicy) {

	if info == nil {
		return
	}

	// Create the edge entry for both nodes. We always add the channel
	// structure to the cache, even if both policies are currently disabled,
	// so that later policy updates can find and update the channel entry.
	hasPolicies := policy1 != nil || policy2 != nil

	c.updateOrAddEdge(info.NodeKey1Bytes, &DirectedChannel{
		ChannelID: info.ChannelID,
		IsNode1:   true,
		OtherNode: info.NodeKey2Bytes,
		Capacity:  info.Capacity,
		version:   info.Version,
	}, hasPolicies)
	c.updateOrAddEdge(info.NodeKey2Bytes, &DirectedChannel{
		ChannelID: info.ChannelID,
		IsNode1:   false,
		OtherNode: info.NodeKey1Bytes,
		Capacity:  info.Capacity,
		version:   info.Version,
	}, hasPolicies)

	// Skip adding policies if both are disabled, as the channel is
	// currently unusable for routing. However, we still add the channel
	// structure above so that policy updates can later enable it.
	if policy1 != nil && policy1.IsDisabled &&
		policy2 != nil && policy2.IsDisabled {

		log.Debugf("Skipping policies for channel %v: both "+
			"policies are disabled (channel structure still "+
			"cached for future updates)", info.ChannelID)

		return
	}

	// The policy's node is always the to_node. So if policy 1 has to_node
	// of node 2 then we have the policy 1 as seen from node 1. If the
	// entry of another gossip version was kept above, updatePolicy leaves
	// it alone, so the policies of this version can't leak into it.
	if policy1 != nil {
		fromNode, toNode := info.NodeKey1Bytes, info.NodeKey2Bytes
		if !policy1.IsNode1 {
			fromNode, toNode = toNode, fromNode
		}
		c.updatePolicy(policy1, fromNode, toNode)
	}
	if policy2 != nil {
		fromNode, toNode := info.NodeKey2Bytes, info.NodeKey1Bytes
		if policy2.IsNode1 {
			fromNode, toNode = toNode, fromNode
		}
		c.updatePolicy(policy2, fromNode, toNode)
	}
}

// updateOrAddEdge makes sure the edge information for a node is either updated
// if it already exists or is added to that node's list of channels. An entry
// that a lower ranked gossip version would replace is kept, see
// preferIncomingEdge.
func (c *GraphCache) updateOrAddEdge(node route.Vertex, edge *DirectedChannel,
	hasPolicies bool) {

	if len(c.nodeChannels[node]) == 0 {
		c.nodeChannels[node] = make(map[uint64]*DirectedChannel)
	}

	existing, ok := c.nodeChannels[node][edge.ChannelID]
	if ok && !preferIncomingEdge(existing, edge, hasPolicies) {
		return
	}

	edge.hasPolicies = hasPolicies
	c.nodeChannels[node][edge.ChannelID] = edge
}

// preferIncomingEdge reports whether an incoming edge outranks the cached
// edge. An edge with policies wins, followed by the higher gossip version.
func preferIncomingEdge(existing, incoming *DirectedChannel,
	incomingHasPolicies bool) bool {

	// A repeated announcement on the same version always wins, so that a
	// channel whose policies were withdrawn also loses them here.
	if existing.version == incoming.version {
		return true
	}

	if existing.hasPolicies != incomingHasPolicies {
		return incomingHasPolicies
	}

	return incoming.version > existing.version
}

// UpdatePolicy updates a single policy on both the from and to node. The order
// of the from and to node is not strictly important. But we assume that a
// channel edge was added beforehand so that the directed channel struct already
// exists in the cache.
//
// The policy is only applied to an entry built from the policy's gossip
// version. It returns false if the channel is cached from another version
// that the policy's version now outranks, see preferIncomingEdge. The cached
// entry is left untouched in that case, and the caller has to replace it with
// the channel the Store now prefers, using ReplaceChannel.
func (c *GraphCache) UpdatePolicy(policy *models.CachedEdgePolicy, fromNode,
	toNode route.Vertex) bool {

	c.mtx.Lock()
	defer c.mtx.Unlock()

	return c.updatePolicy(policy, fromNode, toNode)
}

// updatePolicy implements UpdatePolicy.
//
// NOTE: The write lock must be held when calling this function.
func (c *GraphCache) updatePolicy(policy *models.CachedEdgePolicy, fromNode,
	toNode route.Vertex) bool {

	upToDate := true
	updatePolicy := func(nodeKey route.Vertex) {
		if len(c.nodeChannels[nodeKey]) == 0 {
			log.Debugf("Node=%v not found in graph cache", nodeKey)

			return
		}

		channel, ok := c.nodeChannels[nodeKey][policy.ChannelID]
		if !ok {
			log.Debugf("Channel=%v not found in graph cache",
				policy.ChannelID)

			return
		}

		// A policy of another gossip version must not be mixed into
		// this entry. If its version doesn't outrank the cached one,
		// the cached entry stays the preferred one and the policy is
		// ignored here. Otherwise, the whole entry is stale.
		if channel.version != policy.Version {
			incoming := &DirectedChannel{version: policy.Version}
			if preferIncomingEdge(channel, incoming, true) {
				upToDate = false
			}

			return
		}

		channel.hasPolicies = true

		// Edge 1 is defined as the policy for the direction of node1 to
		// node2.
		switch {
		// This is node 1, and it is edge 1, so this is the outgoing
		// policy for node 1.
		case channel.IsNode1 && policy.IsNode1:
			channel.OutPolicySet = true
			policy.InboundFee.WhenSome(func(fee lnwire.Fee) {
				channel.InboundFee = fee
			})

		// This is node 2, and it is edge 2, so this is the outgoing
		// policy for node 2.
		case !channel.IsNode1 && !policy.IsNode1:
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

	return upToDate
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

// channelVersion returns the gossip version that the cached entry of a
// channel was built from, as seen from the given node, and false if the node
// has no cached entry for the channel.
func (c *GraphCache) channelVersion(node route.Vertex,
	chanID uint64) (lnwire.GossipVersion, bool) {

	c.mtx.RLock()
	defer c.mtx.RUnlock()

	channel, ok := c.nodeChannels[node][chanID]
	if !ok {
		return 0, false
	}

	return channel.version, true
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

	var features *lnwire.FeatureVector
	if set, ok := c.nodeFeatures[node]; ok {
		features = set.preferred
	} else {
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

	set, ok := c.nodeFeatures[node]
	if !ok || set.preferred == nil {
		// The router expects the features to never be nil, so we return
		// an empty feature set instead.
		return lnwire.EmptyFeatureVector()
	}

	return set.preferred
}
