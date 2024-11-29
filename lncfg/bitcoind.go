package lncfg

import "time"

const (
	// DefaultTxPollingJitter defines the default TxPollingIntervalJitter
	// to be used for bitcoind backend.
	DefaultTxPollingJitter = 0.5
)

// Bitcoind holds the configuration options for the daemon's connection to
// bitcoind.
//
//nolint:ll
type Bitcoind struct {
	Dir                  string        `long:"dir" description:"The base directory that contains the node's data, logs, configuration file, etc."`
	ConfigPath           string        `long:"config" description:"Configuration filepath. If not set, will default to the default filename under 'dir'."`
	RPCCookie            string        `long:"rpccookie" description:"Authentication cookie file for RPC connections. If not set, will default to .cookie under 'dir'."`
	RPCHost              string        `long:"rpchost" description:"The daemon's rpc listening address. If a port is omitted, then the default port for the selected chain parameters will be used."`
	RPCUser              string        `long:"rpcuser" description:"Username for RPC connections"`
	RPCPass              string        `long:"rpcpass" default-mask:"-" description:"Password for RPC connections"`
	ZMQPubRawBlock       string        `long:"zmqpubrawblock" description:"The address listening for ZMQ connections to deliver raw block notifications"`
	ZMQPubRawTx          string        `long:"zmqpubrawtx" description:"The address listening for ZMQ connections to deliver raw transaction notifications"`
	ZMQReadDeadline      time.Duration `long:"zmqreaddeadline" description:"The read deadline for reading ZMQ messages from both the block and tx subscriptions"`
	EstimateMode         string        `long:"estimatemode" description:"The fee estimate mode. Must be either ECONOMICAL or CONSERVATIVE."`
	PrunedNodeMaxPeers   int           `long:"pruned-node-max-peers" description:"The maximum number of peers lnd will choose from the backend node to retrieve pruned blocks from. This only applies to pruned nodes."`
	RPCPolling           bool          `long:"rpcpolling" description:"Poll the bitcoind RPC interface for block and transaction notifications instead of using the ZMQ interface"`
	BlockPollingInterval time.Duration `long:"blockpollinginterval" description:"The interval that will be used to poll bitcoind for new blocks. Only used if rpcpolling is true."`
	TxPollingInterval    time.Duration `long:"txpollinginterval" description:"The interval that will be used to poll bitcoind for new tx. Only used if rpcpolling is true."`
}
