package sqlite

import "time"

// Config holds sqlite configuration data.
//
//nolint:ll
type Config struct {
	Timeout        time.Duration `long:"timeout" description:"The time after which a database query should be timed out."`
	BusyTimeout    time.Duration `long:"busytimeout" description:"The maximum amount of time to wait for a database connection to become available for a query."`
	MaxConnections int           `long:"maxconnections" description:"The maximum number of open connections to the database. Set to zero for unlimited."`
	PragmaOptions  []string      `long:"pragmaoptions" description:"A list of pragma options to set on a database connection. For example, 'auto_vacuum=incremental'. Note that the flag must be specified multiple times if multiple options are to be set."`

	// Quit is an optional channel that is closed once the daemon starts
	// shutting down. It is used to abort an in-flight transaction retry
	// loop, so that a transaction which keeps hitting serialization errors
	// can't delay shutdown for the length of the retry budget.
	//
	// NOTE: This is injected at runtime and is deliberately not a command
	// line flag.
	Quit <-chan struct{}
}
