//go:build !kvdb_postgres

package sqlbase

func Init(maxConnections int) {}
