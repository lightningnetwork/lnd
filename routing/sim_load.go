package routing

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strconv"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

// The JSON shapes below mirror the output of `lncli describegraph`, which
// encodes 64-bit numbers as strings.

type describeGraphJSON struct {
	Nodes []describeGraphNode `json:"nodes"`
	Edges []describeGraphEdge `json:"edges"`
}

type describeGraphNode struct {
	PubKey string `json:"pub_key"`
	Alias  string `json:"alias"`
}

type describeGraphEdge struct {
	ChannelID   string               `json:"channel_id"`
	Capacity    string               `json:"capacity"`
	Node1Pub    string               `json:"node1_pub"`
	Node2Pub    string               `json:"node2_pub"`
	Node1Policy *describeGraphPolicy `json:"node1_policy"`
	Node2Policy *describeGraphPolicy `json:"node2_policy"`

	// Balance and BalanceCertainty are not part of `lncli describegraph`,
	// which cannot see anyone else's balances. They are what an externally
	// modelled graph adds: Balance is node1's side of the channel in
	// satoshis, string encoded like every other 64 bit field, and
	// BalanceCertainty is the modeller's confidence in it from zero to
	// one.
	//
	// Reading them is not a behavior change. A snapshot that carries
	// neither, which is every snapshot the program has scored so far,
	// loads exactly as it always did, and even a graph that carries both
	// only has them read once a scenario asks for the from_graph
	// liquidity model.
	Balance          string  `json:"balance"`
	BalanceCertainty float64 `json:"balance_certainty"`
}

type describeGraphPolicy struct {
	TimeLockDelta    uint32 `json:"time_lock_delta"`
	MinHTLC          string `json:"min_htlc"`
	FeeBaseMsat      string `json:"fee_base_msat"`
	FeeRateMilliMsat string `json:"fee_rate_milli_msat"`
	MaxHTLCMsat      string `json:"max_htlc_msat"`
	Disabled         bool   `json:"disabled"`

	// The inbound fee pair is emitted as JSON numbers rather than strings,
	// because both are int32 on the wire and describegraph only
	// string-encodes the 64 bit fields. They are signed, and on the real
	// graph they are overwhelmingly negative.
	InboundFeeBaseMsat      int32 `json:"inbound_fee_base_msat"`
	InboundFeeRateMilliMsat int32 `json:"inbound_fee_rate_milli_msat"`
}

// parseInt64 parses describegraph's string-encoded integers, treating the
// empty string as zero.
func parseInt64(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	return strconv.ParseInt(s, 10, 64)
}

// toSimPolicy converts a describegraph policy to a SimPolicy. A nil policy
// (unannounced direction) is returned as a disabled policy.
func (p *describeGraphPolicy) toSimPolicy() (SimPolicy, error) {
	if p == nil {
		return SimPolicy{Disabled: true}, nil
	}

	baseFee, err := parseInt64(p.FeeBaseMsat)
	if err != nil {
		return SimPolicy{}, err
	}
	feeRate, err := parseInt64(p.FeeRateMilliMsat)
	if err != nil {
		return SimPolicy{}, err
	}
	minHTLC, err := parseInt64(p.MinHTLC)
	if err != nil {
		return SimPolicy{}, err
	}
	maxHTLC, err := parseInt64(p.MaxHTLCMsat)
	if err != nil {
		return SimPolicy{}, err
	}

	return SimPolicy{
		BaseFeeMsat:   lnwire.MilliSatoshi(baseFee),
		FeeRatePPM:    lnwire.MilliSatoshi(feeRate),
		TimeLockDelta: uint16(p.TimeLockDelta),
		MinHTLCMsat:   lnwire.MilliSatoshi(minHTLC),
		MaxHTLCMsat:   lnwire.MilliSatoshi(maxHTLC),

		// The snapshot's inbound fees were dropped on the floor until
		// stage B: 4,783 directed policies announce one and the loader
		// simply did not read the fields. Preserving them here is not
		// yet a behavior change, because nothing charges or shows an
		// inbound fee until a scenario file asks for it.
		InboundBaseMsat: p.InboundFeeBaseMsat,
		InboundRatePPM:  p.InboundFeeRateMilliMsat,

		Disabled: p.Disabled,
	}, nil
}

// LoadSimGraphFromFile builds a SimGraph from an `lncli describegraph` JSON
// snapshot on disk. Channels missing both policies are skipped, since path
// finding could never use them. Balances are initialized to a 50/50 split;
// use AssignLiquidity to apply a liquidity model. A file that carries
// modelled balances has them recorded but not applied, since which liquidity
// a scenario is scored on is the scenario's call and not the graph's.
func LoadSimGraphFromFile(path string) (*SimGraph, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var graphJSON describeGraphJSON
	if err := json.Unmarshal(data, &graphJSON); err != nil {
		return nil, fmt.Errorf("unable to parse describegraph "+
			"JSON: %w", err)
	}

	g := NewSimGraph()

	for _, node := range graphJSON.Nodes {
		pubKey, err := route.NewVertexFromStr(node.PubKey)
		if err != nil {
			return nil, fmt.Errorf("invalid node pubkey %v: %w",
				node.PubKey, err)
		}

		if _, err := g.AddNode(pubKey, node.Alias); err != nil {
			return nil, err
		}
	}

	for _, edge := range graphJSON.Edges {
		// Channels with no announced policy in either direction are
		// useless for routing.
		if edge.Node1Policy == nil && edge.Node2Policy == nil {
			continue
		}

		chanID, err := parseInt64(edge.ChannelID)
		if err != nil {
			return nil, fmt.Errorf("invalid channel id %v: %w",
				edge.ChannelID, err)
		}
		capacity, err := parseInt64(edge.Capacity)
		if err != nil {
			return nil, fmt.Errorf("invalid capacity %v: %w",
				edge.Capacity, err)
		}

		node1, err := route.NewVertexFromStr(edge.Node1Pub)
		if err != nil {
			return nil, err
		}
		node2, err := route.NewVertexFromStr(edge.Node2Pub)
		if err != nil {
			return nil, err
		}

		policy1, err := edge.Node1Policy.toSimPolicy()
		if err != nil {
			return nil, err
		}
		policy2, err := edge.Node2Policy.toSimPolicy()
		if err != nil {
			return nil, err
		}

		err = g.AddChannel(
			uint64(chanID), node1, node2,
			btcutil.Amount(capacity), policy1, policy2,
		)
		if err != nil {
			return nil, err
		}

		// Carry over the modelled balance, if this file has one. A
		// balance outside the channel is a corrupt file rather than an
		// unusual world, so it fails here instead of being clamped
		// into something plausible.
		if edge.Balance == "" {
			continue
		}

		balance, err := parseInt64(edge.Balance)
		if err != nil {
			return nil, fmt.Errorf("invalid balance %v on "+
				"channel %v: %w", edge.Balance,
				edge.ChannelID, err)
		}
		if balance < 0 || balance > capacity {
			return nil, fmt.Errorf("channel %v: balance %v sats "+
				"is outside its %v sat capacity",
				edge.ChannelID, balance, capacity)
		}

		err = g.setGraphBalance(
			uint64(chanID), node1,
			lnwire.NewMSatFromSatoshis(btcutil.Amount(balance)),
			edge.BalanceCertainty,
		)
		if err != nil {
			return nil, err
		}
	}

	return g, nil
}

// NodeByAlias returns the pubkey of the node with the given alias. Aliases
// collide freely on real graphs, so ties resolve to the lexicographically
// smallest pubkey to keep scenario resolution deterministic.
func (g *SimGraph) NodeByAlias(alias string) (route.Vertex, error) {
	var (
		best  route.Vertex
		found bool
	)
	for pubKey, node := range g.nodes {
		if node.Alias != alias {
			continue
		}
		if !found || bytes.Compare(pubKey[:], best[:]) < 0 {
			best = pubKey
			found = true
		}
	}

	if !found {
		return route.Vertex{}, fmt.Errorf("no node with alias %q",
			alias)
	}

	return best, nil
}
