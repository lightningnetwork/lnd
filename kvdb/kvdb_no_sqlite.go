//go:build !kvdb_sqlite || (windows && (arm || 386)) || (linux && (ppc64 || mips || mipsle || mips64))

package kvdb

import (
	"fmt"
	"runtime"

	"github.com/btcsuite/btcwallet/walletdb"
)

var errSqliteNotAvailable = fmt.Errorf("sqlite backend not available either "+
	"due to the `kvdb_sqlite` build tag not being set, or due to this "+
	"OS(%s) and/or architecture(%s) not being supported", runtime.GOOS,
	runtime.GOARCH)

// SqliteBackend is conditionally set to false when the kvdb_sqlite build tag is
// not defined. This will allow testing of other database backends.
const SqliteBackend = false

// StartSqliteTestBackend is a stub returning nil, and errSqliteNotAvailable
// error.
func StartSqliteTestBackend(path, name, table string) (walletdb.DB, error) {
	return nil, errSqliteNotAvailable
}
