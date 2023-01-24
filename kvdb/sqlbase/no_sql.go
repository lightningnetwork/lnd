//go:build !kvdb_postgres && (!kvdb_sqlite || (windows && (arm || 386)) || (linux && (ppc64 || mips || mipsle || mips64)))

package sqlbase

func Init(maxConnections int) {}
