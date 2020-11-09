package kvdb

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

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

// BoltBackendConfig is a struct that holds settings specific to the bolt
// database backend.
type BoltBackendConfig struct {
	// DBPath is the directory path in which the database file should be
	// stored.
	DBPath string

	// DBFileName is the name of the database file.
	DBFileName string

	// NoFreelistSync, if true, prevents the database from syncing its
	// freelist to disk, resulting in improved performance at the expense of
	// increased startup time.
	NoFreelistSync bool

	// AutoCompact specifies if a Bolt based database backend should be
	// automatically compacted on startup (if the minimum age of the
	// database file is reached). This will require additional disk space
	// for the compacted copy of the database but will result in an overall
	// lower database size after the compaction.
	AutoCompact bool

	// AutoCompactMinAge specifies the minimum time that must have passed
	// since a bolt database file was last compacted for the compaction to
	// be considered again.
	AutoCompactMinAge time.Duration
}

// GetBoltBackend opens (or creates if doesn't exits) a bbolt
// backed database and returns a kvdb.Backend wrapping it.
func GetBoltBackend(cfg *BoltBackendConfig) (Backend, error) {
	dbFilePath := filepath.Join(cfg.DBPath, cfg.DBFileName)
	var (
		db  Backend
		err error
	)

	if !fileExists(dbFilePath) {
		if !fileExists(cfg.DBPath) {
			if err := os.MkdirAll(cfg.DBPath, 0700); err != nil {
				return nil, err
			}
		}

		db, err = Create(BoltBackendName, dbFilePath, cfg.NoFreelistSync)
	} else {
		db, err = Open(BoltBackendName, dbFilePath, cfg.NoFreelistSync)
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
		db, err := GetBoltBackend(&BoltBackendConfig{
			DBPath:         path,
			DBFileName:     name,
			NoFreelistSync: true,
		})
		if err != nil {
			return nil, nil, err
		}
		return db, empty, nil
	} else if TestBackend == EtcdBackendName {
		return GetEtcdTestBackend(path, name)
	}

	return nil, nil, fmt.Errorf("unknown backend")
}
