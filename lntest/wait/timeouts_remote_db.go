//go:build kvdb_etcd || kvdb_postgres
// +build kvdb_etcd kvdb_postgres

package wait

import "time"

const (
	// extraTimeout is the additional time we wait for the postgres backend
	// until the issue is resolved:
	// - https://github.com/lightningnetwork/lnd/issues/8809
	extraTimeout = time.Second * 30

	// MinerMempoolTimeout is the max time we will wait for a transaction
	// to propagate to the mining node's mempool.
	MinerMempoolTimeout = time.Minute + extraTimeout

	// ChannelOpenTimeout is the max time we will wait before a channel to
	// be considered opened.
	ChannelOpenTimeout = time.Second*30 + extraTimeout

	// ChannelCloseTimeout is the max time we will wait before a channel is
	// considered closed.
	ChannelCloseTimeout = time.Second*30 + extraTimeout

	// DefaultTimeout is a timeout that will be used for various wait
	// scenarios where no custom timeout value is defined.
	DefaultTimeout = time.Second*60 + extraTimeout

	// AsyncBenchmarkTimeout is the timeout used when running the async
	// payments benchmark.
	AsyncBenchmarkTimeout = time.Minute*5 + extraTimeout

	// NodeStartTimeout is the timeout value when waiting for a node to
	// become fully started.
	NodeStartTimeout = time.Minute*2 + extraTimeout

	// SqliteBusyTimeout is the maximum time that a call to the sqlite db
	// will wait for the connection to become available.
	SqliteBusyTimeout = time.Second*10 + extraTimeout

	// PaymentTimeout is the timeout used when sending payments.
	PaymentTimeout = time.Second*60 + extraTimeout
)
