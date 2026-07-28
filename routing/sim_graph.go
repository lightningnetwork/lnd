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

	// InboundBaseMsat is the flat fee the owner of this end charges for
	// htlcs ARRIVING over this channel, i.e. for flow in the direction
	// OPPOSITE to the one the fields above price. It is negative when the
	// node offers a discount for inbound flow, which is how the field is
	// used in practice: 4,660 of the 4,783 real policies that carry one
	// are discounts.
	//
	// The direction is the one thing worth being careful about. A node
	// announces its inbound fee on its OWN channel update, the same update
	// that carries the outbound fee it charges for sending out over the
	// channel, but the inbound fee applies to htlcs coming the other way.
	// So this field lives on the policy of the node that charges it, which
	// is the end that owns it, and it is charged when that node RECEIVES.
	InboundBaseMsat int32

	// InboundRatePPM is the proportional inbound fee in parts per million,
	// charged by the owner of this end on htlcs arriving over this channel.
	// It is applied to the outgoing amount plus the outgoing fee, not to
	// the incoming amount. See InboundBaseMsat for the direction rule.
	InboundRatePPM int32

	// Disabled indicates that forwarding in this direction is disabled.
	Disabled bool
}

// fee returns the total routing fee this policy charges for forwarding the
// given amount.
func (p *SimPolicy) fee(amt lnwire.MilliSatoshi) lnwire.MilliSatoshi {
	return p.BaseFeeMsat + amt*p.FeeRatePPM/1_000_000
}

// wireInboundFee returns this policy's inbound fee in the wire form lnd's own
// graph cache and path finding pass around.
func (p *SimPolicy) wireInboundFee() lnwire.Fee {
	return lnwire.Fee{
		BaseFee: p.InboundBaseMsat,
		FeeRate: p.InboundRatePPM,
	}
}

// inboundFee returns what the owner of this end charges an htlc arriving over
// this channel, given the amount the receiving node will send onward plus the
// fee it charges for doing so. The arithmetic is lnd's own, called through
// lnd's own type, so that the simulator cannot drift from the production
// rounding rules: positive fees round down, negative fees round up, and the
// rate is capped at ten times the amount to keep the multiplication in range.
//
// The result is signed. A negative return is a discount, and it is the CALLER
// that decides how far a discount may go, since a discount is bounded by the
// outgoing fee it is netted against rather than by anything on this policy.
func (p *SimPolicy) inboundFee(amt lnwire.MilliSatoshi) int64 {
	fee := models.NewInboundFeeFromWire(p.wireInboundFee())

	return fee.CalcFee(amt)
}

// hasInboundFee reports whether this end announces an inbound fee at all.
func (p *SimPolicy) hasInboundFee() bool {
	return p.InboundBaseMsat != 0 || p.InboundRatePPM != 0
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

	// enforceSourceLimits makes the sender's own announced htlc limits
	// bind on the first hop, like every other hop's. It is off unless an
	// htlc_limits section turned it on, which keeps every scenario file
	// written before stage A forwarding exactly as it always did.
	enforceSourceLimits bool

	// inboundFees switches the whole inbound fee mechanism on: forwarding
	// nodes charge what they announced for htlcs arriving over a channel,
	// and the gossip view shows every node's inbound fee. It is off unless
	// an inbound_fees section turned it on.
	//
	// The flag is deliberately separate from the loader's parse. A
	// describegraph snapshot always carries its real inbound fees now, so
	// without this gate the mainnet tier would start pricing them the day
	// the loader learned to read them, and every published mainnet number
	// would move without a scenario file asking for it.
	inboundFees bool
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

	// InboundFeeCharged is how many forwarding hops priced a NON-ZERO
	// inbound fee, whether or not the htlc then cleared. This is the
	// running half of stage B's manipulation check and the one counter
	// here that is not an alarm: it says the mechanism reached the wire.
	// It cannot say the mechanism changed anything, because a discount
	// never moves money on its own. A discount changes what a sender is
	// willing to pay, so its effect is visible in fee_ppm_on_success and
	// in nothing counted here.
	InboundFeeCharged int `json:"inbound_fee_charged,omitempty"`

	// InboundFeeRefusals is how many htlcs were refused for insufficient
	// fee where the inbound fee is the reason: the same htlc would have
	// cleared had the receiving node announced no inbound fee.
	//
	// Read this the way stage A's refusal counters are read. lnd's path
	// finding prices inbound fees before it sends (pathfind.go's
	// processEdge adds the inbound fee to the amount every candidate hop
	// must send), so an lnd arm on an honest tier reports zero here and a
	// non-zero reading means some sender ignored a fee its own gossip view
	// showed it. What it is NOT is a measure of how much the inbound fees
	// of a tier matter; only the static census can say that.
	InboundFeeRefusals int `json:"inbound_fee_refusals,omitempty"`
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

// inboundPolicy returns the policy that supplies the inbound fee of the node
// forwarding at hop i of the given route, or nil if no inbound fee applies.
//
// The node forwarding at hop i is the one the previous hop delivered to, and
// the channel it received over is the previous hop's. Its inbound fee lives on
// its own end of that channel, since a node announces the fee it charges for
// arriving flow in the update for its own outgoing direction. Hop zero is the
// sender, which charges itself nothing.
func (g *SimGraph) inboundPolicy(rt *route.Route, i int,
	node route.Vertex) *SimPolicy {

	if !g.inboundFees || i == 0 {
		return nil
	}

	channel, ok := g.channels[rt.Hops[i-1].ChannelID]
	if !ok {
		return nil
	}

	end := channel.end(node)
	if end == nil {
		return nil
	}

	return &end.policy
}

// countInboundRefusal records an htlc that an inbound fee turned away: a fee
// insufficiency the same htlc would not have hit had the forwarding node
// announced no inbound fee. Any other failure, and any fee insufficiency the
// outbound fee alone explains, belongs to somebody else and is not counted.
func (g *SimGraph) countInboundRefusal(policy, inPolicy *SimPolicy,
	failure lnwire.FailureMessage, amtIn, amtOut lnwire.MilliSatoshi) {

	if inPolicy == nil || !inPolicy.hasInboundFee() {
		return
	}

	if failure.Code() != lnwire.CodeFeeInsufficient {
		return
	}

	outFee, _ := nodeFee(policy, nil, amtOut)
	if amtIn < amtOut+outFee {
		return
	}

	g.policyStats.InboundFeeRefusals++
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
// The inbound fee on each directed channel is the ITERATED node's own, the
// one it charges for htlcs arriving to it over that channel. That is the same
// thing lnd's graph cache puts there (graph/db/graph_cache.go writes it from
// the node's own outgoing update), so lnd's path finding reads it correctly
// with no adaptation, and it is the one unambiguous place a candidate can
// find it. InPolicy.InboundFee is deliberately left unset: on lnd's cache
// that option describes the OTHER node's inbound fee, and a sealed gossip
// view is a protocol surface rather than a replica of one implementation's
// cache. See the SimNetworkView contract for the convention as candidates
// are told it.
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

		// While the mechanism is off the field stays at the zero fee it
		// has carried for the whole program, so a graph loaded from a
		// snapshot full of real inbound fees still presents the view
		// every published number was measured against.
		var inboundFee lnwire.Fee
		if g.inboundFees {
			inboundFee = ourEnd.policy.wireInboundFee()
		}

		directedChannel := &graphdb.DirectedChannel{
			ChannelID:    channel.ID,
			IsNode1:      nodePub == channel.ends[0].owner,
			OtherNode:    otherEnd.owner,
			Capacity:     channel.Capacity,
			OutPolicySet: !ourEnd.policy.Disabled,
			InboundFee:   inboundFee,
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
	// chanID is the channel the hop crossed. The ends alone identify the
	// liquidity, but not the channel it sits in, and a hold has to be
	// readable as "this payment is reserving THIS directed edge" for the
	// runner to attribute one of the sender's payments contending with
	// another of its own.
	chanID uint64

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

// simHoldEdge names one directed channel end that an in-flight htlc reserves
// liquidity on. It is the key self contention is attributed by: the graph owns
// the holds map, so it can say exactly which directed edge a payment is
// sitting on while another one asks for it.
type simHoldEdge struct {
	// ChanID is the channel the reservation is on.
	ChanID uint64

	// From is the node whose outbound side is reserved. A reservation is
	// directional because liquidity is.
	From route.Vertex
}

// simHoldReservation is one directed edge a hold reserves liquidity on, and
// how much of it.
type simHoldReservation struct {
	edge simHoldEdge
	amt  lnwire.MilliSatoshi
}

// holdReservations returns what the given hold has reserved, edge by edge. It
// is runner-side truth: the sealed gossip view exposes none of it, and the
// runner needs it to tell which of the sender's own payments is sitting on the
// liquidity another one is asking for.
func (g *SimGraph) holdReservations(id uint64) []simHoldReservation {
	moves, ok := g.holds[id]
	if !ok {
		return nil
	}

	reservations := make([]simHoldReservation, 0, len(moves))
	for i := range moves {
		reservations = append(reservations, simHoldReservation{
			edge: simHoldEdge{
				ChanID: moves[i].chanID,
				From:   moves[i].from.owner,
			},
			amt: moves[i].amt,
		})
	}

	return reservations
}

// endLiquidity returns the hidden balance of one directed channel end and how
// much of it in-flight htlcs currently hold, reporting false when the channel
// does not exist or the named node is not a party to it.
//
// This is the hidden state path finding exists to predict, so nothing on the
// sealed view exposes it. The runner reads it to answer one question that only
// the truth can answer: whether an htlc that failed for want of liquidity would
// have cleared if the sender's other payments had not been holding some.
func (g *SimGraph) endLiquidity(chanID uint64,
	owner route.Vertex) (lnwire.MilliSatoshi, lnwire.MilliSatoshi, bool) {

	channel, ok := g.channels[chanID]
	if !ok {
		return 0, 0, false
	}

	end := channel.end(owner)
	if end == nil {
		return 0, 0, false
	}

	return end.balance, end.held, true
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

		// The forwarding node's inbound fee is announced on its OWN end
		// of the channel the htlc arrived over, which is the channel of
		// the previous hop. The sender pays no inbound fee to itself,
		// and the exit hop is never a forwarding node here, so this
		// resolves to nil at exactly the two hops lnd's path finding
		// exempts (pathfind.go passes !isExitHop to the edge unifier,
		// and the source is never a pivot).
		inPolicy := g.inboundPolicy(rt, i, prevNode)
		if inPolicy != nil && inPolicy.hasInboundFee() {
			g.policyStats.InboundFeeCharged++
		}

		// Intermediate nodes enforce their whole announced policy
		// before forwarding.
		if i > 0 {
			failure := checkPolicy(
				policy, inPolicy, amtIn, amtOut, expiryIn,
				expiryOut,
			)
			if failure != nil {
				g.countLimitRefusal(
					checkHtlcLimits(policy, amtOut), false,
				)
				g.countInboundRefusal(
					policy, inPolicy, failure, amtIn, amtOut,
				)
				revert()
				return SimHtlcResult{
					FailureSource: prevNode,
					Failure:       failure,
				}, nil, nil
			}
		}

		// The sender stays exempt from its own fee and its own timelock
		// delta, neither of which it pays or grants to itself, so those
		// two checks are unsatisfiable at hop zero by construction. Its
		// announced htlc LIMITS are a different matter: under stage A
		// they bind on the first hop exactly like every other hop's.
		// That is the rule lnd's own local edge selection already
		// applies, since getEdgeLocal runs amtInRange on the source's
		// policy (checking min and max htlc, and ignoring the disabled
		// flag) before it will build a route over a local channel.
		// Enforcing it here removes a special case rather than adding
		// one, and it keeps the wire from carrying an htlc lnd's
		// pathfinder refused to plan.
		if i == 0 && g.enforceSourceLimits {
			violation := checkHtlcLimits(policy, amtOut)

			failure := limitFailure(violation, amtOut)
			if failure != nil {
				g.countLimitRefusal(violation, true)
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
			chanID: channel.ID,
			from:   sendingEnd,
			to:     receivingEnd,
			amt:    amtOut,
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

// nodeFee returns the total fee a forwarding node requires for sending amtOut
// over the policy it announced on its outgoing channel, given the policy it
// announced on the channel the htlc ARRIVED over. A nil inPolicy is a node
// that charges nothing for inbound flow, which is every node while the inbound
// fee mechanism is off and most nodes while it is on.
//
// The two components are computed separately and only then added, which is
// what htlcswitch/link.go's CheckHtlcForward does and for the reason it
// documents: rounding an aggregate rate produces a number slightly above the
// sum of the separately rounded parts, and a sender that computed it the other
// way would have its forwards refused.
//
// The total is floored at zero. lnd's link expresses the same floor as a pair
// of conditions rather than a clamp, refusing an htlc whose incoming amount is
// below its outgoing one and separately refusing one that underpays the signed
// expected fee; the two forms accept exactly the same htlcs. A node that would
// end up paying to forward simply forwards for free instead.
//
// The signed inbound component is returned alongside the total, because the
// counters want to know whether an inbound fee was priced at all and the total
// cannot say: a discount that the floor swallowed and an absent fee produce
// the same number.
func nodeFee(policy, inPolicy *SimPolicy,
	amtOut lnwire.MilliSatoshi) (lnwire.MilliSatoshi, int64) {

	outFee := policy.fee(amtOut)

	var inFee int64
	if inPolicy != nil {
		inFee = inPolicy.inboundFee(amtOut + outFee)
	}

	total := int64(outFee) + inFee
	if total < 0 {
		total = 0
	}

	return lnwire.MilliSatoshi(total), inFee
}

// checkPolicy applies the forwarding policy checks of a node to an htlc that
// arrives with (amtIn, expiryIn) and is to be forwarded with (amtOut,
// expiryOut). policy is what the node announced for the channel it forwards
// out over; inPolicy is what it announced for the channel the htlc arrived
// over, and supplies the node's inbound fee. It returns a failure message if
// any check fails.
func checkPolicy(policy, inPolicy *SimPolicy, amtIn,
	amtOut lnwire.MilliSatoshi, expiryIn,
	expiryOut uint32) lnwire.FailureMessage {

	var emptyUpdate lnwire.ChannelUpdate1

	if policy.Disabled {
		return lnwire.NewChannelDisabled(0, emptyUpdate)
	}

	if failure := limitFailure(
		checkHtlcLimits(policy, amtOut), amtOut,
	); failure != nil {

		return failure
	}

	fee, _ := nodeFee(policy, inPolicy, amtOut)
	if amtIn < amtOut+fee {
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
