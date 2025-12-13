package localchans

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/discovery"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/funding"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing"
)

// Manager manages the node's local channels. The only operation that is
// currently implemented is updating forwarding policies.
type Manager struct {
	// SelfPub contains the public key of the local node.
	SelfPub *btcec.PublicKey

	// DefaultRoutingPolicy is the default routing policy.
	DefaultRoutingPolicy models.ForwardingPolicy

	// UpdateForwardingPolicies is used by the manager to update active
	// links with a new policy.
	UpdateForwardingPolicies func(
		chanPolicies map[wire.OutPoint]models.ForwardingPolicy)

	// PropagateChanPolicyUpdate is called to persist a new policy to disk
	// and broadcast it to the network.
	PropagateChanPolicyUpdate func(
		edgesToUpdate []discovery.EdgeWithInfo) error

	// ForAllOutgoingChannels is required to iterate over all our local
	// channels. The ChannelEdgePolicy parameter may be nil.
	ForAllOutgoingChannels func(ctx context.Context,
		cb func(*models.ChannelEdgeInfo,
			*models.ChannelEdgePolicy) error, reset func()) error

	// FetchChannel is used to query local channel parameters. Optionally an
	// existing db tx can be supplied.
	FetchChannel func(chanPoint wire.OutPoint) (*channeldb.OpenChannel,
		error)

	// AddEdge is used to add edge/channel to the topology of the router.
	AddEdge func(ctx context.Context, edge *models.ChannelEdgeInfo) error

	// policyUpdateLock ensures that the database and the link do not fall
	// out of sync if there are concurrent fee update calls. Without it,
	// there is a chance that policy A updates the database, then policy B
	// updates the database, then policy B updates the link, then policy A
	// updates the link.
	policyUpdateLock sync.Mutex
}

// UpdatePolicy updates the policy for the specified channels on disk and in
// the active links.
func (r *Manager) UpdatePolicy(ctx context.Context,
	newSchema routing.ChannelPolicy,
	createMissingEdge bool, chanPoints ...wire.OutPoint) (
	[]*lnrpc.FailedUpdate, error) {

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

	// NOTE: edge may be nil when this function is called.
	processChan := func(info *models.ChannelEdgeInfo,
		edge *models.ChannelEdgePolicy) error {

		// If we have a channel filter, and this channel isn't a part
		// of it, then we'll skip it.
		_, ok := unprocessedChans[info.ChannelPoint]
		if !ok && haveChanFilter {
			return nil
		}

		// Mark this channel as found by removing it. unprocessedChans
		// will be used to report invalid channels later on.
		delete(unprocessedChans, info.ChannelPoint)

		if edge == nil {
			log.Errorf("Got nil channel edge policy when updating "+
				"a channel. Channel point: %v",
				info.ChannelPoint.String())

			failedUpdates = append(failedUpdates, makeFailureItem(
				info.ChannelPoint,
				lnrpc.UpdateFailure_UPDATE_FAILURE_NOT_FOUND,
				"edge policy not found",
			))

			return nil
		}

		// Apply the new policy to the edge.
		err := r.updateEdge(info.ChannelPoint, edge, newSchema)
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

		var inboundWireFee lnwire.Fee
		edge.InboundFee.WhenSome(func(fee lnwire.Fee) {
			inboundWireFee = fee
		})
		inboundFee := models.NewInboundFeeFromWire(inboundWireFee)

		// Add updated policy to list of policies to send to switch.
		policiesToUpdate[info.ChannelPoint] = models.ForwardingPolicy{
			BaseFee:       edge.FeeBaseMSat,
			FeeRate:       edge.FeeProportionalMillionths,
			TimeLockDelta: uint32(edge.TimeLockDelta),
			MinHTLCOut:    edge.MinHTLC,
			MaxHTLC:       edge.MaxHTLC,
			InboundFee:    inboundFee,
		}

		return nil
	}

	// Next, we'll loop over all the outgoing channels the router knows of.
	// If we have a filter then we'll only collect those channels, otherwise
	// we'll collect them all.
	err := r.ForAllOutgoingChannels(
		ctx, processChan,
		func() {
			failedUpdates = nil
			edgesToUpdate = nil
			clear(policiesToUpdate)
		},
	)
	if err != nil {
		return nil, err
	}

	// Construct a list of failed policy updates.
	for chanPoint := range unprocessedChans {
		channel, err := r.FetchChannel(chanPoint)
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

		// If the edge was not found, but the channel is found, that
		// means the edge is missing in the graph database and should be
		// recreated. The edge and policy are created in-memory. The
		// edge is inserted in createEdge below and the policy will be
		// added to the graph in the PropagateChanPolicyUpdate call
		// below.
		case createMissingEdge:
			log.Warnf("Missing edge for active channel (%s) "+
				"during policy update. Recreating edge with "+
				"default policy.",
				channel.FundingOutpoint.String())

			info, edge, failedUpdate := r.createMissingEdge(
				ctx, channel, newSchema,
			)
			if failedUpdate == nil {
				err = processChan(info, edge)
				if err != nil {
					return nil, err
				}
			} else {
				failedUpdates = append(
					failedUpdates, failedUpdate,
				)
			}

		default:
			log.Warnf("Missing edge for active channel (%s) "+
				"during policy update. Could not update "+
				"policy.", channel.FundingOutpoint.String())

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

func (r *Manager) createMissingEdge(ctx context.Context,
	channel *channeldb.OpenChannel,
	newSchema routing.ChannelPolicy) (*models.ChannelEdgeInfo,
	*models.ChannelEdgePolicy, *lnrpc.FailedUpdate) {

	info, edge, err := r.createEdge(channel, time.Now())
	if err != nil {
		log.Errorf("Failed to recreate missing edge "+
			"for channel (%s): %v",
			channel.FundingOutpoint.String(), err)

		return nil, nil, makeFailureItem(
			channel.FundingOutpoint,
			lnrpc.UpdateFailure_UPDATE_FAILURE_UNKNOWN,
			"could not update policies",
		)
	}

	// Validate the newly created edge policy with the user defined new
	// schema before adding the edge to the database.
	err = r.updateEdge(channel.FundingOutpoint, edge, newSchema)
	if err != nil {
		return nil, nil, makeFailureItem(
			info.ChannelPoint,
			lnrpc.UpdateFailure_UPDATE_FAILURE_INVALID_PARAMETER,
			err.Error(),
		)
	}

	// Insert the edge into the database to avoid `edge not
	// found` errors during policy update propagation.
	err = r.AddEdge(ctx, info)
	if err != nil {
		log.Errorf("Attempt to add missing edge for "+
			"channel (%s) errored with: %v",
			channel.FundingOutpoint.String(), err)

		return nil, nil, makeFailureItem(
			channel.FundingOutpoint,
			lnrpc.UpdateFailure_UPDATE_FAILURE_UNKNOWN,
			"could not add edge",
		)
	}

	return info, edge, nil
}

// createEdge recreates an edge and policy from an open channel in-memory.
func (r *Manager) createEdge(channel *channeldb.OpenChannel,
	timestamp time.Time) (*models.ChannelEdgeInfo,
	*models.ChannelEdgePolicy, error) {

	nodeKey1Bytes := r.SelfPub.SerializeCompressed()
	nodeKey2Bytes := channel.IdentityPub.SerializeCompressed()
	bitcoinKey1Bytes := channel.LocalChanCfg.MultiSigKey.PubKey.
		SerializeCompressed()
	bitcoinKey2Bytes := channel.RemoteChanCfg.MultiSigKey.PubKey.
		SerializeCompressed()
	channelFlags := lnwire.ChanUpdateChanFlags(0)

	// Make it such that node_id_1 is the lexicographically-lesser of the
	// two compressed keys sorted in ascending lexicographic order.
	if bytes.Compare(nodeKey2Bytes, nodeKey1Bytes) < 0 {
		nodeKey1Bytes, nodeKey2Bytes = nodeKey2Bytes, nodeKey1Bytes
		bitcoinKey1Bytes, bitcoinKey2Bytes = bitcoinKey2Bytes,
			bitcoinKey1Bytes
		channelFlags = 1
	}

	// We need to make sure we use the real scid for public confirmed
	// zero-conf channels.
	shortChanID := channel.ShortChanID()
	isPublic := channel.ChannelFlags&lnwire.FFAnnounceChannel != 0
	if isPublic && channel.IsZeroConf() && channel.ZeroConfConfirmed() {
		shortChanID = channel.ZeroConfRealScid()
	}

	fundingScript, err := funding.MakeFundingScript(channel)
	if err != nil {
		return nil, nil, fmt.Errorf("unable to create funding "+
			"script: %v", err)
	}

	info := &models.ChannelEdgeInfo{
		ChannelID:     shortChanID.ToUint64(),
		ChainHash:     channel.ChainHash,
		Features:      lnwire.EmptyFeatureVector(),
		Capacity:      channel.Capacity,
		ChannelPoint:  channel.FundingOutpoint,
		FundingScript: fn.Some(fundingScript),
	}

	copy(info.NodeKey1Bytes[:], nodeKey1Bytes)
	copy(info.NodeKey2Bytes[:], nodeKey2Bytes)
	copy(info.BitcoinKey1Bytes[:], bitcoinKey1Bytes)
	copy(info.BitcoinKey2Bytes[:], bitcoinKey2Bytes)

	// Construct a dummy channel edge policy with default values that will
	// be updated with the new values in the call to processChan below.
	timeLockDelta := uint16(r.DefaultRoutingPolicy.TimeLockDelta)
	edge := &models.ChannelEdgePolicy{
		ChannelID:                 shortChanID.ToUint64(),
		LastUpdate:                timestamp,
		TimeLockDelta:             timeLockDelta,
		ChannelFlags:              channelFlags,
		MessageFlags:              lnwire.ChanUpdateRequiredMaxHtlc,
		FeeBaseMSat:               r.DefaultRoutingPolicy.BaseFee,
		FeeProportionalMillionths: r.DefaultRoutingPolicy.FeeRate,
		MinHTLC:                   r.DefaultRoutingPolicy.MinHTLCOut,
		MaxHTLC:                   r.DefaultRoutingPolicy.MaxHTLC,
	}

	copy(edge.ToNode[:], channel.IdentityPub.SerializeCompressed())

	return info, edge, nil
}

// updateEdge updates the given edge with the new schema.
func (r *Manager) updateEdge(chanPoint wire.OutPoint,
	edge *models.ChannelEdgePolicy,
	newSchema routing.ChannelPolicy) error {

	channel, err := r.FetchChannel(chanPoint)
	if err != nil {
		return err
	}

	// Update forwarding fee scheme and required time lock delta.
	edge.FeeBaseMSat = newSchema.BaseFee
	edge.FeeProportionalMillionths = lnwire.MilliSatoshi(
		newSchema.FeeRate,
	)

	// If inbound fees are set, we update the edge with them.
	err = fn.MapOptionZ(newSchema.InboundFee,
		func(f models.InboundFee) error {
			inboundWireFee := f.ToWire()
			edge.InboundFee = fn.Some(inboundWireFee)

			return edge.ExtraOpaqueData.PackRecords(
				&inboundWireFee,
			)
		})
	if err != nil {
		return err
	}

	edge.TimeLockDelta = uint16(newSchema.TimeLockDelta)

	// Retrieve negotiated channel htlc amt limits.
	amtMin, amtMax, err := r.getHtlcAmtLimits(channel)
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
func (r *Manager) getHtlcAmtLimits(ch *channeldb.OpenChannel) (
	lnwire.MilliSatoshi, lnwire.MilliSatoshi, error) {

	// The max htlc policy field must be less than or equal to the channel
	// capacity AND less than or equal to the max in-flight HTLC value.
	// Since the latter is always less than or equal to the former, just
	// return the max in-flight value.
	maxAmt := ch.LocalChanCfg.ChannelStateBounds.MaxPendingAmount

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
