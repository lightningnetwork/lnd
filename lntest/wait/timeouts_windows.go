//go:build windows && !kvdb_etcd && !kvdb_postgres
// +build windows,!kvdb_etcd,!kvdb_postgres

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
	DefaultTimeout = time.Second * 60

	// AsyncBenchmarkTimeout is the timeout used when running the async
	// payments benchmark.
	AsyncBenchmarkTimeout = time.Minute * 5

	// NodeStartTimeout is the timeout value when waiting for a node to
	// become fully started.
	NodeStartTimeout = time.Minute * 3

	// SqliteBusyTimeout is the maximum time that a call to the sqlite db
	// will wait for the connection to become available.
	SqliteBusyTimeout = time.Second * 10

	// PaymentTimeout is the timeout used when sending payments.
	PaymentTimeout = time.Second * 120
)
