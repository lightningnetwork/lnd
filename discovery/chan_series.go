package discovery

import (
	"context"
	"iter"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/netann"
	"github.com/lightningnetwork/lnd/routing/route"
)

// ChannelGraphTimeSeries is an interface that provides time and block based
// querying into our view of the channel graph. New channels will have
// monotonically increasing block heights, and new channel updates will have
// increasing timestamps. Once we connect to a peer, we'll use the methods in
// this interface to determine if we're already in sync, or need to request
// some new information from them.
type ChannelGraphTimeSeries interface {
	// HighestChanID should return the channel ID of the channel we know of
	// that's furthest in the target chain. This channel will have a block
	// height that's close to the current tip of the main chain as we
	// know it.  We'll use this to start our QueryChannelRange dance with
	// the remote node.
	HighestChanID(ctx context.Context,
		chain chainhash.Hash) (*lnwire.ShortChannelID, error)

	// UpdatesInHorizon returns all known channel and node updates with an
	// update timestamp between the start time and end time. We'll use this
	// to catch up a remote node to the set of channel updates that they
	// may have missed out on within the target chain.
	UpdatesInHorizon(chain chainhash.Hash, startTime time.Time,
		endTime time.Time) iter.Seq2[lnwire.Message, error]

	// FilterKnownChanIDs takes a target chain, and a set of channel ID's,
	// and returns a filtered set of chan ID's. This filtered set of chan
	// ID's represents the ID's that we don't know of which were in the
	// passed superSet.
	FilterKnownChanIDs(chain chainhash.Hash,
		superSet []graphdb.ChannelUpdateInfo,
		isZombieChan func(time.Time, time.Time) bool) (
		[]lnwire.ShortChannelID, error)

	// FilterChannelRange returns the set of channels that we created
	// between the start height and the end height. The channel IDs are
	// grouped by their common block height. We'll use this to to a remote
	// peer's QueryChannelRange message.
	FilterChannelRange(chain chainhash.Hash, startHeight, endHeight uint32,
		withTimestamps bool) ([]graphdb.BlockChannelRange, error)

	// FetchChanAnns returns a full set of channel announcements as well as
	// their updates that match the set of specified short channel ID's.
	// We'll use this to reply to a QueryShortChanIDs message sent by a
	// remote peer. The response will contain a unique set of
	// ChannelAnnouncements, the latest ChannelUpdate for each of the
	// announcements, and a unique set of NodeAnnouncements.
	FetchChanAnns(chain chainhash.Hash,
		shortChanIDs []lnwire.ShortChannelID) ([]lnwire.Message, error)

	// FetchChanUpdates returns the latest channel update messages for the
	// specified short channel ID. If no channel updates are known for the
	// channel, then an empty slice will be returned.
	FetchChanUpdates(chain chainhash.Hash,
		shortChanID lnwire.ShortChannelID) ([]*lnwire.ChannelUpdate1,
		error)
}

// ChanSeries is an implementation of the ChannelGraphTimeSeries
// interface backed by the channeldb ChannelGraph database. We'll provide this
// implementation to the AuthenticatedGossiper so it can properly use the
// in-protocol channel range queries to quickly and efficiently synchronize our
// channel state with all peers.
type ChanSeries struct {
	graph *graphdb.ChannelGraph
}

// NewChanSeries constructs a new ChanSeries backed by a channeldb.ChannelGraph.
// The returned ChanSeries implements the ChannelGraphTimeSeries interface.
func NewChanSeries(graph *graphdb.ChannelGraph) *ChanSeries {
	return &ChanSeries{
		graph: graph,
	}
}

// HighestChanID should return is the channel ID of the channel we know of
// that's furthest in the target chain. This channel will have a block height
// that's close to the current tip of the main chain as we know it.  We'll use
// this to start our QueryChannelRange dance with the remote node.
//
// NOTE: This is part of the ChannelGraphTimeSeries interface.
func (c *ChanSeries) HighestChanID(ctx context.Context,
	_ chainhash.Hash) (*lnwire.ShortChannelID, error) {

	chanID, err := c.graph.HighestChanID(ctx)
	if err != nil {
		return nil, err
	}

	shortChanID := lnwire.NewShortChanIDFromInt(chanID)
	return &shortChanID, nil
}

// UpdatesInHorizon returns all known channel and node updates with an update
// timestamp between the start time and end time. We'll use this to catch up a
// remote node to the set of channel updates that they may have missed out on
// within the target chain.
//
// NOTE: This is part of the ChannelGraphTimeSeries interface.
func (c *ChanSeries) UpdatesInHorizon(chain chainhash.Hash,
	startTime, endTime time.Time) iter.Seq2[lnwire.Message, error] {

	return func(yield func(lnwire.Message, error) bool) {
		// First, we'll query for all the set of channels that have an
		// update that falls within the specified horizon.
		chansInHorizon := c.graph.ChanUpdatesInHorizon(
			startTime, endTime,
		)

		for channel, err := range chansInHorizon {
			if err != nil {
				yield(nil, err)
				return
			}
			// If the channel hasn't been fully advertised yet, or
			// is a private channel, then we'll skip it as we can't
			// construct a full authentication proof if one is
			// requested.
			if channel.Info.AuthProof == nil {
				continue
			}

			//nolint:ll
			chanAnn, edge1, edge2, err := netann.CreateChanAnnouncement(
				channel.Info.AuthProof, channel.Info,
				channel.Policy1, channel.Policy2,
			)
			if err != nil {
				if !yield(nil, err) {
					return
				}

				continue
			}

			if !yield(chanAnn, nil) {
				return
			}

			// We don't want to send channel updates that don't
			// conform to the spec (anymore), so check to make sure
			// that these channel updates are valid before yielding
			// them.
			if edge1 != nil {
				err := netann.ValidateChannelUpdateFields(
					0, edge1,
				)
				if err != nil {
					log.Errorf("not sending invalid "+
						"channel update %v: %v",
						edge1, err)
				} else if !yield(edge1, nil) {
					return
				}
			}
			if edge2 != nil {
				err := netann.ValidateChannelUpdateFields(
					0, edge2,
				)
				if err != nil {
					log.Errorf("not sending invalid "+
						"channel update %v: %v", edge2,
						err)
				} else if !yield(edge2, nil) {
					return
				}
			}
		}

		// Next, we'll send out all the node announcements that have an
		// update within the horizon as well. We send these second to
		// ensure that they follow any active channels they have.
		nodeAnnsInHorizon := c.graph.NodeUpdatesInHorizon(
			startTime, endTime, graphdb.WithIterPublicNodesOnly(),
		)
		for nodeAnn, err := range nodeAnnsInHorizon {
			if err != nil {
				yield(nil, err)
				return
			}
			nodeUpdate, err := nodeAnn.NodeAnnouncement(true)
			if err != nil {
				if !yield(nil, err) {
					return
				}

				continue
			}

			if !yield(nodeUpdate, nil) {
				return
			}
		}
	}
}

// FilterKnownChanIDs takes a target chain, and a set of channel ID's, and
// returns a filtered set of chan ID's. This filtered set of chan ID's
// represents the ID's that we don't know of which were in the passed superSet.
//
// NOTE: This is part of the ChannelGraphTimeSeries interface.
func (c *ChanSeries) FilterKnownChanIDs(_ chainhash.Hash,
	superSet []graphdb.ChannelUpdateInfo,
	isZombieChan func(time.Time, time.Time) bool) (
	[]lnwire.ShortChannelID, error) {

	newChanIDs, err := c.graph.FilterKnownChanIDs(superSet, isZombieChan)
	if err != nil {
		return nil, err
	}

	filteredIDs := make([]lnwire.ShortChannelID, 0, len(newChanIDs))
	for _, chanID := range newChanIDs {
		filteredIDs = append(
			filteredIDs, lnwire.NewShortChanIDFromInt(chanID),
		)
	}

	return filteredIDs, nil
}

// FilterChannelRange returns the set of channels that we created between the
// start height and the end height. The channel IDs are grouped by their common
// block height. We'll use this respond to a remote peer's QueryChannelRange
// message.
//
// NOTE: This is part of the ChannelGraphTimeSeries interface.
func (c *ChanSeries) FilterChannelRange(_ chainhash.Hash, startHeight,
	endHeight uint32, withTimestamps bool) ([]graphdb.BlockChannelRange,
	error) {

	return c.graph.FilterChannelRange(
		startHeight, endHeight, withTimestamps,
	)
}

// FetchChanAnns returns a full set of channel announcements as well as their
// updates that match the set of specified short channel ID's.  We'll use this
// to reply to a QueryShortChanIDs message sent by a remote peer. The response
// will contain a unique set of ChannelAnnouncements, the latest ChannelUpdate
// for each of the announcements, and a unique set of NodeAnnouncements.
// Invalid node announcements are skipped and logged for debugging purposes.
//
// NOTE: This is part of the ChannelGraphTimeSeries interface.
func (c *ChanSeries) FetchChanAnns(chain chainhash.Hash,
	shortChanIDs []lnwire.ShortChannelID) ([]lnwire.Message, error) {

	chanIDs := make([]uint64, 0, len(shortChanIDs))
	for _, chanID := range shortChanIDs {
		chanIDs = append(chanIDs, chanID.ToUint64())
	}

	channels, err := c.graph.FetchChanInfos(chanIDs)
	if err != nil {
		return nil, err
	}

	// We'll use this map to ensure we don't send the same node
	// announcement more than one time as one node may have many channel
	// anns we'll need to send.
	nodePubsSent := make(map[route.Vertex]struct{})

	chanAnns := make([]lnwire.Message, 0, len(channels)*3)
	for _, channel := range channels {
		// If the channel doesn't have an authentication proof, then we
		// won't send it over as it may not yet be finalized, or be a
		// non-advertised channel.
		if channel.Info.AuthProof == nil {
			continue
		}

		chanAnn, edge1, edge2, err := netann.CreateChanAnnouncement(
			channel.Info.AuthProof, channel.Info, channel.Policy1,
			channel.Policy2,
		)
		if err != nil {
			return nil, err
		}

		chanAnns = append(chanAnns, chanAnn)
		if edge1 != nil {
			chanAnns = append(chanAnns, edge1)

			// If this edge has a validated node announcement, that
			// we haven't yet sent, then we'll send that as well.
			nodePub := channel.Node2.PubKeyBytes
			hasNodeAnn := channel.Node2.HaveAnnouncement()
			if _, ok := nodePubsSent[nodePub]; !ok && hasNodeAnn {
				nodeAnn, err := channel.Node2.NodeAnnouncement(
					true,
				)
				if err != nil {
					return nil, err
				}

				err = netann.ValidateNodeAnnFields(nodeAnn)
				if err != nil {
					log.Debugf("Skipping forwarding "+
						"invalid node announcement "+
						"%x: %v", nodeAnn.NodeID, err)
				} else {
					chanAnns = append(chanAnns, nodeAnn)
					nodePubsSent[nodePub] = struct{}{}
				}
			}
		}
		if edge2 != nil {
			chanAnns = append(chanAnns, edge2)

			// If this edge has a validated node announcement, that
			// we haven't yet sent, then we'll send that as well.
			nodePub := channel.Node1.PubKeyBytes
			hasNodeAnn := channel.Node1.HaveAnnouncement()
			if _, ok := nodePubsSent[nodePub]; !ok && hasNodeAnn {
				nodeAnn, err := channel.Node1.NodeAnnouncement(
					true,
				)
				if err != nil {
					return nil, err
				}

				err = netann.ValidateNodeAnnFields(nodeAnn)
				if err != nil {
					log.Debugf("Skipping forwarding "+
						"invalid node announcement "+
						"%x: %v", nodeAnn.NodeID, err)
				} else {
					chanAnns = append(chanAnns, nodeAnn)
					nodePubsSent[nodePub] = struct{}{}
				}
			}
		}
	}

	return chanAnns, nil
}

// FetchChanUpdates returns the latest channel update messages for the
// specified short channel ID. If no channel updates are known for the channel,
// then an empty slice will be returned.
//
// NOTE: This is part of the ChannelGraphTimeSeries interface.
func (c *ChanSeries) FetchChanUpdates(chain chainhash.Hash,
	shortChanID lnwire.ShortChannelID) ([]*lnwire.ChannelUpdate1, error) {

	chanInfo, e1, e2, err := c.graph.FetchChannelEdgesByID(
		shortChanID.ToUint64(),
	)
	if err != nil {
		return nil, err
	}

	chanUpdates := make([]*lnwire.ChannelUpdate1, 0, 2)
	if e1 != nil {
		chanUpdate, err := netann.ChannelUpdateFromEdge(chanInfo, e1)
		if err != nil {
			return nil, err
		}

		chanUpdates = append(chanUpdates, chanUpdate)
	}
	if e2 != nil {
		chanUpdate, err := netann.ChannelUpdateFromEdge(chanInfo, e2)
		if err != nil {
			return nil, err
		}

		chanUpdates = append(chanUpdates, chanUpdate)
	}

	return chanUpdates, nil
}

// A compile-time assertion to ensure that ChanSeries meets the
// ChannelGraphTimeSeries interface.
var _ ChannelGraphTimeSeries = (*ChanSeries)(nil)
