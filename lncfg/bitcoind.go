package lncfg

import (
	"errors"
	"time"

	"github.com/btcsuite/btcd/btcutil"
)

var (
	defaultBitcoindDir = btcutil.AppDataDir("bitcoin", false)
)

const (
	// defaultTxPollInterval is the default interval at which we poll for
	// new transactions.
	defaultTxPollInterval = 10 * time.Second

	// defaultTxPollingJitter is the default jitter we apply to the
	// polling. With the above defaultTxPollInterval, this means we would
	// poll transitions randomly between 5 and 15 seconds.
	defaultTxPollingJitter = 0.5

	defaultRPCHost              = "localhost"
	defaultBitcoindEstimateMode = "CONSERVATIVE"
	defaultPrunedNodeMaxPeers   = 4

	// defaultZMQReadDeadline is the default read deadline to be used for
	// both the block and tx ZMQ subscriptions.
	defaultZMQReadDeadline = 5 * time.Second
)

// Bitcoind holds the configuration options for the daemon's connection to
// bitcoind.
//
//nolint:lll
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
	TxPollingJitter      float64       `long:"txpollingjitter" description:"The factor used to simulates jitter by scaling 'txpollinginterval' with it. This value must be greater than 0 to see any effect. Use -1 to disable it."`
}

// DefaultBitcoind returns a default configuration for the bitcoind backend.
func DefaultBitcoind() *Bitcoind {
	return &Bitcoind{
		Dir:                defaultBitcoindDir,
		RPCHost:            defaultRPCHost,
		EstimateMode:       defaultBitcoindEstimateMode,
		PrunedNodeMaxPeers: defaultPrunedNodeMaxPeers,
		ZMQReadDeadline:    defaultZMQReadDeadline,
		TxPollingInterval:  defaultTxPollInterval,
		TxPollingJitter:    defaultTxPollingJitter,
	}
}

// ValidatePollingConfig validates the polling configuration.
func (b *Bitcoind) ValidatePollingConfig() error {
	if b.TxPollingJitter < 0 && b.TxPollingJitter != -1 {
		return errors.New("txpollingjitter must be greater than 0 or " +
			"equal to -1(disabled)")
	}

	if b.TxPollingInterval > 2*time.Minute {
		return errors.New("txpollinginterval must be less than 2 " +
			"minutes")
	}

	if b.BlockPollingInterval > 2*time.Minute {
		return errors.New("blockpollinginterval must be less than 2 " +
			"minutes")
	}

	return nil
}
