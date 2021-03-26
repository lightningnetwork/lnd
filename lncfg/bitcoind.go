package lncfg

// Bitcoind holds the configuration options for the daemon's connection to
// bitcoind.
type Bitcoind struct {
	Dir                string `long:"dir" description:"The base directory that contains the node's data, logs, configuration file, etc."`
	RPCHost            string `long:"rpchost" description:"The daemon's rpc listening address. If a port is omitted, then the default port for the selected chain parameters will be used."`
	RPCUser            string `long:"rpcuser" description:"Username for RPC connections"`
	RPCPass            string `long:"rpcpass" default-mask:"-" description:"Password for RPC connections"`
	ZMQPubRawBlock     string `long:"zmqpubrawblock" description:"The address listening for ZMQ connections to deliver raw block notifications"`
	ZMQPubRawTx        string `long:"zmqpubrawtx" description:"The address listening for ZMQ connections to deliver raw transaction notifications"`
	EstimateMode       string `long:"estimatemode" description:"The fee estimate mode. Must be either ECONOMICAL or CONSERVATIVE."`
	PrunedNodeMaxPeers int    `long:"pruned-node-max-peers" description:"The maximum number of peers lnd will choose from the backend node to retrieve pruned blocks from. This only applies to pruned nodes."`
}
