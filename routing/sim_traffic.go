package routing

import (
	"container/heap"
	"fmt"
	"math"
	"math/rand"
	"sort"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

// trafficFinalCltvDelta is the final cltv delta background senders use,
// matching the value the candidate contract documents.
const trafficFinalCltvDelta = 40

// trafficMaxHops caps the route length of background payments.
const trafficMaxHops = 8

// SimTrafficParams configures the exogenous background traffic model: seeded
// payments between random node pairs that move hidden liquidity between the
// scenario payments, the way other people's payments do on the real network.
type SimTrafficParams struct {
	// PaymentsPerGap is how many background payments are executed before
	// each scenario payment.
	PaymentsPerGap int `json:"payments_per_gap"`

	// MinAmtMsat and MaxAmtMsat bound the payment amounts, drawn
	// log-uniformly so small payments dominate, as they do in practice.
	MinAmtMsat uint64 `json:"min_amt_msat"`
	MaxAmtMsat uint64 `json:"max_amt_msat"`

	// RouteAttempts is how many alternative routes a background sender
	// tries before giving up on a payment. Defaults to 3.
	RouteAttempts int `json:"route_attempts"`

	// FocusFraction is the share of background payments that use a node
	// from the focus set (the scenario's own source and targets) as one
	// endpoint, so that a configurable part of the exogenous process
	// crosses the corridors under test rather than churning liquidity in
	// parts of the graph no scored payment ever visits. Zero, the
	// default, draws both endpoints uniformly.
	FocusFraction float64 `json:"focus_fraction"`

	// Seed makes the traffic sequence reproducible. The pair and amount
	// choices depend only on the seed, so two runs against the same
	// scenario file face the same exogenous process.
	Seed int64 `json:"seed"`
}

// trafficMinShrinkFactor bounds how far a background sender will scale an
// amount down looking for a corridor that can carry it. Each retry halves,
// so this is reached after a few attempts.
const trafficMinShrinkFactor = 0.05

// simTraffic executes background payments directly against the hidden
// balances of a SimGraph.
//
// This is an ENVIRONMENT process, not a player, and it is written to move
// liquidity at the configured rate rather than to model a realistic sender's
// ignorance. That distinction was learned the hard way: the first version
// routed on public knowledge alone and gave up after a few failures, so only
// ~18% of its payments settled — and since a failed payment moves nothing,
// the exogenous process ran about 5x weaker than every scenario file
// requested. Experiments that turned the traffic knob (exp-008's drift
// question, exp-010b's atomic arena) were therefore measuring a much calmer
// network than they claimed to. Background senders here consult hidden
// balances when choosing a corridor and scale the amount down until one
// fits, which is the environment's privilege; what the CANDIDATE may see is
// unchanged and still sealed.
type simTraffic struct {
	graph  *SimGraph
	params SimTrafficParams
	rng    *rand.Rand

	// nodes is the sorted node list, fixed at construction so that pair
	// selection is deterministic for a given seed.
	nodes []route.Vertex

	// cumDegree[i] is the running sum of channel counts over nodes[:i+1],
	// used to draw endpoints in proportion to how well connected they
	// are. Uniform draws are wrong on a real topology: the mainnet
	// snapshot has a MEDIAN degree of 1 and 68% of its nodes hold two
	// channels or fewer, so uniform sampling picks leaf-to-leaf pairs
	// that frequently have no path between them at any amount. Weighting
	// by degree puts the churn on the corridors that carry real traffic.
	cumDegree []int
	totDegree int

	// focus holds the nodes the scenario payments themselves use. A
	// FocusFraction share of background payments takes one endpoint from
	// here so the churn lands where it can actually interfere.
	focus []route.Vertex

	// Sent and Settled count background payments for reporting.
	Sent    int
	Settled int
}

// SetFocus points a share of the background traffic at the given nodes, which
// the runner fills with the scenario source and targets. Order is preserved
// so the draw stays deterministic for a seed.
func (t *simTraffic) SetFocus(nodes []route.Vertex) {
	t.focus = t.focus[:0]
	for _, v := range nodes {
		if _, ok := t.graph.nodes[v]; ok {
			t.focus = append(t.focus, v)
		}
	}
}

// newSimTraffic builds a traffic engine over the given graph.
func newSimTraffic(graph *SimGraph, params *SimTrafficParams) (*simTraffic,
	error) {

	if params.PaymentsPerGap <= 0 {
		return nil, fmt.Errorf("payments_per_gap must be positive")
	}
	if params.MinAmtMsat == 0 || params.MaxAmtMsat < params.MinAmtMsat {
		return nil, fmt.Errorf("invalid traffic amount range [%d, %d]",
			params.MinAmtMsat, params.MaxAmtMsat)
	}
	if params.RouteAttempts <= 0 {
		params.RouteAttempts = 3
	}

	nodes := make([]route.Vertex, 0, len(graph.nodes))
	for v := range graph.nodes {
		nodes = append(nodes, v)
	}
	sort.Slice(nodes, func(i, j int) bool {
		return nodes[i].String() < nodes[j].String()
	})

	if len(nodes) < 2 {
		return nil, fmt.Errorf("traffic needs at least two nodes")
	}

	cumDegree := make([]int, len(nodes))
	total := 0
	for i, v := range nodes {
		total += len(graph.nodes[v].channels)
		cumDegree[i] = total
	}

	return &simTraffic{
		graph:     graph,
		params:    *params,
		rng:       rand.New(rand.NewSource(params.Seed)),
		nodes:     nodes,
		cumDegree: cumDegree,
		totDegree: total,
	}, nil
}

// pickNode draws one node with probability proportional to its channel count.
func (t *simTraffic) pickNode() route.Vertex {
	if t.totDegree == 0 {
		return t.nodes[t.rng.Intn(len(t.nodes))]
	}

	draw := t.rng.Intn(t.totDegree)
	idx := sort.SearchInts(t.cumDegree, draw+1)
	if idx >= len(t.nodes) {
		idx = len(t.nodes) - 1
	}

	return t.nodes[idx]
}

// run executes one gap's worth of background payments.
func (t *simTraffic) run() {
	t.runN(t.params.PaymentsPerGap)
}

// runN executes the given number of background payments, the slice of the
// exogenous process that belongs to some stretch of virtual time shorter than
// a full gap.
func (t *simTraffic) runN(n int) {
	for i := 0; i < n; i++ {
		t.sendOne()
	}
}

// pickPair chooses the endpoints of one background payment. With probability
// FocusFraction one of them is drawn from the focus set, so that share of the
// churn crosses the corridors the scored payments use.
func (t *simTraffic) pickPair() (route.Vertex, route.Vertex) {
	sender := t.pickNode()
	receiver := t.pickNode()

	if len(t.focus) == 0 || t.rng.Float64() >= t.params.FocusFraction {
		return sender, receiver
	}

	// Replace one endpoint, chosen by the same rng so the direction of
	// the focused flow varies rather than always originating there.
	pick := t.focus[t.rng.Intn(len(t.focus))]
	if t.rng.Intn(2) == 0 {
		return pick, receiver
	}

	return sender, pick
}

// sendOne attempts a single background payment between a random pair.
//
// The sender scales its amount down until it finds a corridor that can carry
// it, which is what makes this process actually move liquidity: an amount
// drawn blind from the configured range mostly exceeds what a bimodal channel
// holds, and a payment that fails moves nothing at all.
func (t *simTraffic) sendOne() {
	sender, receiver := t.pickPair()
	if sender == receiver {
		return
	}

	// Draw the amount log-uniformly from the configured range.
	logMin := math.Log(float64(t.params.MinAmtMsat))
	logMax := math.Log(float64(t.params.MaxAmtMsat))
	desired := lnwire.MilliSatoshi(math.Exp(
		logMin + t.rng.Float64()*(logMax-logMin),
	))

	t.Sent++

	floor := lnwire.MilliSatoshi(
		float64(desired) * trafficMinShrinkFactor,
	)
	if floor < lnwire.MilliSatoshi(t.params.MinAmtMsat) {
		floor = lnwire.MilliSatoshi(t.params.MinAmtMsat)
	}

	amt := desired
	blacklist := make(map[trafficEdgeKey]struct{})
	for attempt := 0; attempt < t.params.RouteAttempts; attempt++ {
		rt := t.findRoute(sender, receiver, amt, blacklist)
		if rt == nil {
			// No corridor carries this much. Halve and look
			// again rather than abandoning the payment, the way
			// a real sender would fall back to a smaller
			// transfer or a split.
			if amt <= floor {
				return
			}

			amt /= 2
			if amt < floor {
				amt = floor
			}

			continue
		}

		result, err := t.graph.SendHtlc(rt)
		if err != nil {
			return
		}

		if result.Failure == nil {
			t.Settled++
			return
		}

		// The route was liquidity-checked when it was found, so a
		// failure here means something moved underneath it: an
		// in-flight hold from the scenario payment, or an earlier
		// background payment in this same gap. Blacklist the edge
		// that failed and let the next attempt route around it.
		idx := getNodeIndexSim(rt, result.FailureSource)
		if idx == nil || *idx >= len(rt.Hops) {
			return
		}
		blacklist[trafficEdgeKey{
			from:   result.FailureSource,
			chanID: rt.Hops[*idx].ChannelID,
		}] = struct{}{}
	}
}

// trafficEdgeKey identifies a directed channel edge for blacklisting.
type trafficEdgeKey struct {
	from   route.Vertex
	chanID uint64
}

// trafficPathNode is the per-node state of the backward Dijkstra search.
type trafficPathNode struct {
	// amtIn is the amount that must arrive at this node for the payment
	// amount to reach the receiver, i.e. amount plus downstream fees.
	amtIn lnwire.MilliSatoshi

	// expiryIn is the cltv expiry that must arrive at this node.
	expiryIn uint32

	// hops is the number of channels between this node and the receiver.
	hops int

	// nextChan and nextNode point one step toward the receiver.
	nextChan uint64
	nextNode route.Vertex
}

// trafficHeapItem is a priority queue entry for the search.
type trafficHeapItem struct {
	node  route.Vertex
	amtIn lnwire.MilliSatoshi
}

type trafficHeap []trafficHeapItem

func (h trafficHeap) Len() int { return len(h) }

func (h trafficHeap) Less(i, j int) bool { return h[i].amtIn < h[j].amtIn }

func (h trafficHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }

func (h *trafficHeap) Push(value any) {
	*h = append(*h, value.(trafficHeapItem))
}

func (h *trafficHeap) Pop() any {
	old := *h
	item := old[len(old)-1]
	*h = old[:len(old)-1]

	return item
}

// findRoute runs a backward cheapest-fee Dijkstra from the receiver and
// builds a well-formed route, or returns nil if no usable path exists. The
// search only uses public knowledge: policies and capacities, never hidden
// balances.
func (t *simTraffic) findRoute(sender, receiver route.Vertex,
	amt lnwire.MilliSatoshi,
	blacklist map[trafficEdgeKey]struct{}) *route.Route {

	states := map[route.Vertex]*trafficPathNode{
		receiver: {
			amtIn:    amt,
			expiryIn: trafficFinalCltvDelta,
		},
	}
	settled := make(map[route.Vertex]struct{})

	pq := &trafficHeap{{node: receiver, amtIn: amt}}
	heap.Init(pq)

	for pq.Len() > 0 {
		item := heap.Pop(pq).(trafficHeapItem)
		if _, done := settled[item.node]; done {
			continue
		}
		settled[item.node] = struct{}{}

		if item.node == sender {
			break
		}

		state := states[item.node]
		if state.hops >= trafficMaxHops {
			continue
		}

		node := t.graph.nodes[item.node]
		for _, channel := range node.channels {
			// u would forward INTO item.node over this channel.
			u := channel.otherEnd(item.node).owner
			if _, done := settled[u]; done {
				continue
			}

			key := trafficEdgeKey{from: u, chanID: channel.ID}
			if _, banned := blacklist[key]; banned {
				continue
			}

			sendingEnd := channel.end(u)
			policy := &sendingEnd.policy
			if !trafficEdgeUsable(
				sendingEnd, channel, state.amtIn,
			) {
				continue
			}

			amtIn := state.amtIn + policy.fee(state.amtIn)
			existing, seen := states[u]
			if seen && existing.amtIn <= amtIn {
				continue
			}

			states[u] = &trafficPathNode{
				amtIn: amtIn,
				expiryIn: state.expiryIn +
					uint32(policy.TimeLockDelta),
				hops:     state.hops + 1,
				nextChan: channel.ID,
				nextNode: item.node,
			}
			heap.Push(pq, trafficHeapItem{node: u, amtIn: amtIn})
		}
	}

	if _, reached := settled[sender]; !reached {
		return nil
	}

	return t.buildRoute(sender, receiver, amt, states)
}

// trafficEdgeUsable applies the policy and capacity filters for forwarding
// the given amount, and then the hidden liquidity check.
//
// Consulting the hidden balance is the environment's privilege and the whole
// reason this process moves the liquidity it is configured to move: routing
// on capacity alone picks corridors that a bimodal balance distribution
// cannot fund, and the resulting failure moves nothing. Candidates still see
// only the sealed gossip view.
func trafficEdgeUsable(end *simChannelEnd, channel *SimChannel,
	amt lnwire.MilliSatoshi) bool {

	policy := &end.policy
	if policy.Disabled {
		return false
	}
	if amt < policy.MinHTLCMsat {
		return false
	}
	if policy.MaxHTLCMsat != 0 && amt > policy.MaxHTLCMsat {
		return false
	}

	capMsat := lnwire.NewMSatFromSatoshis(channel.Capacity)
	if amt > capMsat {
		return false
	}

	return end.available() >= amt
}

// buildRoute walks the next pointers from sender to receiver and assembles a
// route with the amount and expiry accumulation SendHtlc expects.
func (t *simTraffic) buildRoute(sender, receiver route.Vertex,
	amt lnwire.MilliSatoshi,
	states map[route.Vertex]*trafficPathNode) *route.Route {

	var hops []*route.Hop

	current := sender
	for current != receiver {
		state := states[current]

		// Hop semantics: AmtToForward is the amount the hop's node
		// forwards ONWARD, i.e. the amount that must arrive at the
		// node two steps downstream. For the final hop it is the
		// payment amount, which is exactly what the receiver's state
		// holds. Same shape for the expiry.
		amtToForward := amt
		outgoingTimeLock := uint32(trafficFinalCltvDelta)
		if state.nextNode != receiver {
			afterNext := states[states[state.nextNode].nextNode]
			amtToForward = afterNext.amtIn
			outgoingTimeLock = afterNext.expiryIn
		}

		hops = append(hops, &route.Hop{
			PubKeyBytes:      state.nextNode,
			ChannelID:        state.nextChan,
			AmtToForward:     amtToForward,
			OutgoingTimeLock: outgoingTimeLock,
		})

		current = state.nextNode
	}

	if len(hops) == 0 {
		return nil
	}

	// The route total is what the sender puts on its first channel: the
	// amount that must arrive at the first hop's node. For a direct
	// channel this is the payment amount itself, via the receiver state.
	first := states[states[sender].nextNode]

	return &route.Route{
		TotalAmount:   first.amtIn,
		TotalTimeLock: first.expiryIn,
		SourcePubKey:  sender,
		Hops:          hops,
	}
}
