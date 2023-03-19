package discovery

import (
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/netann"
	"github.com/lightningnetwork/lnd/routing"
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
	HighestChanID(chain chainhash.Hash) (*lnwire.ShortChannelID, error)

	// UpdatesInHorizon returns all known channel and node updates with an
	// update timestamp between the start time and end time. We'll use this
	// to catch up a remote node to the set of channel updates that they
	// may have missed out on within the target chain.
	UpdatesInHorizon(chain chainhash.Hash,
		startTime time.Time, endTime time.Time) ([]lnwire.Message, error)

	// FilterKnownChanIDs takes a target chain, and a set of channel ID's,
	// and returns a filtered set of chan ID's. This filtered set of chan
	// ID's represents the ID's that we don't know of which were in the
	// passed superSet.
	FilterKnownChanIDs(chain chainhash.Hash,
		superSet []lnwire.ShortChannelID) ([]lnwire.ShortChannelID, error)

	// FilterChannelRange returns the set of channels that we created
	// between the start height and the end height. The channel IDs are
	// grouped by their common block height. We'll use this to to a remote
	// peer's QueryChannelRange message.
	FilterChannelRange(chain chainhash.Hash,
		startHeight, endHeight uint32) ([]channeldb.BlockChannelRange, error)

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
		shortChanID lnwire.ShortChannelID) ([]*lnwire.ChannelUpdate, error)
}

// ChanSeries is an implementation of the ChannelGraphTimeSeries
// interface backed by the channeldb ChannelGraph database. We'll provide this
// implementation to the AuthenticatedGossiper so it can properly use the
// in-protocol channel range queries to quickly and efficiently synchronize our
// channel state with all peers.
type ChanSeries struct {
	graph *channeldb.ChannelGraph
}

// NewChanSeries constructs a new ChanSeries backed by a channeldb.ChannelGraph.
// The returned ChanSeries implements the ChannelGraphTimeSeries interface.
func NewChanSeries(graph *channeldb.ChannelGraph) *ChanSeries {
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
func (c *ChanSeries) HighestChanID(chain chainhash.Hash) (*lnwire.ShortChannelID, error) {
	chanID, err := c.graph.HighestChanID()
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
	startTime time.Time, endTime time.Time) ([]lnwire.Message, error) {

	var updates []lnwire.Message

	// First, we'll query for all the set of channels that have an update
	// that falls within the specified horizon.
	chansInHorizon, err := c.graph.ChanUpdatesInHorizon(
		startTime, endTime,
	)
	if err != nil {
		return nil, err
	}
	for _, channel := range chansInHorizon {
		// If the channel hasn't been fully advertised yet, or is a
		// private channel, then we'll skip it as we can't construct a
		// full authentication proof if one is requested.
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

		updates = append(updates, chanAnn)
		if edge1 != nil {
			// We don't want to send channel updates that don't
			// conform to the spec (anymore).
			err := routing.ValidateChannelUpdateFields(0, edge1)
			if err != nil {
				log.Errorf("not sending invalid channel "+
					"update %v: %v", edge1, err)
			} else {
				updates = append(updates, edge1)
			}
		}
		if edge2 != nil {
			err := routing.ValidateChannelUpdateFields(0, edge2)
			if err != nil {
				log.Errorf("not sending invalid channel "+
					"update %v: %v", edge2, err)
			} else {
				updates = append(updates, edge2)
			}
		}
	}

	// Next, we'll send out all the node announcements that have an update
	// within the horizon as well. We send these second to ensure that they
	// follow any active channels they have.
	nodeAnnsInHorizon, err := c.graph.NodeUpdatesInHorizon(
		startTime, endTime,
	)
	if err != nil {
		return nil, err
	}
	for _, nodeAnn := range nodeAnnsInHorizon {
		// Ensure we only forward nodes that are publicly advertised to
		// prevent leaking information about nodes.
		isNodePublic, err := c.graph.IsPublicNode(nodeAnn.PubKeyBytes)
		if err != nil {
			log.Errorf("Unable to determine if node %x is "+
				"advertised: %v", nodeAnn.PubKeyBytes, err)
			continue
		}

		if !isNodePublic {
			log.Tracef("Skipping forwarding announcement for "+
				"node %x due to being unadvertised",
				nodeAnn.PubKeyBytes)
			continue
		}

		nodeUpdate, err := nodeAnn.NodeAnnouncement(true)
		if err != nil {
			return nil, err
		}

		updates = append(updates, nodeUpdate)
	}

	return updates, nil
}

// FilterKnownChanIDs takes a target chain, and a set of channel ID's, and
// returns a filtered set of chan ID's. This filtered set of chan ID's
// represents the ID's that we don't know of which were in the passed superSet.
//
// NOTE: This is part of the ChannelGraphTimeSeries interface.
func (c *ChanSeries) FilterKnownChanIDs(chain chainhash.Hash,
	superSet []lnwire.ShortChannelID) ([]lnwire.ShortChannelID, error) {

	chanIDs := make([]uint64, 0, len(superSet))
	for _, chanID := range superSet {
		chanIDs = append(chanIDs, chanID.ToUint64())
	}

	newChanIDs, err := c.graph.FilterKnownChanIDs(chanIDs)
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
func (c *ChanSeries) FilterChannelRange(chain chainhash.Hash,
	startHeight, endHeight uint32) ([]channeldb.BlockChannelRange, error) {

	return c.graph.FilterChannelRange(startHeight, endHeight)
}

// FetchChanAnns returns a full set of channel announcements as well as their
// updates that match the set of specified short channel ID's.  We'll use this
// to reply to a QueryShortChanIDs message sent by a remote peer. The response
// will contain a unique set of ChannelAnnouncements, the latest ChannelUpdate
// for each of the announcements, and a unique set of NodeAnnouncements.
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
			nodePub := channel.Policy1.Node.PubKeyBytes
			hasNodeAnn := channel.Policy1.Node.HaveNodeAnnouncement
			if _, ok := nodePubsSent[nodePub]; !ok && hasNodeAnn {
				nodeAnn, err := channel.Policy1.Node.NodeAnnouncement(true)
				if err != nil {
					return nil, err
				}

				chanAnns = append(chanAnns, nodeAnn)
				nodePubsSent[nodePub] = struct{}{}
			}
		}
		if edge2 != nil {
			chanAnns = append(chanAnns, edge2)

			// If this edge has a validated node announcement, that
			// we haven't yet sent, then we'll send that as well.
			nodePub := channel.Policy2.Node.PubKeyBytes
			hasNodeAnn := channel.Policy2.Node.HaveNodeAnnouncement
			if _, ok := nodePubsSent[nodePub]; !ok && hasNodeAnn {
				nodeAnn, err := channel.Policy2.Node.NodeAnnouncement(true)
				if err != nil {
					return nil, err
				}

				chanAnns = append(chanAnns, nodeAnn)
				nodePubsSent[nodePub] = struct{}{}
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
	shortChanID lnwire.ShortChannelID) ([]*lnwire.ChannelUpdate, error) {

	chanInfo, e1, e2, err := c.graph.FetchChannelEdgesByID(
		shortChanID.ToUint64(),
	)
	if err != nil {
		return nil, err
	}

	chanUpdates := make([]*lnwire.ChannelUpdate, 0, 2)
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
