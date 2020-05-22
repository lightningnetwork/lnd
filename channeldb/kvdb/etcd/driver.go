// +build kvdb_etcd

package etcd

import (
	"fmt"

	"github.com/btcsuite/btcwallet/walletdb"
)

const (
	dbType = "etcd"
)

// parseArgs parses the arguments from the walletdb Open/Create methods.
func parseArgs(funcName string, args ...interface{}) (*BackendConfig, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("invalid number of arguments to %s.%s -- "+
			"expected: etcd.BackendConfig",
			dbType, funcName,
		)
	}

	config, ok := args[0].(BackendConfig)
	if !ok {
		return nil, fmt.Errorf("argument to %s.%s is invalid -- "+
			"expected: etcd.BackendConfig",
			dbType, funcName,
		)
	}

	return &config, nil
}

// createDBDriver is the callback provided during driver registration that
// creates, initializes, and opens a database for use.
func createDBDriver(args ...interface{}) (walletdb.DB, error) {
	config, err := parseArgs("Create", args...)
	if err != nil {
		return nil, err
	}

	return newEtcdBackend(*config)
}

// openDBDriver is the callback provided during driver registration that opens
// an existing database for use.
func openDBDriver(args ...interface{}) (walletdb.DB, error) {
	config, err := parseArgs("Open", args...)
	if err != nil {
		return nil, err
	}

	return newEtcdBackend(*config)
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
