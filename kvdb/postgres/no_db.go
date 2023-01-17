//go:build !kvdb_postgres

package postgres

func Init(maxConnections int) {}
