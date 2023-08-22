package localchans

import (
	"errors"
	"fmt"
	"sync"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/channeldb/models"
	"github.com/lightningnetwork/lnd/discovery"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing"
)

// Manager manages the node's local channels. The only operation that is
// currently implemented is updating forwarding policies.
type Manager struct {
	// UpdateForwardingPolicies is used by the manager to update active
	// links with a new policy.
	UpdateForwardingPolicies func(
		chanPolicies map[wire.OutPoint]models.ForwardingPolicy)

	// PropagateChanPolicyUpdate is called to persist a new policy to disk
	// and broadcast it to the network.
	PropagateChanPolicyUpdate func(
		edgesToUpdate []discovery.EdgeWithInfo) error

	// ForAllOutgoingChannels is required to iterate over all our local
	// channels.
	ForAllOutgoingChannels func(cb func(kvdb.RTx,
		*channeldb.ChannelEdgeInfo,
		*channeldb.ChannelEdgePolicy) error) error

	// FetchChannel is used to query local channel parameters. Optionally an
	// existing db tx can be supplied.
	FetchChannel func(tx kvdb.RTx, chanPoint wire.OutPoint) (
		*channeldb.OpenChannel, error)

	// policyUpdateLock ensures that the database and the link do not fall
	// out of sync if there are concurrent fee update calls. Without it,
	// there is a chance that policy A updates the database, then policy B
	// updates the database, then policy B updates the link, then policy A
	// updates the link.
	policyUpdateLock sync.Mutex
}

// UpdatePolicy updates the policy for the specified channels on disk and in
// the active links.
func (r *Manager) UpdatePolicy(newSchema routing.ChannelPolicy,
	chanPoints ...wire.OutPoint) ([]*lnrpc.FailedUpdate, error) {

	r.policyUpdateLock.Lock()
	defer r.policyUpdateLock.Unlock()

	// First, we'll construct a set of all the channels that we are
	// trying to update.
	unprocessedChans := make(map[wire.OutPoint]struct{})
	for _, chanPoint := range chanPoints {
		unprocessedChans[chanPoint] = struct{}{}
	}

	haveChanFilter := len(unprocessedChans) != 0

	var failedUpdates []*lnrpc.FailedUpdate
	var edgesToUpdate []discovery.EdgeWithInfo
	policiesToUpdate := make(map[wire.OutPoint]models.ForwardingPolicy)

	// Next, we'll loop over all the outgoing channels the router knows of.
	// If we have a filter then we'll only collected those channels,
	// otherwise we'll collect them all.
	err := r.ForAllOutgoingChannels(func(
		tx kvdb.RTx,
		info *channeldb.ChannelEdgeInfo,
		edge *channeldb.ChannelEdgePolicy) error {

		// If we have a channel filter, and this channel isn't a part
		// of it, then we'll skip it.
		_, ok := unprocessedChans[info.ChannelPoint]
		if !ok && haveChanFilter {
			return nil
		}

		// Mark this channel as found by removing it. unprocessedChans
		// will be used to report invalid channels later on.
		delete(unprocessedChans, info.ChannelPoint)

		// Apply the new policy to the edge.
		err := r.updateEdge(tx, info.ChannelPoint, edge, newSchema)
		if err != nil {
			failedUpdates = append(failedUpdates,
				makeFailureItem(info.ChannelPoint,
					lnrpc.UpdateFailure_UPDATE_FAILURE_INVALID_PARAMETER,
					err.Error(),
				))

			return nil
		}

		// Add updated edge to list of edges to send to gossiper.
		edgesToUpdate = append(edgesToUpdate, discovery.EdgeWithInfo{
			Info: info,
			Edge: edge,
		})

		// Add updated policy to list of policies to send to switch.
		policiesToUpdate[info.ChannelPoint] = models.ForwardingPolicy{
			BaseFee:       edge.FeeBaseMSat,
			FeeRate:       edge.FeeProportionalMillionths,
			TimeLockDelta: uint32(edge.TimeLockDelta),
			MinHTLCOut:    edge.MinHTLC,
			MaxHTLC:       edge.MaxHTLC,
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	// Construct a list of failed policy updates.
	for chanPoint := range unprocessedChans {
		channel, err := r.FetchChannel(nil, chanPoint)
		switch {
		case errors.Is(err, channeldb.ErrChannelNotFound):
			failedUpdates = append(failedUpdates,
				makeFailureItem(chanPoint,
					lnrpc.UpdateFailure_UPDATE_FAILURE_NOT_FOUND,
					"not found",
				))

		case err != nil:
			failedUpdates = append(failedUpdates,
				makeFailureItem(chanPoint,
					lnrpc.UpdateFailure_UPDATE_FAILURE_INTERNAL_ERR,
					err.Error(),
				))

		case channel.IsPending:
			failedUpdates = append(failedUpdates,
				makeFailureItem(chanPoint,
					lnrpc.UpdateFailure_UPDATE_FAILURE_PENDING,
					"not yet confirmed",
				))

		default:
			failedUpdates = append(failedUpdates,
				makeFailureItem(chanPoint,
					lnrpc.UpdateFailure_UPDATE_FAILURE_UNKNOWN,
					"could not update policies",
				))
		}
	}

	// Commit the policy updates to disk and broadcast to the network. We
	// validated the new policy above, so we expect no validation errors. If
	// this would happen because of a bug, the link policy will be
	// desynchronized. It is currently not possible to atomically commit
	// multiple edge updates.
	err = r.PropagateChanPolicyUpdate(edgesToUpdate)
	if err != nil {
		return nil, err
	}

	// Update active links.
	r.UpdateForwardingPolicies(policiesToUpdate)

	return failedUpdates, nil
}

// updateEdge updates the given edge with the new schema.
func (r *Manager) updateEdge(tx kvdb.RTx, chanPoint wire.OutPoint,
	edge *channeldb.ChannelEdgePolicy,
	newSchema routing.ChannelPolicy) error {

	// Update forwarding fee scheme and required time lock delta.
	edge.FeeBaseMSat = newSchema.BaseFee
	edge.FeeProportionalMillionths = lnwire.MilliSatoshi(
		newSchema.FeeRate,
	)
	edge.TimeLockDelta = uint16(newSchema.TimeLockDelta)

	// Retrieve negotiated channel htlc amt limits.
	amtMin, amtMax, err := r.getHtlcAmtLimits(tx, chanPoint)
	if err != nil {
		return err
	}

	// We now update the edge max htlc value.
	switch {
	// If a non-zero max htlc was specified, use it to update the edge.
	// Otherwise keep the value unchanged.
	case newSchema.MaxHTLC != 0:
		edge.MaxHTLC = newSchema.MaxHTLC

	// If this edge still doesn't have a max htlc set, set it to the max.
	// This is an on-the-fly migration.
	case !edge.MessageFlags.HasMaxHtlc():
		edge.MaxHTLC = amtMax

	// If this edge has a max htlc that exceeds what the channel can
	// actually carry, correct it now. This can happen, because we
	// previously set the max htlc to the channel capacity.
	case edge.MaxHTLC > amtMax:
		edge.MaxHTLC = amtMax
	}

	// If a new min htlc is specified, update the edge.
	if newSchema.MinHTLC != nil {
		edge.MinHTLC = *newSchema.MinHTLC
	}

	// If the MaxHtlc flag wasn't already set, we can set it now.
	edge.MessageFlags |= lnwire.ChanUpdateRequiredMaxHtlc

	// Validate htlc amount constraints.
	switch {
	case edge.MinHTLC < amtMin:
		return fmt.Errorf(
			"min htlc amount of %v is below min htlc parameter of %v",
			edge.MinHTLC, amtMin,
		)

	case edge.MaxHTLC > amtMax:
		return fmt.Errorf(
			"max htlc size of %v is above max pending amount of %v",
			edge.MaxHTLC, amtMax,
		)

	case edge.MinHTLC > edge.MaxHTLC:
		return fmt.Errorf(
			"min_htlc %v greater than max_htlc %v",
			edge.MinHTLC, edge.MaxHTLC,
		)
	}

	// Clear signature to help prevent usage of the previous signature.
	edge.SetSigBytes(nil)

	return nil
}

// getHtlcAmtLimits retrieves the negotiated channel min and max htlc amount
// constraints.
func (r *Manager) getHtlcAmtLimits(tx kvdb.RTx, chanPoint wire.OutPoint) (
	lnwire.MilliSatoshi, lnwire.MilliSatoshi, error) {

	ch, err := r.FetchChannel(tx, chanPoint)
	if err != nil {
		return 0, 0, err
	}

	// The max htlc policy field must be less than or equal to the channel
	// capacity AND less than or equal to the max in-flight HTLC value.
	// Since the latter is always less than or equal to the former, just
	// return the max in-flight value.
	maxAmt := ch.LocalChanCfg.ChannelConstraints.MaxPendingAmount

	return ch.LocalChanCfg.MinHTLC, maxAmt, nil
}

// makeFailureItem creates a lnrpc.FailedUpdate object.
func makeFailureItem(outPoint wire.OutPoint, updateFailure lnrpc.UpdateFailure,
	errStr string) *lnrpc.FailedUpdate {

	outpoint := lnrpc.MarshalOutPoint(&outPoint)

	return &lnrpc.FailedUpdate{
		Outpoint:    outpoint,
		Reason:      updateFailure,
		UpdateError: errStr,
	}
}
