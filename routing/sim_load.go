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
}

type describeGraphPolicy struct {
	TimeLockDelta    uint32 `json:"time_lock_delta"`
	MinHTLC          string `json:"min_htlc"`
	FeeBaseMsat      string `json:"fee_base_msat"`
	FeeRateMilliMsat string `json:"fee_rate_milli_msat"`
	MaxHTLCMsat      string `json:"max_htlc_msat"`
	Disabled         bool   `json:"disabled"`
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
		Disabled:      p.Disabled,
	}, nil
}

// LoadSimGraphFromFile builds a SimGraph from an `lncli describegraph` JSON
// snapshot on disk. Channels missing both policies are skipped, since path
// finding could never use them. Balances are initialized to a 50/50 split;
// use AssignLiquidity to apply a liquidity model.
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
