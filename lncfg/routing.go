package lncfg

import "fmt"

// Routing holds the configuration options for routing.
//
//nolint:ll
type Routing struct {
	AssumeChannelValid bool `long:"assumechanvalid" description:"DEPRECATED: Skip checking channel spentness during graph validation. This speedup comes at the risk of using an unvalidated view of the network for routing. (default: false)" hidden:"true"`

	StrictZombiePruning bool `long:"strictgraphpruning" description:"If true, then the graph will be pruned more aggressively for zombies. In practice this means that edges with a single stale edge will be considered a zombie."`

	BlindedPaths BlindedPaths `group:"blinding" namespace:"blinding"`
}

// BlindedPaths holds the configuration options for blinded path construction.
//
//nolint:ll
type BlindedPaths struct {
	MinNumRealHops           uint8   `long:"min-num-real-hops" description:"The minimum number of real hops to include in a blinded path. This doesn't include our node, so if the minimum is 1, then the path will contain at minimum our node along with an introduction node hop. If it is zero then the shortest path will use our node as an introduction node."`
	NumHops                  uint8   `long:"num-hops" description:"The number of hops to include in a blinded path. This doesn't include our node, so if it is 1, then the path will contain our node along with an introduction node or dummy node hop. If paths shorter than NumHops is found, then they will be padded using dummy hops."`
	MaxNumPaths              uint8   `long:"max-num-paths" description:"The maximum number of blinded paths to select and add to an invoice."`
	PolicyIncreaseMultiplier float64 `long:"policy-increase-multiplier" description:"The amount by which to increase certain policy values of hops on a blinded path in order to add a probing buffer."`
	PolicyDecreaseMultiplier float64 `long:"policy-decrease-multiplier" description:"The amount by which to decrease certain policy values of hops on a blinded path in order to add a probing buffer."`
}

// Validate checks that the various routing config options are sane.
//
// NOTE: this is part of the Validator interface.
func (r *Routing) Validate() error {
	if r.BlindedPaths.MinNumRealHops > r.BlindedPaths.NumHops {
		return fmt.Errorf("the minimum number of real hops in a " +
			"blinded path must be smaller than or equal to the " +
			"number of hops expected to be included in each path")
	}

	if r.BlindedPaths.MaxNumPaths == 0 {
		return fmt.Errorf("blinded max num paths cannot be 0")
	}

	if r.BlindedPaths.PolicyIncreaseMultiplier < 1 {
		return fmt.Errorf("the blinded route policy increase " +
			"multiplier must be greater than or equal to 1")
	}

	if r.BlindedPaths.PolicyDecreaseMultiplier > 1 ||
		r.BlindedPaths.PolicyDecreaseMultiplier < 0 {

		return fmt.Errorf("the blinded route policy decrease " +
			"multiplier must be in the range (0,1]")
	}

	return nil
}
