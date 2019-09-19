package localchans

import (
	"fmt"
	"sync"

	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/discovery"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing"
)

// Manager manages the node's local channels. The only operation that is
// currently implemented is updating forwarding policies.
type Manager struct {
	// UpdateForwardingPolicies is used by the manager to update active
	// links with a new policy.
	UpdateForwardingPolicies func(
		chanPolicies map[wire.OutPoint]htlcswitch.ForwardingPolicy)

	// PropagateChanPolicyUpdate is called to persist a new policy to disk
	// and broadcast it to the network.
	PropagateChanPolicyUpdate func(
		edgesToUpdate []discovery.EdgeWithInfo) error

	// ForAllOutgoingChannels is required to iterate over all our local
	// channels.
	ForAllOutgoingChannels func(cb func(*channeldb.ChannelEdgeInfo,
		*channeldb.ChannelEdgePolicy) error) error

	// policyUpdateLock ensures that the database and the link do not fall
	// out of sync if there are concurrent fee update calls. Without it,
	// there is a chance that policy A updates the database, then policy B
	// updates the database, then policy B updates the link, then policy A
	// updates the link.
	policyUpdateLock sync.Mutex
}

// UpdatePolicy updates the policy for the specified channels on disk and in the
// active links.
func (r *Manager) UpdatePolicy(newSchema routing.ChannelPolicy,
	chanPoints ...wire.OutPoint) error {

	r.policyUpdateLock.Lock()
	defer r.policyUpdateLock.Unlock()

	// First, we'll construct a set of all the channels that need to be
	// updated.
	chansToUpdate := make(map[wire.OutPoint]struct{})
	for _, chanPoint := range chanPoints {
		chansToUpdate[chanPoint] = struct{}{}
	}

	haveChanFilter := len(chansToUpdate) != 0

	var edgesToUpdate []discovery.EdgeWithInfo
	policiesToUpdate := make(map[wire.OutPoint]htlcswitch.ForwardingPolicy)

	// Next, we'll loop over all the outgoing channels the router knows of.
	// If we have a filter then we'll only collected those channels,
	// otherwise we'll collect them all.
	err := r.ForAllOutgoingChannels(func(
		info *channeldb.ChannelEdgeInfo,
		edge *channeldb.ChannelEdgePolicy) error {

		// If we have a channel filter, and this channel isn't a part
		// of it, then we'll skip it.
		_, ok := chansToUpdate[info.ChannelPoint]
		if !ok && haveChanFilter {
			return nil
		}

		// Apply the new policy to the edge.
		err := r.updateEdge(info.Capacity, edge, newSchema)
		if err != nil {
			return nil
		}

		// Add updated edge to list of edges to send to gossiper.
		edgesToUpdate = append(edgesToUpdate, discovery.EdgeWithInfo{
			Info: info,
			Edge: edge,
		})

		// Add updated policy to list of policies to send to switch.
		policiesToUpdate[info.ChannelPoint] = htlcswitch.ForwardingPolicy{
			BaseFee:       edge.FeeBaseMSat,
			FeeRate:       edge.FeeProportionalMillionths,
			TimeLockDelta: uint32(edge.TimeLockDelta),
			MinHTLC:       edge.MinHTLC,
			MaxHTLC:       edge.MaxHTLC,
		}

		return nil
	})
	if err != nil {
		return err
	}

	// Commit the policy updates to disk and broadcast to the network. We
	// validated the new policy above, so we expect no validation errors. If
	// this would happen because of a bug, the link policy will be
	// desynchronized. It is currently not possible to atomically commit
	// multiple edge updates.
	err = r.PropagateChanPolicyUpdate(edgesToUpdate)
	if err != nil {
		return err
	}

	// Update active links.
	r.UpdateForwardingPolicies(policiesToUpdate)

	return nil
}

// updateEdge updates the given edge with the new schema.
func (r *Manager) updateEdge(capacity btcutil.Amount,
	edge *channeldb.ChannelEdgePolicy,
	newSchema routing.ChannelPolicy) error {

	// Update forwarding fee scheme and required time lock delta.
	edge.FeeBaseMSat = newSchema.BaseFee
	edge.FeeProportionalMillionths = lnwire.MilliSatoshi(
		newSchema.FeeRate,
	)
	edge.TimeLockDelta = uint16(newSchema.TimeLockDelta)

	// Max htlc is currently always set to the channel capacity.
	edge.MessageFlags |= lnwire.ChanUpdateOptionMaxHtlc
	edge.MaxHTLC = lnwire.NewMSatFromSatoshis(capacity)

	// Validate htlc amount constraints.
	if edge.MinHTLC > edge.MaxHTLC {
		return fmt.Errorf("min_htlc %v greater than max_htlc %v",
			edge.MinHTLC, edge.MaxHTLC)
	}

	// Clear signature to help prevent usage of the previous signature.
	edge.SetSigBytes(nil)

	return nil
}
