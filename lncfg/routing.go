package lncfg

// Routing holds the configuration options for routing.
//
//nolint:lll
type Routing struct {
	AssumeChannelValid bool `long:"assumechanvalid" description:"Skip checking channel spentness during graph validation. This speedup comes at the risk of using an unvalidated view of the network for routing. (default: false)"`

	StrictZombiePruning bool `long:"strictgraphpruning" description:"If true, then the graph will be pruned more aggressively for zombies. In practice this means that edges with a single stale edge will be considered a zombie."`
}
