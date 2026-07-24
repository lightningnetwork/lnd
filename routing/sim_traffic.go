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

	// Seed makes the traffic sequence reproducible. The pair and amount
	// choices depend only on the seed, so two runs against the same
	// scenario file face the same exogenous process.
	Seed int64 `json:"seed"`
}

// simTraffic executes background payments directly against the hidden
// balances of a SimGraph. Background senders behave like naive fee-optimizing
// nodes: they route along the cheapest path the public gossip view allows and
// have no knowledge of hidden liquidity, so some of their payments fail, and
// only the settled ones move balances.
type simTraffic struct {
	graph  *SimGraph
	params SimTrafficParams
	rng    *rand.Rand

	// nodes is the sorted node list, fixed at construction so that pair
	// selection is deterministic for a given seed.
	nodes []route.Vertex

	// Sent and Settled count background payments for reporting.
	Sent    int
	Settled int
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

	return &simTraffic{
		graph:  graph,
		params: *params,
		rng:    rand.New(rand.NewSource(params.Seed)),
		nodes:  nodes,
	}, nil
}

// run executes one gap's worth of background payments.
func (t *simTraffic) run() {
	for i := 0; i < t.params.PaymentsPerGap; i++ {
		t.sendOne()
	}
}

// sendOne attempts a single background payment between a random pair.
func (t *simTraffic) sendOne() {
	sender := t.nodes[t.rng.Intn(len(t.nodes))]
	receiver := t.nodes[t.rng.Intn(len(t.nodes))]
	if sender == receiver {
		return
	}

	// Draw the amount log-uniformly from the configured range.
	logMin := math.Log(float64(t.params.MinAmtMsat))
	logMax := math.Log(float64(t.params.MaxAmtMsat))
	amt := lnwire.MilliSatoshi(math.Exp(
		logMin + t.rng.Float64()*(logMax-logMin),
	))

	t.Sent++

	// A naive sender: cheapest path first, blacklist the failing edge
	// and retry a couple of times, then give up.
	blacklist := make(map[trafficEdgeKey]struct{})
	for attempt := 0; attempt < t.params.RouteAttempts; attempt++ {
		rt := t.findRoute(sender, receiver, amt, blacklist)
		if rt == nil {
			return
		}

		result, err := t.graph.SendHtlc(rt)
		if err != nil {
			return
		}

		if result.Failure == nil {
			t.Settled++
			return
		}

		// Blacklist the directed edge that failed so the retry
		// explores a different corridor.
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

			policy := &channel.end(u).policy
			if !trafficEdgeUsable(
				policy, channel, state.amtIn,
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

// trafficEdgeUsable applies the public policy and capacity filters for
// forwarding the given amount.
func trafficEdgeUsable(policy *SimPolicy, channel *SimChannel,
	amt lnwire.MilliSatoshi) bool {

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

	return amt <= capMsat
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
