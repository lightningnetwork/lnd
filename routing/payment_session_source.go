package routing

import (
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/lightningnetwork/lnd/zpay32"
)

// A compile time assertion to ensure SessionSource meets the
// PaymentSessionSource interface.
var _ PaymentSessionSource = (*SessionSource)(nil)

// SessionSource defines a source for the router to retrieve new payment
// sessions.
type SessionSource struct {
	// GraphSessionFactory can be used to gain access to a Graph session.
	// If the backing DB allows it, this will mean that a read transaction
	// is being held during the use of the session.
	GraphSessionFactory GraphSessionFactory

	// SourceNode is the graph's source node.
	SourceNode *models.Node

	// GetLink is a method that allows querying the lower link layer
	// to determine the up to date available bandwidth at a prospective link
	// to be traversed. If the link isn't available, then a value of zero
	// should be returned. Otherwise, the current up to date knowledge of
	// the available bandwidth of the link should be returned.
	GetLink getLinkQuery

	// MissionControl is a shared memory of sorts that executions of payment
	// path finding use in order to remember which vertexes/edges were
	// pruned from prior attempts. During payment execution, errors sent by
	// nodes are mapped into a vertex or edge to be pruned. Each run will
	// then take into account this set of pruned vertexes/edges to reduce
	// route failure and pass on graph information gained to the next
	// execution.
	MissionControl MissionControlQuerier

	// PathFindingConfig defines global parameters that control the
	// trade-off in path finding between fees and probability.
	PathFindingConfig PathFindingConfig
}

// NewPaymentSession creates a new payment session backed by the latest prune
// view from Mission Control. An optional set of routing hints can be provided
// in order to populate additional edges to explore when finding a path to the
// payment's destination.
func (m *SessionSource) NewPaymentSession(p *LightningPayment,
	firstHopBlob fn.Option[tlv.Blob],
	trafficShaper fn.Option[htlcswitch.AuxTrafficShaper]) (PaymentSession,
	error) {

	getBandwidthHints := func(graph Graph) (bandwidthHints, error) {
		return newBandwidthManager(
			graph, m.SourceNode.PubKeyBytes, m.GetLink,
			firstHopBlob, trafficShaper,
		)
	}

	session, err := newPaymentSession(
		p, m.SourceNode.PubKeyBytes, getBandwidthHints,
		m.GraphSessionFactory, m.MissionControl, m.PathFindingConfig,
	)
	if err != nil {
		return nil, err
	}

	return session, nil
}

// NewPaymentSessionEmpty creates a new paymentSession instance that is empty,
// and will be exhausted immediately. Used for failure reporting to
// missioncontrol for resumed payment we don't want to make more attempts for.
func (m *SessionSource) NewPaymentSessionEmpty() PaymentSession {
	return &paymentSession{
		empty: true,
	}
}

// RouteHintsToEdges converts a list of invoice route hints to an edge map that
// can be passed into pathfinding.
func RouteHintsToEdges(routeHints [][]zpay32.HopHint, target route.Vertex) (
	map[route.Vertex][]AdditionalEdge, error) {

	edges := make(map[route.Vertex][]AdditionalEdge)

	// Traverse through all of the available hop hints and include them in
	// our edges map, indexed by the public key of the channel's starting
	// node.
	for _, routeHint := range routeHints {
		// If multiple hop hints are provided within a single route
		// hint, we'll assume they must be chained together and sorted
		// in forward order in order to reach the target successfully.
		for i, hopHint := range routeHint {
			// In order to determine the end node of this hint,
			// we'll need to look at the next hint's start node. If
			// we've reached the end of the hints list, we can
			// assume we've reached the destination.
			endNode := target
			if i != len(routeHint)-1 {
				nodeID := routeHint[i+1].NodeID
				copy(
					endNode[:],
					nodeID.SerializeCompressed(),
				)
			}

			// Finally, create the channel edge from the hop hint
			// and add it to list of edges corresponding to the node
			// at the start of the channel.
			edgePolicy := &models.CachedEdgePolicy{
				ToNodePubKey: func() route.Vertex {
					return endNode
				},
				ToNodeFeatures: lnwire.EmptyFeatureVector(),
				ChannelID:      hopHint.ChannelID,
				FeeBaseMSat: lnwire.MilliSatoshi(
					hopHint.FeeBaseMSat,
				),
				FeeProportionalMillionths: lnwire.MilliSatoshi(
					hopHint.FeeProportionalMillionths,
				),
				TimeLockDelta: hopHint.CLTVExpiryDelta,
			}

			edge := &PrivateEdge{
				policy: edgePolicy,
			}

			v := route.NewVertex(hopHint.NodeID)
			edges[v] = append(edges[v], edge)
		}
	}

	return edges, nil
}
