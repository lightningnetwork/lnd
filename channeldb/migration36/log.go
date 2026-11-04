package migration36

import "github.com/btcsuite/btclog/v2"

// log is disabled until the database package supplies its logger.
var log = btclog.Disabled

// UseLogger sets the migration package logger.
func UseLogger(logger btclog.Logger) {
	log = logger
}
