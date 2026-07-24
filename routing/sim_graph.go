package routing

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

// SimPolicy describes the forwarding policy for one direction of a simulated
// channel. It mirrors the fields of a BOLT 7 channel update that are relevant
// for path finding and forwarding.
type SimPolicy struct {
	// BaseFeeMsat is the flat fee charged for forwarding an HTLC in this
	// direction.
	BaseFeeMsat lnwire.MilliSatoshi

	// FeeRatePPM is the proportional fee in parts per million charged for
	// forwarding an HTLC in this direction.
	FeeRatePPM lnwire.MilliSatoshi

	// TimeLockDelta is the CLTV delta this node requires when forwarding
	// in this direction.
	TimeLockDelta uint16

	// MinHTLCMsat is the smallest HTLC that will be forwarded in this
	// direction.
	MinHTLCMsat lnwire.MilliSatoshi

	// MaxHTLCMsat is the largest HTLC that will be forwarded in this
	// direction. A value of zero means no maximum is enforced.
	MaxHTLCMsat lnwire.MilliSatoshi

	// Disabled indicates that forwarding in this direction is disabled.
	Disabled bool
}

// fee returns the total routing fee this policy charges for forwarding the
// given amount.
func (p *SimPolicy) fee(amt lnwire.MilliSatoshi) lnwire.MilliSatoshi {
	return p.BaseFeeMsat + amt*p.FeeRatePPM/1_000_000
}

// simChannelEnd holds the state of one side of a simulated channel: the
// hidden true outbound liquidity and the policy this side announced.
type simChannelEnd struct {
	owner   route.Vertex
	balance lnwire.MilliSatoshi
	policy  SimPolicy
}

// SimChannel is a single channel in the simulated network. The two ends are
// ordered lexicographically by pubkey, matching gossip conventions.
type SimChannel struct {
	// ID is the short channel id of the channel.
	ID uint64

	// Capacity is the total capacity of the channel.
	Capacity btcutil.Amount

	// ends holds the two directional ends, index 0 being the
	// lexicographically smaller pubkey (node1).
	ends [2]simChannelEnd
}

// end returns the channel end owned by the given node, or nil if the node is
// not a party to this channel.
func (c *SimChannel) end(v route.Vertex) *simChannelEnd {
	switch v {
	case c.ends[0].owner:
		return &c.ends[0]
	case c.ends[1].owner:
		return &c.ends[1]
	default:
		return nil
	}
}

// otherEnd returns the channel end not owned by the given node.
func (c *SimChannel) otherEnd(v route.Vertex) *simChannelEnd {
	switch v {
	case c.ends[0].owner:
		return &c.ends[1]
	case c.ends[1].owner:
		return &c.ends[0]
	default:
		return nil
	}
}

// SimNode is a node in the simulated network.
type SimNode struct {
	// PubKey is the node's identity key.
	PubKey route.Vertex

	// Alias is an optional human readable name for the node.
	Alias string

	// channels holds all channels this node is a party to.
	channels []*SimChannel
}

// SimGraph is an in-memory Lightning Network with hidden channel balances.
// It implements both the Graph and GraphSessionFactory interfaces so that
// the production path finding and mission control code can run against it
// unmodified, while sendHtlc simulates the actual forwarding semantics
// (policy checks, liquidity checks) of the network.
type SimGraph struct {
	nodes    map[route.Vertex]*SimNode
	channels map[uint64]*SimChannel
}

// NewSimGraph instantiates an empty simulated network.
func NewSimGraph() *SimGraph {
	return &SimGraph{
		nodes:    make(map[route.Vertex]*SimNode),
		channels: make(map[uint64]*SimChannel),
	}
}

// SimNodePubKey deterministically derives a node pubkey from a numeric id.
// The id must be non-zero.
func SimNodePubKey(id uint32) route.Vertex {
	var seed [32]byte
	binary.BigEndian.PutUint32(seed[28:], id)
	_, pub := btcec.PrivKeyFromBytes(seed[:])

	var v route.Vertex
	copy(v[:], pub.SerializeCompressed())

	return v
}

// AddNode adds a node to the network. Adding the same pubkey twice is an
// error.
func (g *SimGraph) AddNode(pubKey route.Vertex, alias string) (*SimNode,
	error) {

	if _, exists := g.nodes[pubKey]; exists {
		return nil, fmt.Errorf("node %v already exists", pubKey)
	}

	node := &SimNode{
		PubKey: pubKey,
		Alias:  alias,
	}
	g.nodes[pubKey] = node

	return node, nil
}

// Node returns the node with the given pubkey, or nil if it doesn't exist.
func (g *SimGraph) Node(pubKey route.Vertex) *SimNode {
	return g.nodes[pubKey]
}

// NumNodes returns the number of nodes in the network.
func (g *SimGraph) NumNodes() int {
	return len(g.nodes)
}

// NumChannels returns the number of channels in the network.
func (g *SimGraph) NumChannels() int {
	return len(g.channels)
}

// AddChannel adds a channel between two existing nodes. The policies are
// given from the perspective of each passed node: policyA is the policy
// nodeA announces for HTLCs it forwards out over this channel. The initial
// balance is a 50/50 split; use a liquidity model to redistribute.
func (g *SimGraph) AddChannel(id uint64, nodeA, nodeB route.Vertex,
	capacity btcutil.Amount, policyA, policyB SimPolicy) error {

	a, ok := g.nodes[nodeA]
	if !ok {
		return fmt.Errorf("unknown node %v", nodeA)
	}
	b, ok := g.nodes[nodeB]
	if !ok {
		return fmt.Errorf("unknown node %v", nodeB)
	}

	if _, exists := g.channels[id]; exists {
		return fmt.Errorf("channel %v already exists", id)
	}

	// Order the ends lexicographically to match gossip conventions.
	endA := simChannelEnd{
		owner:   nodeA,
		balance: lnwire.NewMSatFromSatoshis(capacity / 2),
		policy:  policyA,
	}
	endB := simChannelEnd{
		owner:   nodeB,
		balance: lnwire.NewMSatFromSatoshis(capacity / 2),
		policy:  policyB,
	}

	channel := &SimChannel{
		ID:       id,
		Capacity: capacity,
	}
	if bytes.Compare(nodeA[:], nodeB[:]) < 0 {
		channel.ends = [2]simChannelEnd{endA, endB}
	} else {
		channel.ends = [2]simChannelEnd{endB, endA}
	}

	g.channels[id] = channel
	a.channels = append(a.channels, channel)
	b.channels = append(b.channels, channel)

	return nil
}

// LocalBalances returns the outbound balances of all channels of the given
// node, keyed by channel id. This is used to construct exact bandwidth hints
// for the sender, mirroring a real node's knowledge of its local channels.
func (g *SimGraph) LocalBalances(
	pubKey route.Vertex) map[uint64]lnwire.MilliSatoshi {

	balances := make(map[uint64]lnwire.MilliSatoshi)

	node, ok := g.nodes[pubKey]
	if !ok {
		return balances
	}

	for _, channel := range node.channels {
		balances[channel.ID] = channel.end(pubKey).balance
	}

	return balances
}

// ForEachNodeDirectedChannel calls the callback for every channel of the
// given node, presenting the public gossip view (policies and capacity, not
// balances).
//
// NOTE: Part of the Graph interface.
func (g *SimGraph) ForEachNodeDirectedChannel(_ context.Context,
	nodePub route.Vertex, cb func(channel *graphdb.DirectedChannel) error,
	_ func()) error {

	node, ok := g.nodes[nodePub]
	if !ok {
		return graphdb.ErrGraphNodeNotFound
	}

	for _, channel := range node.channels {
		ourEnd := channel.end(nodePub)
		otherEnd := channel.otherEnd(nodePub)
		inPolicy := otherEnd.policy

		directedChannel := &graphdb.DirectedChannel{
			ChannelID:    channel.ID,
			IsNode1:      nodePub == channel.ends[0].owner,
			OtherNode:    otherEnd.owner,
			Capacity:     channel.Capacity,
			OutPolicySet: !ourEnd.policy.Disabled,
			InPolicy: &models.CachedEdgePolicy{
				ChannelID:     channel.ID,
				IsDisabled:    inPolicy.Disabled,
				TimeLockDelta: inPolicy.TimeLockDelta,
				MinHTLC:       inPolicy.MinHTLCMsat,
				MaxHTLC:       inPolicy.MaxHTLCMsat,
				HasMaxHTLC:    inPolicy.MaxHTLCMsat != 0,
				FeeBaseMSat:   inPolicy.BaseFeeMsat,
				FeeProportionalMillionths: inPolicy.
					FeeRatePPM,
				ToNodePubKey: func() route.Vertex {
					return nodePub
				},
				ToNodeFeatures: lnwire.EmptyFeatureVector(),
			},
		}

		if err := cb(directedChannel); err != nil {
			return err
		}
	}

	return nil
}

// FetchNodeFeatures returns the features of the given node.
//
// NOTE: Part of the Graph interface.
func (g *SimGraph) FetchNodeFeatures(_ context.Context,
	_ route.Vertex) (*lnwire.FeatureVector, error) {

	return lnwire.EmptyFeatureVector(), nil
}

// GraphSession provides the callback with access to the graph.
//
// NOTE: Part of the GraphSessionFactory interface.
func (g *SimGraph) GraphSession(_ context.Context,
	cb func(graph graphdb.NodeTraverser) error, _ func()) error {

	return cb(g)
}

// SimHtlcResult describes the resolution of a simulated htlc. If failure is
// nil, the htlc was settled.
type SimHtlcResult struct {
	// FailureSource is the node that generated the failure.
	FailureSource route.Vertex

	// Failure is the failure message, nil on settlement.
	Failure lnwire.FailureMessage
}

// balanceMove records a single applied balance mutation so that it can be
// unwound when a downstream hop fails.
type balanceMove struct {
	from *simChannelEnd
	to   *simChannelEnd
	amt  lnwire.MilliSatoshi
}

// SendHtlc sends an htlc along the given route through the simulated
// network and synchronously returns its resolution. Forwarding applies the
// same policy checks a real node would: disabled channels, min/max htlc
// limits, fee sufficiency, cltv deltas and (hidden) liquidity.
func (g *SimGraph) SendHtlc(rt *route.Route) (SimHtlcResult, error) {
	var moves []balanceMove

	// revert unwinds all balance mutations applied so far.
	revert := func() {
		for _, m := range moves {
			m.from.balance += m.amt
			m.to.balance -= m.amt
		}
	}

	// The incoming amount and expiry of the htlc arriving at the current
	// forwarding node. For the source these are the route totals.
	amtIn := rt.TotalAmount
	expiryIn := rt.TotalTimeLock
	prevNode := rt.SourcePubKey

	for i, routeHop := range rt.Hops {
		channel, ok := g.channels[routeHop.ChannelID]
		if !ok {
			revert()
			return SimHtlcResult{}, fmt.Errorf("unknown channel "+
				"%v in route", routeHop.ChannelID)
		}

		sendingEnd := channel.end(prevNode)
		receivingEnd := channel.otherEnd(prevNode)
		if sendingEnd == nil ||
			receivingEnd.owner != routeHop.PubKeyBytes {

			revert()
			return SimHtlcResult{}, fmt.Errorf("channel %v does "+
				"not connect %v to %v", routeHop.ChannelID,
				prevNode, routeHop.PubKeyBytes)
		}

		// The amount carried over channel i is the route total for
		// the first channel and the previous hop's amt-to-forward
		// after that. Hops[i].AmtToForward is the amount the *next*
		// node forwards onward, not the amount on this channel.
		amtOut := rt.TotalAmount
		expiryOut := rt.TotalTimeLock
		if i > 0 {
			amtOut = rt.Hops[i-1].AmtToForward
			expiryOut = rt.Hops[i-1].OutgoingTimeLock
		}
		policy := &sendingEnd.policy

		// The source doesn't check its own policy, but intermediate
		// nodes enforce theirs before forwarding.
		if i > 0 {
			failure := checkPolicy(
				policy, amtIn, amtOut, expiryIn, expiryOut,
			)
			if failure != nil {
				revert()
				return SimHtlcResult{
					FailureSource: prevNode,
					Failure:       failure,
				}, nil
			}
		}

		// Liquidity check: the sending end must have the outgoing
		// amount available. This is the hidden state that path
		// finding is trying to predict.
		if sendingEnd.balance < amtOut {
			revert()
			return SimHtlcResult{
				FailureSource: prevNode,
				Failure: lnwire.NewTemporaryChannelFailure(
					nil,
				),
			}, nil
		}

		// Move the balance and record the move for potential unwind.
		sendingEnd.balance -= amtOut
		receivingEnd.balance += amtOut
		moves = append(moves, balanceMove{
			from: sendingEnd,
			to:   receivingEnd,
			amt:  amtOut,
		})

		// Advance to the next hop.
		amtIn = amtOut
		expiryIn = expiryOut
		prevNode = routeHop.PubKeyBytes
	}

	// All hops succeeded, the htlc is settled at the final node.
	return SimHtlcResult{}, nil
}

// checkPolicy applies the forwarding policy checks of a node to an htlc that
// arrives with (amtIn, expiryIn) and is to be forwarded with (amtOut,
// expiryOut). It returns a failure message if any check fails.
func checkPolicy(policy *SimPolicy, amtIn, amtOut lnwire.MilliSatoshi,
	expiryIn, expiryOut uint32) lnwire.FailureMessage {

	var emptyUpdate lnwire.ChannelUpdate1

	if policy.Disabled {
		return lnwire.NewChannelDisabled(0, emptyUpdate)
	}

	if amtOut < policy.MinHTLCMsat {
		return lnwire.NewAmountBelowMinimum(amtOut, emptyUpdate)
	}

	if policy.MaxHTLCMsat != 0 && amtOut > policy.MaxHTLCMsat {
		return lnwire.NewTemporaryChannelFailure(nil)
	}

	if amtIn < amtOut+policy.fee(amtOut) {
		return lnwire.NewFeeInsufficient(amtIn, emptyUpdate)
	}

	if expiryIn < expiryOut+uint32(policy.TimeLockDelta) {
		return lnwire.NewIncorrectCltvExpiry(expiryIn, emptyUpdate)
	}

	return nil
}

// Compile-time interface checks.
var _ Graph = (*SimGraph)(nil)
var _ GraphSessionFactory = (*SimGraph)(nil)
