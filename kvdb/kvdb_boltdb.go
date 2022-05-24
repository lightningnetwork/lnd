//go:build !kvdb_etcd && !kvdb_postgres && !kvdb_sqlite
// +build !kvdb_etcd,!kvdb_postgres,!kvdb_sqlite

package kvdb

// When no other db backends are specified via dev flags, boltdb will be the default test db backend.
const TestBackend = BoltBackendName
