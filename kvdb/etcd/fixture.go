//go:build kvdb_etcd
// +build kvdb_etcd

package etcd

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/client/v3/namespace"
)

const (
	// testEtcdTimeout is used for all RPC calls initiated by the test fixture.
	testEtcdTimeout = 5 * time.Second
)

// EtcdTestFixture holds internal state of the etcd test fixture.
type EtcdTestFixture struct {
	t      *testing.T
	cli    *clientv3.Client
	config *Config
}

// NewTestEtcdInstance creates an embedded etcd instance for testing, listening
// on random open ports. Returns the connection config and a cleanup func that
// will stop the etcd instance.
func NewTestEtcdInstance(t *testing.T, path string) (*Config, func()) {
	t.Helper()

	config, cleanup, err := NewEmbeddedEtcdInstance(path, 0, 0, "")
	if err != nil {
		t.Fatalf("error while staring embedded etcd instance: %v", err)
	}

	return config, cleanup
}

// NewEtcdTestFixture creates a new etcd-test fixture. This is helper
// object to facilitate etcd tests and ensure pre- and post-conditions.
func NewEtcdTestFixture(t *testing.T) *EtcdTestFixture {
	tmpDir := t.TempDir()

	config, etcdCleanup := NewTestEtcdInstance(t, tmpDir)
	t.Cleanup(etcdCleanup)

	cli, err := clientv3.New(clientv3.Config{
		Endpoints: strings.Split(config.Host, ","),
		Username:  config.User,
		Password:  config.Pass,
	})
	if err != nil {
		t.Fatalf("unable to create etcd test fixture: %v", err)
	}

	// Apply the default namespace (since that's what we use in tests).
	cli.KV = namespace.NewKV(cli.KV, defaultNamespace)
	cli.Watcher = namespace.NewWatcher(cli.Watcher, defaultNamespace)
	cli.Lease = namespace.NewLease(cli.Lease, defaultNamespace)

	return &EtcdTestFixture{
		t:      t,
		cli:    cli,
		config: config,
	}
}

func (f *EtcdTestFixture) NewBackend(singleWriter bool) walletdb.DB {
	cfg := f.BackendConfig()
	if singleWriter {
		cfg.SingleWriter = true
	}

	db, err := newEtcdBackend(context.Background(), cfg)
	require.NoError(f.t, err)

	return db
}

// Put puts a string key/value into the test etcd database.
func (f *EtcdTestFixture) Put(key, value string) {
	ctx, cancel := context.WithTimeout(
		context.Background(), testEtcdTimeout,
	)
	defer cancel()

	_, err := f.cli.Put(ctx, key, value)
	if err != nil {
		f.t.Fatalf("etcd test fixture failed to put: %v", err)
	}
}

// Get queries a key and returns the stored value from the test etcd database.
func (f *EtcdTestFixture) Get(key string) string {
	ctx, cancel := context.WithTimeout(
		context.Background(), testEtcdTimeout,
	)
	defer cancel()

	resp, err := f.cli.Get(ctx, key)
	if err != nil {
		f.t.Fatalf("etcd test fixture failed to get: %v", err)
	}

	if len(resp.Kvs) > 0 {
		return string(resp.Kvs[0].Value)
	}

	return ""
}

// Dump scans and returns all key/values from the test etcd database.
func (f *EtcdTestFixture) Dump() map[string]string {
	ctx, cancel := context.WithTimeout(
		context.Background(), testEtcdTimeout,
	)
	defer cancel()

	resp, err := f.cli.Get(ctx, "\x00", clientv3.WithFromKey())
	if err != nil {
		f.t.Fatalf("etcd test fixture failed to get: %v", err)
	}

	result := make(map[string]string)
	for _, kv := range resp.Kvs {
		result[string(kv.Key)] = string(kv.Value)
	}

	return result
}

// BackendConfig returns the backend config for connecting to the embedded
// etcd instance.
func (f *EtcdTestFixture) BackendConfig() Config {
	return *f.config
}
