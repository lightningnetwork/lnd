package postgres

import "time"

// Config holds postgres configuration data.
//
//nolint:ll
type Config struct {
	Dsn            string        `long:"dsn" description:"Database connection string."`
	Timeout        time.Duration `long:"timeout" description:"Database connection timeout. Set to zero to disable."`
	MaxConnections int           `long:"maxconnections" description:"The maximum number of open connections to the database. Set to zero for unlimited."`
	WithGlobalLock bool          `long:"withgloballock" description:"Use a global lock to ensure a single writer."`

	// Quit is an optional channel that is closed once the daemon starts
	// shutting down. It is used to abort an in-flight transaction retry
	// loop, so that a transaction which keeps hitting serialization errors
	// can't delay shutdown for the length of the retry budget.
	//
	// NOTE: This is injected at runtime and is deliberately not a command
	// line flag.
	Quit <-chan struct{}
}
