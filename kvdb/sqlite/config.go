package sqlite

import "time"

// Config holds sqlite configuration data.
type Config struct {
	TablePrefix string        `long:"table_prefix" description:"Prefix that will be added to each database table."`
	Filename    string        `long:"filename" description:"Full path to sqlite database file."`
	Timeout     time.Duration `long:"timeout" description:"Database connection timeout. Set to zero to disable."`
}
