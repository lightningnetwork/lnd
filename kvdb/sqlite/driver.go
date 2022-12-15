//go:build kvdb_sqlite && !(windows && (arm || 386)) && !(linux && (ppc64 || mips || mipsle || mips64))

package sqlite

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcwallet/walletdb"
)

const (
	dbType = "sqlite"
)

// parseArgs parses the arguments from the walletdb Open/Create methods.
func parseArgs(funcName string, args ...interface{}) (context.Context, *Config,
	string, string, string, error) {

	if len(args) != 5 {
		return nil, nil, "", "", "", fmt.Errorf("invalid number of "+
			"arguments to %s.%s -- expected: context.Context, "+
			"sql.Config, string, string, string", dbType, funcName)
	}

	ctx, ok := args[0].(context.Context)
	if !ok {
		return nil, nil, "", "", "", fmt.Errorf("argument 0 to %s.%s "+
			"is invalid -- expected: context.Context", dbType,
			funcName)
	}

	config, ok := args[1].(*Config)
	if !ok {
		return nil, nil, "", "", "", fmt.Errorf("argument 1 to %s.%s "+
			"is invalid -- expected: sqlite.Config", dbType,
			funcName)
	}

	dbPath, ok := args[2].(string)
	if !ok {
		return nil, nil, "", "", "", fmt.Errorf("argument 2 to %s.%s "+
			"is invalid -- expected string", dbType, dbPath)
	}

	fileName, ok := args[3].(string)
	if !ok {
		return nil, nil, "", "", "", fmt.Errorf("argument 3 to %s.%s "+
			"is invalid -- expected string", dbType, funcName)
	}

	prefix, ok := args[4].(string)
	if !ok {
		return nil, nil, "", "", "", fmt.Errorf("argument 4 to %s.%s "+
			"is invalid -- expected string", dbType, funcName,
		)
	}

	return ctx, config, dbPath, fileName, prefix, nil
}

// createDBDriver is the callback provided during driver registration that
// creates, initializes, and opens a database for use.
func createDBDriver(args ...interface{}) (walletdb.DB, error) {
	ctx, config, dbPath, filename, prefix, err := parseArgs(
		"Create", args...,
	)
	if err != nil {
		return nil, err
	}

	return NewSqliteBackend(ctx, config, dbPath, filename, prefix)
}

// openDBDriver is the callback provided during driver registration that opens
// an existing database for use.
func openDBDriver(args ...interface{}) (walletdb.DB, error) {
	ctx, config, dbPath, filename, prefix, err := parseArgs("Open", args...)
	if err != nil {
		return nil, err
	}

	return NewSqliteBackend(ctx, config, dbPath, filename, prefix)
}

func init() {
	// Register the driver.
	driver := walletdb.Driver{
		DbType: dbType,
		Create: createDBDriver,
		Open:   openDBDriver,
	}
	if err := walletdb.RegisterDriver(driver); err != nil {
		panic(fmt.Sprintf("Failed to regiser database driver '%s': %v",
			dbType, err))
	}
}
