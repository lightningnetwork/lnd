package kvdb

import (
	"fmt"
	"os"
	"path/filepath"

	_ "github.com/btcsuite/btcwallet/walletdb/bdb" // Import to register backend.
)

// fileExists returns true if the file exists, and false otherwise.
func fileExists(path string) bool {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return false
		}
	}

	return true
}

// GetBoltBackend opens (or creates if doesn't exits) a bbolt
// backed database and returns a kvdb.Backend wrapping it.
func GetBoltBackend(path, name string, noFreeListSync bool) (Backend, error) {
	dbFilePath := filepath.Join(path, name)
	var (
		db  Backend
		err error
	)

	if !fileExists(dbFilePath) {
		if !fileExists(path) {
			if err := os.MkdirAll(path, 0700); err != nil {
				return nil, err
			}
		}

		db, err = Create(BoltBackendName, dbFilePath, noFreeListSync)
	} else {
		db, err = Open(BoltBackendName, dbFilePath, noFreeListSync)
	}

	if err != nil {
		return nil, err
	}

	return db, nil
}

// GetTestBackend opens (or creates if doesn't exist) a bbolt or etcd
// backed database (for testing), and returns a kvdb.Backend and a cleanup
// func. Whether to create/open bbolt or embedded etcd database is based
// on the TestBackend constant which is conditionally compiled with build tag.
// The passed path is used to hold all db files, while the name is only used
// for bbolt.
func GetTestBackend(path, name string) (Backend, func(), error) {
	empty := func() {}

	if TestBackend == BoltBackendName {
		db, err := GetBoltBackend(path, name, true)
		if err != nil {
			return nil, nil, err
		}
		return db, empty, nil
	} else if TestBackend == EtcdBackendName {
		return GetEtcdTestBackend(path, name)
	}

	return nil, nil, fmt.Errorf("unknown backend")
}
