package lncfg

// Routing holds the configuration options for routing.
type Routing struct {
	AssumeChannelValid bool `long:"assumechanvalid" description:"Skip checking channel spentness during graph validation. This speedup comes at the risk of using an unvalidated view of the network for routing. (default: false)"`
}
