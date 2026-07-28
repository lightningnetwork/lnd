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

	// held is the part of the balance that in-flight htlcs have reserved
	// but not settled yet. It is always at most the balance, and it is
	// zero unless a payment is running with hold semantics.
	held lnwire.MilliSatoshi
}

// available returns the liquidity this end can put behind a new htlc: its
// balance less whatever the htlcs already in flight over it hold.
func (e *simChannelEnd) available() lnwire.MilliSatoshi {
	return e.balance - e.held
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

	// holds tracks the liquidity reservations of the htlcs that have
	// traversed the network but are not settled yet, keyed by hold id.
	// It is empty unless a payment is running with hold semantics.
	holds map[uint64][]balanceMove

	// nextHoldID hands out hold ids; zero is never used so that it can
	// stand for "no hold".
	nextHoldID uint64

	// policyStats counts the htlcs the announced min and max htlc limits
	// refused, over the whole life of the network.
	policyStats SimPolicyStats
}

// SimPolicyStats counts the forwarding refusals that announced htlc limits
// caused. It is the running half of stage A's manipulation check: a maximum
// htlc violation comes back as a plain TemporaryChannelFailure, which is
// exactly what a depleted channel returns, so without a counter a tier whose
// ceilings bind constantly is indistinguishable in the output from one whose
// ceilings never bind at all.
type SimPolicyStats struct {
	// MinHtlcRefusals is how many htlcs were refused for falling under an
	// announced minimum.
	MinHtlcRefusals int `json:"htlc_min_refusals,omitempty"`

	// MaxHtlcRefusals is how many were refused for exceeding an announced
	// maximum.
	MaxHtlcRefusals int `json:"htlc_max_refusals,omitempty"`

	// SourceRefusals is how many of the two above happened at the sender's
	// own first hop, which can only occur while the source's announced
	// policy is being enforced.
	SourceRefusals int `json:"htlc_source_refusals,omitempty"`
}

// PolicyStats reports the htlc limit refusals the network has handed out.
func (g *SimGraph) PolicyStats() SimPolicyStats {
	return g.policyStats
}

// simLimitViolation says which announced htlc limit an amount falls foul of.
type simLimitViolation uint8

const (
	// simLimitNone is an amount both limits accept.
	simLimitNone simLimitViolation = iota

	// simLimitBelowMin is an amount under the announced minimum.
	simLimitBelowMin

	// simLimitAboveMax is an amount over the announced maximum.
	simLimitAboveMax
)

// checkHtlcLimits reports which of a policy's announced htlc limits the given
// amount violates, if either. The order matters and matches checkPolicy's: an
// amount that is somehow both under the floor and over the ceiling is reported
// as under the floor, since that is the failure a real node would return.
func checkHtlcLimits(policy *SimPolicy,
	amt lnwire.MilliSatoshi) simLimitViolation {

	if amt < policy.MinHTLCMsat {
		return simLimitBelowMin
	}

	if policy.MaxHTLCMsat != 0 && amt > policy.MaxHTLCMsat {
		return simLimitAboveMax
	}

	return simLimitNone
}

// limitFailure is the wire failure a node returns for an htlc limit
// violation, and nil for an amount both limits accept. A floor violation says
// what it is; a ceiling violation comes back as a plain temporary channel
// failure, which is what lnd's own link returns for one and what a depleted
// channel returns too. The sender cannot tell those two apart, which is
// exactly why the refusals are counted.
func limitFailure(violation simLimitViolation,
	amt lnwire.MilliSatoshi) lnwire.FailureMessage {

	var emptyUpdate lnwire.ChannelUpdate1

	switch violation {
	case simLimitBelowMin:
		return lnwire.NewAmountBelowMinimum(amt, emptyUpdate)

	case simLimitAboveMax:
		return lnwire.NewTemporaryChannelFailure(nil)
	}

	return nil
}

// countLimitRefusal records one htlc that an announced limit turned away.
// Anything that is not a limit violation is somebody else's failure and is
// deliberately not counted here.
func (g *SimGraph) countLimitRefusal(violation simLimitViolation,
	atSource bool) {

	switch violation {
	case simLimitBelowMin:
		g.policyStats.MinHtlcRefusals++

	case simLimitAboveMax:
		g.policyStats.MaxHtlcRefusals++

	default:
		return
	}

	if atSource {
		g.policyStats.SourceRefusals++
	}
}

// NewSimGraph instantiates an empty simulated network.
func NewSimGraph() *SimGraph {
	return &SimGraph{
		nodes:    make(map[route.Vertex]*SimNode),
		channels: make(map[uint64]*SimChannel),
		holds:    make(map[uint64][]balanceMove),
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
		balances[channel.ID] = channel.end(pubKey).available()
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

// balanceMove records a single hop's liquidity commitment so that it can be
// unwound when a downstream hop fails, and so that a held htlc can later be
// settled or released as a unit.
type balanceMove struct {
	from *simChannelEnd
	to   *simChannelEnd
	amt  lnwire.MilliSatoshi
}

// apply moves the amount across the channel: the settlement of one hop.
func (m *balanceMove) apply() {
	m.from.balance -= m.amt
	m.to.balance += m.amt
}

// unapply undoes apply.
func (m *balanceMove) unapply() {
	m.from.balance += m.amt
	m.to.balance -= m.amt
}

// reserve holds the amount on the sending end, taking it out of the
// liquidity available to every other htlc without moving it yet.
func (m *balanceMove) reserve() {
	m.from.held += m.amt
}

// unreserve gives back the reservation made by reserve.
func (m *balanceMove) unreserve() {
	m.from.held -= m.amt
}

// simCommitMode selects what a route walk does with the liquidity it
// traverses.
type simCommitMode uint8

const (
	// simCommitSettle moves the balance of every hop as the htlc passes
	// it, the instantly settling htlc the simulator has always used.
	simCommitSettle simCommitMode = iota

	// simCommitHold only reserves the outgoing liquidity of every hop,
	// leaving the balances untouched until the resulting hold is settled
	// or released. This is the htlc a receiver sits on while it waits for
	// the rest of an mpp set to arrive.
	simCommitHold
)

// SendHtlc sends an htlc along the given route through the simulated
// network and synchronously returns its resolution. Forwarding applies the
// same policy checks a real node would: disabled channels, min/max htlc
// limits, fee sufficiency, cltv deltas and (hidden) liquidity.
func (g *SimGraph) SendHtlc(rt *route.Route) (SimHtlcResult, error) {
	result, _, err := g.walkHtlc(rt, simCommitSettle)

	return result, err
}

// HoldHtlc sends an htlc along the given route but stops short of settling
// it: each hop reserves its outgoing liquidity instead of moving it, so
// sibling shards and background traffic see the reduced availability while
// the htlc is in flight. A settled resolution returns a non-zero hold id that
// must eventually be passed to either SettleHold or ReleaseHold. A failure
// leaves nothing reserved and returns a zero hold id.
func (g *SimGraph) HoldHtlc(rt *route.Route) (SimHtlcResult, uint64, error) {
	result, moves, err := g.walkHtlc(rt, simCommitHold)
	if err != nil || result.Failure != nil {
		return result, 0, err
	}

	g.nextHoldID++
	id := g.nextHoldID
	g.holds[id] = moves

	return result, id, nil
}

// SettleHold turns a hold into real balance movement: every reservation
// becomes the transfer the settling htlc would have made all along, which
// pays each forwarding node the difference between what it received and what
// it sent on. Settling an unknown hold is a no-op.
func (g *SimGraph) SettleHold(id uint64) {
	moves, ok := g.holds[id]
	if !ok {
		return
	}
	delete(g.holds, id)

	for i := range moves {
		moves[i].unreserve()
		moves[i].apply()
	}
}

// ReleaseHold cancels a hold: the reserved liquidity becomes available again
// and no balance moves at all, so an htlc that is never settled leaves the
// network exactly as it found it. Releasing an unknown hold is a no-op.
func (g *SimGraph) ReleaseHold(id uint64) {
	moves, ok := g.holds[id]
	if !ok {
		return
	}
	delete(g.holds, id)

	for i := range moves {
		moves[i].unreserve()
	}
}

// walkHtlc walks an htlc along the given route, applying the same policy and
// liquidity checks a real forwarding node would. The commit mode decides what
// happens to the liquidity of each hop it clears: simCommitSettle moves it,
// simCommitHold merely reserves it and hands the reservations back so that
// the caller can settle or release them later. Either way a failure part of
// the way down the route unwinds everything committed so far, so the graph is
// never left holding a half-forwarded htlc.
func (g *SimGraph) walkHtlc(rt *route.Route,
	mode simCommitMode) (SimHtlcResult, []balanceMove, error) {

	var moves []balanceMove

	// commit applies one hop's liquidity commitment in the current mode.
	commit := func(m *balanceMove) {
		if mode == simCommitHold {
			m.reserve()
			return
		}

		m.apply()
	}

	// revert unwinds all liquidity commitments applied so far.
	revert := func() {
		for i := range moves {
			if mode == simCommitHold {
				moves[i].unreserve()
				continue
			}

			moves[i].unapply()
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
			return SimHtlcResult{}, nil, fmt.Errorf("unknown "+
				"channel %v in route", routeHop.ChannelID)
		}

		sendingEnd := channel.end(prevNode)
		receivingEnd := channel.otherEnd(prevNode)
		if sendingEnd == nil ||
			receivingEnd.owner != routeHop.PubKeyBytes {

			revert()
			return SimHtlcResult{}, nil, fmt.Errorf("channel %v "+
				"does not connect %v to %v", routeHop.ChannelID,
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
				g.countLimitRefusal(
					checkHtlcLimits(policy, amtOut), false,
				)
				revert()
				return SimHtlcResult{
					FailureSource: prevNode,
					Failure:       failure,
				}, nil, nil
			}
		}

		// Liquidity check: the sending end must have the outgoing
		// amount available. Liquidity that an in-flight htlc already
		// holds does not count, so sibling shards and background
		// payments contend for the same balance. This is the hidden
		// state that path finding is trying to predict.
		if sendingEnd.available() < amtOut {
			revert()
			return SimHtlcResult{
				FailureSource: prevNode,
				Failure: lnwire.NewTemporaryChannelFailure(
					nil,
				),
			}, nil, nil
		}

		// Commit the hop's liquidity and record it so that it can be
		// unwound, settled or released later.
		move := balanceMove{
			from: sendingEnd,
			to:   receivingEnd,
			amt:  amtOut,
		}
		commit(&move)
		moves = append(moves, move)

		// Advance to the next hop.
		amtIn = amtOut
		expiryIn = expiryOut
		prevNode = routeHop.PubKeyBytes
	}

	// All hops succeeded: the htlc has reached the final node, either
	// settled outright or held there pending its siblings.
	return SimHtlcResult{}, moves, nil
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

	if failure := limitFailure(
		checkHtlcLimits(policy, amtOut), amtOut,
	); failure != nil {

		return failure
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
