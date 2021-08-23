//go:build kvdb_etcd
// +build kvdb_etcd

package etcd

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcwallet/walletdb"
)

const (
	dbType = "etcd"
)

// parseArgs parses the arguments from the walletdb Open/Create methods.
func parseArgs(funcName string, args ...interface{}) (context.Context,
	*Config, error) {

	if len(args) != 2 {
		return nil, nil, fmt.Errorf("invalid number of arguments to "+
			"%s.%s -- expected: context.Context, etcd.Config",
			dbType, funcName,
		)
	}

	ctx, ok := args[0].(context.Context)
	if !ok {
		return nil, nil, fmt.Errorf("argument 0 to %s.%s is invalid "+
			"-- expected: context.Context",
			dbType, funcName,
		)
	}

	config, ok := args[1].(*Config)
	if !ok {
		return nil, nil, fmt.Errorf("argument 1 to %s.%s is invalid -- "+
			"expected: etcd.Config",
			dbType, funcName,
		)
	}

	return ctx, config, nil
}

// createDBDriver is the callback provided during driver registration that
// creates, initializes, and opens a database for use.
func createDBDriver(args ...interface{}) (walletdb.DB, error) {
	ctx, config, err := parseArgs("Create", args...)
	if err != nil {
		return nil, err
	}

	return newEtcdBackend(ctx, *config)
}

// openDBDriver is the callback provided during driver registration that opens
// an existing database for use.
func openDBDriver(args ...interface{}) (walletdb.DB, error) {
	ctx, config, err := parseArgs("Open", args...)
	if err != nil {
		return nil, err
	}

	return newEtcdBackend(ctx, *config)
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
