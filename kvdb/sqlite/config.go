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
}
