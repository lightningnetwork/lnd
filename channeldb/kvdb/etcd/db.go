// +build kvdb_etcd

package etcd

import (
	"context"
	"fmt"
	"io"
	"runtime"
	"sync"
	"time"

	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/coreos/etcd/clientv3"
	"github.com/coreos/etcd/pkg/transport"
)

const (
	// etcdConnectionTimeout is the timeout until successful connection to the
	// etcd instance.
	etcdConnectionTimeout = 10 * time.Second

	// etcdLongTimeout is a timeout for longer taking etcd operatons.
	etcdLongTimeout = 30 * time.Second
)

// callerStats holds commit stats for a specific caller. Currently it only
// holds the max stat, meaning that for a particular caller the largest
// commit set is recorded.
type callerStats struct {
	count       int
	commitStats CommitStats
}

func (s callerStats) String() string {
	return fmt.Sprintf("count: %d, retries: %d, rset: %d, wset: %d",
		s.count, s.commitStats.Retries, s.commitStats.Rset, s.commitStats.Wset)
}

// commitStatsCollector collects commit stats for commits succeeding
// and also for commits failing.
type commitStatsCollector struct {
	sync.RWMutex
	succ map[string]*callerStats
	fail map[string]*callerStats
}

// newCommitStatsColletor creates a new commitStatsCollector instance.
func newCommitStatsColletor() *commitStatsCollector {
	return &commitStatsCollector{
		succ: make(map[string]*callerStats),
		fail: make(map[string]*callerStats),
	}
}

// PrintStats returns collected stats pretty printed into a string.
func (c *commitStatsCollector) PrintStats() string {
	c.RLock()
	defer c.RUnlock()

	s := "\nFailure:\n"
	for k, v := range c.fail {
		s += fmt.Sprintf("%s\t%s\n", k, v)
	}

	s += "\nSuccess:\n"
	for k, v := range c.succ {
		s += fmt.Sprintf("%s\t%s\n", k, v)
	}

	return s
}

// updateStatsMap updatess commit stats map for a caller.
func updateStatMap(
	caller string, stats CommitStats, m map[string]*callerStats) {

	if _, ok := m[caller]; !ok {
		m[caller] = &callerStats{}
	}

	curr := m[caller]
	curr.count++

	// Update only if the total commit set is greater or equal.
	currTotal := curr.commitStats.Rset + curr.commitStats.Wset
	if currTotal <= (stats.Rset + stats.Wset) {
		curr.commitStats = stats
	}
}

// callback is an STM commit stats callback passed which can be passed
// using a WithCommitStatsCallback to the STM upon construction.
func (c *commitStatsCollector) callback(succ bool, stats CommitStats) {
	caller := "unknown"

	// Get the caller. As this callback is called from
	// the backend interface that means we need to ascend
	// 4 frames in the callstack.
	_, file, no, ok := runtime.Caller(4)
	if ok {
		caller = fmt.Sprintf("%s#%d", file, no)
	}

	c.Lock()
	defer c.Unlock()

	if succ {
		updateStatMap(caller, stats, c.succ)
	} else {
		updateStatMap(caller, stats, c.fail)
	}
}

// db holds a reference to the etcd client connection.
type db struct {
	config               BackendConfig
	cli                  *clientv3.Client
	commitStatsCollector *commitStatsCollector
}

// Enforce db implements the walletdb.DB interface.
var _ walletdb.DB = (*db)(nil)

// BackendConfig holds and etcd backend config and connection parameters.
type BackendConfig struct {
	// Ctx is the context we use to cancel operations upon exit.
	Ctx context.Context

	// Host holds the peer url of the etcd instance.
	Host string

	// User is the username for the etcd peer.
	User string

	// Pass is the password for the etcd peer.
	Pass string

	// CertFile holds the path to the TLS certificate for etcd RPC.
	CertFile string

	// KeyFile holds the path to the TLS private key for etcd RPC.
	KeyFile string

	// InsecureSkipVerify should be set to true if we intend to
	// skip TLS verification.
	InsecureSkipVerify bool

	// Prefix the hash of the prefix will be used as the root
	// bucket id. This enables key space separation similar to
	// name spaces.
	Prefix string

	// CollectCommitStats indicates wheter to commit commit stats.
	CollectCommitStats bool
}

// newEtcdBackend returns a db object initialized with the passed backend
// config. If etcd connection cannot be estabished, then returns error.
func newEtcdBackend(config BackendConfig) (*db, error) {
	if config.Ctx == nil {
		config.Ctx = context.Background()
	}

	tlsInfo := transport.TLSInfo{
		CertFile:           config.CertFile,
		KeyFile:            config.KeyFile,
		InsecureSkipVerify: config.InsecureSkipVerify,
	}

	tlsConfig, err := tlsInfo.ClientConfig()
	if err != nil {
		return nil, err
	}

	cli, err := clientv3.New(clientv3.Config{
		Context:     config.Ctx,
		Endpoints:   []string{config.Host},
		DialTimeout: etcdConnectionTimeout,
		Username:    config.User,
		Password:    config.Pass,
		TLS:         tlsConfig,
	})

	if err != nil {
		return nil, err
	}

	backend := &db{
		cli:    cli,
		config: config,
	}

	if config.CollectCommitStats {
		backend.commitStatsCollector = newCommitStatsColletor()
	}

	return backend, nil
}

// getSTMOptions creats all STM options based on the backend config.
func (db *db) getSTMOptions() []STMOptionFunc {
	opts := []STMOptionFunc{WithAbortContext(db.config.Ctx)}

	if db.config.CollectCommitStats {
		opts = append(opts,
			WithCommitStatsCallback(db.commitStatsCollector.callback),
		)
	}

	return opts
}

// View opens a database read transaction and executes the function f with the
// transaction passed as a parameter.  After f exits, the transaction is rolled
// back.  If f errors, its error is returned, not a rollback error (if any
// occur).
func (db *db) View(f func(tx walletdb.ReadTx) error) error {
	apply := func(stm STM) error {
		return f(newReadWriteTx(stm, db.config.Prefix))
	}

	return RunSTM(db.cli, apply, db.getSTMOptions()...)
}

// Update opens a database read/write transaction and executes the function f
// with the transaction passed as a parameter.  After f exits, if f did not
// error, the transaction is committed.  Otherwise, if f did error, the
// transaction is rolled back.  If the rollback fails, the original error
// returned by f is still returned.  If the commit fails, the commit error is
// returned.
func (db *db) Update(f func(tx walletdb.ReadWriteTx) error) error {
	apply := func(stm STM) error {
		return f(newReadWriteTx(stm, db.config.Prefix))
	}

	return RunSTM(db.cli, apply, db.getSTMOptions()...)
}

// PrintStats returns all collected stats pretty printed into a string.
func (db *db) PrintStats() string {
	if db.commitStatsCollector != nil {
		return db.commitStatsCollector.PrintStats()
	}

	return ""
}

// BeginReadTx opens a database read transaction.
func (db *db) BeginReadWriteTx() (walletdb.ReadWriteTx, error) {
	return newReadWriteTx(
		NewSTM(db.cli, db.getSTMOptions()...),
		db.config.Prefix,
	), nil
}

// BeginReadWriteTx opens a database read+write transaction.
func (db *db) BeginReadTx() (walletdb.ReadTx, error) {
	return newReadWriteTx(
		NewSTM(db.cli, db.getSTMOptions()...),
		db.config.Prefix,
	), nil
}

// Copy writes a copy of the database to the provided writer.  This call will
// start a read-only transaction to perform all operations.
// This function is part of the walletdb.Db interface implementation.
func (db *db) Copy(w io.Writer) error {
	ctx, cancel := context.WithTimeout(db.config.Ctx, etcdLongTimeout)
	defer cancel()

	readCloser, err := db.cli.Snapshot(ctx)
	if err != nil {
		return err
	}

	_, err = io.Copy(w, readCloser)

	return err
}

// Close cleanly shuts down the database and syncs all data.
// This function is part of the walletdb.Db interface implementation.
func (db *db) Close() error {
	return db.cli.Close()
}

// Batch opens a database read/write transaction and executes the function f
// with the transaction passed as a parameter.  After f exits, if f did not
// error, the transaction is committed.  Otherwise, if f did error, the
// transaction is rolled back.  If the rollback fails, the original error
// returned by f is still returned.  If the commit fails, the commit error is
// returned.
//
// Batch is only useful when there are multiple goroutines calling it.
func (db *db) Batch(apply func(tx walletdb.ReadWriteTx) error) error {
	return db.Update(apply)
}
