//go:build darwin && !kvdb_etcd && !kvdb_postgres
// +build darwin,!kvdb_etcd,!kvdb_postgres

package wait

import "time"

const (
	// MinerMempoolTimeout is the max time we will wait for a transaction
	// to propagate to the mining node's mempool.
	MinerMempoolTimeout = time.Minute

	// ChannelOpenTimeout is the max time we will wait before a channel to
	// be considered opened.
	ChannelOpenTimeout = time.Second * 30

	// ChannelCloseTimeout is the max time we will wait before a channel is
	// considered closed.
	ChannelCloseTimeout = time.Second * 30

	// DefaultTimeout is a timeout that will be used for various wait
	// scenarios where no custom timeout value is defined.
	DefaultTimeout = time.Second * 30

	// AsyncBenchmarkTimeout is the timeout used when running the async
	// payments benchmark. This timeout takes considerably longer on darwin
	// after go1.12 corrected its use of fsync.
	AsyncBenchmarkTimeout = time.Minute * 5

	// NodeStartTimeout is the timeout value when waiting for a node to
	// become fully started.
	//
	// TODO(yy): There is an optimization we can do to increase the time it
	// takes to finish the initial wallet sync. Instead of finding the
	// block birthday using binary search in btcwallet, we can instead
	// search optimistically by looking at the chain tip minus X blocks to
	// get the birthday block. This way in the test the node won't attempt
	// to sync from the beginning of the chain, which is always the case
	// due to how regtest blocks are mined.
	// The other direction of optimization is to change the precision of
	// the regtest block's median time. By consensus, we need to increase
	// at least one second(?), this means in regtest when large amount of
	// blocks are mined in a short time, the block time is actually in the
	// future. We could instead allow the median time to increase by
	// microseconds for itests.
	NodeStartTimeout = time.Minute * 3

	// SqliteBusyTimeout is the maximum time that a call to the sqlite db
	// will wait for the connection to become available.
	SqliteBusyTimeout = time.Second * 10

	// PaymentTimeout is the timeout used when sending payments.
	PaymentTimeout = time.Second * 60
)
