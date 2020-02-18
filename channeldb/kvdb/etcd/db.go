package etcd

import (
	"context"
	"io"
	"time"

	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/coreos/etcd/clientv3"
)

const (
	// etcdConnectionTimeout is the timeout until successful connection to the
	// etcd instance.
	etcdConnectionTimeout = 10 * time.Second

	// etcdLongTimeout is a timeout for longer taking etcd operatons.
	etcdLongTimeout = 30 * time.Second
)

// db holds a reference to the etcd client connection.
type db struct {
	cli *clientv3.Client
}

// Enforce db implements the walletdb.DB interface.
var _ walletdb.DB = (*db)(nil)

// BackendConfig holds and etcd backend config and connection parameters.
type BackendConfig struct {
	// Host holds the peer url of the etcd instance.
	Host string

	// User is the username for the etcd peer.
	User string

	// Pass is the password for the etcd peer.
	Pass string
}

// newEtcdBackend returns a db object initialized with the passed backend
// config. If etcd connection cannot be estabished, then returns error.
func newEtcdBackend(config BackendConfig) (*db, error) {
	cli, err := clientv3.New(clientv3.Config{
		Endpoints:   []string{config.Host},
		DialTimeout: etcdConnectionTimeout,
		Username:    config.User,
		Password:    config.Pass,
	})
	if err != nil {
		return nil, err
	}

	backend := &db{
		cli: cli,
	}

	return backend, nil
}

// View opens a database read transaction and executes the function f with the
// transaction passed as a parameter.  After f exits, the transaction is rolled
// back.  If f errors, its error is returned, not a rollback error (if any
// occur).
func (db *db) View(f func(tx walletdb.ReadTx) error) error {
	apply := func(stm STM) error {
		return f(newReadWriteTx(stm))
	}

	return RunSTM(db.cli, apply)
}

// Update opens a database read/write transaction and executes the function f
// with the transaction passed as a parameter.  After f exits, if f did not
// error, the transaction is committed.  Otherwise, if f did error, the
// transaction is rolled back.  If the rollback fails, the original error
// returned by f is still returned.  If the commit fails, the commit error is
// returned.
func (db *db) Update(f func(tx walletdb.ReadWriteTx) error) error {
	apply := func(stm STM) error {
		return f(newReadWriteTx(stm))
	}

	return RunSTM(db.cli, apply)
}

// BeginReadTx opens a database read transaction.
func (db *db) BeginReadWriteTx() (walletdb.ReadWriteTx, error) {
	return newReadWriteTx(NewSTM(db.cli)), nil
}

// BeginReadWriteTx opens a database read+write transaction.
func (db *db) BeginReadTx() (walletdb.ReadTx, error) {
	return newReadWriteTx(NewSTM(db.cli)), nil
}

// Copy writes a copy of the database to the provided writer.  This call will
// start a read-only transaction to perform all operations.
// This function is part of the walletdb.Db interface implementation.
func (db *db) Copy(w io.Writer) error {
	ctx := context.Background()

	ctx, cancel := context.WithTimeout(ctx, etcdLongTimeout)
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
