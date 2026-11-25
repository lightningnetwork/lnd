package sqlbase

import (
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// noServerDriver is a driver that is never connected to, standing in for a
// driver like SQLite that has no shared server limit.
type noServerDriver struct{}

// Open implements driver.Driver.
func (noServerDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("not implemented")
}

func init() {
	sql.Register("nosrv", noServerDriver{})
}

// maxOpen returns the open connection limit of the pool for the given dsn.
func maxOpen(d *dbConnSet, dsn string) int {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.dbConn[dsn].db.Stats().MaxOpenConnections
}

// maxOpenSum returns the sum of the open connection limits of all pools in the
// set that use the given driver.
func maxOpenSum(d *dbConnSet, driver string) int {
	d.mu.Lock()
	defer d.mu.Unlock()

	var sum int
	for _, c := range d.dbConn {
		if c.driver == driver {
			sum += c.db.Stats().MaxOpenConnections
		}
	}

	return sum
}

// openPools opens n pools with distinct DSNs. The pools are opened lazily by
// the driver, so no server is needed.
func openPools(t *testing.T, d *dbConnSet, drv, name string,
	n int) []string {

	t.Helper()

	dsns := make([]string, 0, n)
	for i := range n {
		dsn := fmt.Sprintf("%s%d", name, i)
		dsns = append(dsns, dsn)

		_, err := d.Open(drv, dsn)
		require.NoError(t, err)
	}

	return dsns
}

// TestDbConnSetBudget asserts that the connection limit is shared by all pools
// of a server driver, so that the pools together can never open more
// connections than the limit no matter how many distinct DSNs are used, and
// that drivers without a shared server are not limited by it.
func TestDbConnSetBudget(t *testing.T) {
	const (
		budget = 200
		pools  = 40
	)

	// Server pools share the budget and hand it back when closed.
	t.Run("pgx", func(t *testing.T) {
		d := newDbConnSet(budget)

		for i := range pools {
			openPools(t, d, "pgx", fmt.Sprintf("p%d-", i), 1)
			require.LessOrEqual(t, maxOpenSum(d, "pgx"), budget)
		}

		dsns := make([]string, 0, pools)
		for i := range pools {
			dsns = append(dsns, fmt.Sprintf("p%d-0", i))
		}

		for _, dsn := range dsns[:pools-1] {
			require.NoError(t, d.Close(dsn))
			require.LessOrEqual(t, maxOpenSum(d, "pgx"), budget)
		}
		require.Equal(t, budget, maxOpenSum(d, "pgx"))
		require.Equal(t, budget, maxOpen(d, dsns[pools-1]))
	})

	// Pools without a shared server each keep the full limit.
	t.Run("no shared server", func(t *testing.T) {
		d := newDbConnSet(budget)

		for _, dsn := range openPools(t, d, "nosrv", "s", pools) {
			require.Equal(t, budget, maxOpen(d, dsn))
		}
	})

	// Pools of different drivers do not affect each other.
	t.Run("mixed", func(t *testing.T) {
		d := newDbConnSet(budget)

		srv := openPools(t, d, "pgx", "p", 10)
		other := openPools(t, d, "nosrv", "s", 10)

		for _, dsn := range srv {
			require.Equal(t, budget/10, maxOpen(d, dsn))
		}
		for _, dsn := range other {
			require.Equal(t, budget, maxOpen(d, dsn))
		}

		for _, dsn := range srv[:5] {
			require.NoError(t, d.Close(dsn))
		}
		for _, dsn := range srv[5:] {
			require.Equal(t, budget/5, maxOpen(d, dsn))
		}
		for _, dsn := range other {
			require.Equal(t, budget, maxOpen(d, dsn))
		}
	})
}
