package sqlbase

import (
	"database/sql"
	"fmt"
	"sync"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// dbConn stores the actual connection and a user count.
type dbConn struct {
	db     *sql.DB
	driver string
	count  int
}

// serverDriver is the name of the driver whose pools all talk to one database
// server and therefore share that server's connection limit. Other drivers,
// such as SQLite, open a separate database per DSN with no shared limit.
const serverDriver = "pgx"

// dbConnSet stores a set of connections.
type dbConnSet struct {
	dbConn map[string]*dbConn

	// maxConnections is the limit on open connections that is shared by all
	// pools in the set. Zero means unlimited.
	maxConnections int

	// mu is used to guard access to the dbConn map.
	mu sync.Mutex
}

// newDbConnSet initializes a new set of connections.
func newDbConnSet(maxConnections int) *dbConnSet {
	return &dbConnSet{
		dbConn:         make(map[string]*dbConn),
		maxConnections: maxConnections,
	}
}

// Open opens a new database connection. If a connection already exists for the
// given dsn, the existing connection is returned.
func (d *dbConnSet) Open(driver, dsn string) (*sql.DB, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if dbConn, ok := d.dbConn[dsn]; ok {
		dbConn.count++

		return dbConn.db, nil
	}

	db, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, err
	}

	d.dbConn[dsn] = &dbConn{
		db:     db,
		driver: driver,
		count:  1,
	}

	// A pool without a shared server keeps the full limit.
	if driver != serverDriver && d.maxConnections != 0 {
		db.SetMaxOpenConns(d.maxConnections)
	}
	d.rebalance()

	return db, nil
}

// rebalance divides the connection limit evenly among the server pools in the
// set, which are the ones that use serverDriver. This is useful to prevent the
// server from running out of connections and returning an error: because the
// per-pool limits add up to at most the shared limit, the pools together never
// open more connections than that, no matter how many distinct DSNs are in
// use. With this client-side limit in place, lnd will wait for a connection to
// become available. Every pool keeps a limit of at least one connection, so
// the shared limit is only exceeded if there are more pools than connections.
// The caller must hold d.mu. Both callers, Open and Close, do. The method only
// reads d.dbConn and calls SetMaxOpenConns, which takes the lock of the sql.DB
// pool but never calls back into the set, so there is no lock cycle. It must
// not take a per-pool lock of its own.
func (d *dbConnSet) rebalance() {
	if d.maxConnections == 0 {
		return
	}

	var pools []*dbConn
	for _, c := range d.dbConn {
		if c.driver == serverDriver {
			pools = append(pools, c)
		}
	}
	if len(pools) == 0 {
		return
	}

	share := max(1, d.maxConnections/len(pools))
	for _, c := range pools {
		c.db.SetMaxOpenConns(share)
	}
}

// Close closes the connection with the given dsn. If there are still other
// users of the same connection, this function does nothing.
func (d *dbConnSet) Close(dsn string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	dbConn, ok := d.dbConn[dsn]
	if !ok {
		return fmt.Errorf("connection not found: %v", dsn)
	}

	// Reduce user count.
	dbConn.count--

	// Do not close if there are other users.
	if dbConn.count > 0 {
		return nil
	}

	// Close connection.
	delete(d.dbConn, dsn)
	d.rebalance()

	return dbConn.db.Close()
}
