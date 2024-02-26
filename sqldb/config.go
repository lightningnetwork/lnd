package sqldb

import (
	"errors"
	"fmt"

	"github.com/lightningnetwork/lnd/kvdb/postgres"
	"github.com/lightningnetwork/lnd/kvdb/sqlite"
)

const (
	PostgresBackend = "postgres"
	SqliteBackend   = "sqlite"
)

var (
	ErrMissingConfig  = errors.New("missing backend config")
	ErrUnknownBackend = errors.New("unknown backend")
)

//nolint:lll
type Config struct {
	Backend string `long:"backend" description:"The selected database backend." hidden:"true"`

	Postgres *postgres.Config `group:"sql.postgres" namespace:"postgres" description:"Postgres settings."`

	Sqlite *sqlite.Config `group:"sql.sqlite" namespace:"sqlite" description:"Sqlite settings."`
}

func (c *Config) Validate() error {
	switch c.Backend {
	case PostgresBackend:
		if c.Postgres == nil {
			return fmt.Errorf("%w for postgres", ErrMissingConfig)
		}

		return c.Postgres.Validate()

	case SqliteBackend:
		if c.Sqlite == nil {
			return fmt.Errorf("%w for sqlite", ErrMissingConfig)
		}

		return c.Sqlite.Validate()

	default:
		return fmt.Errorf("%w: %v", ErrUnknownBackend, c.Backend)
	}
}
