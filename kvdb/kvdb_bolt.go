// +build !kvdb_etcd,!kvdb_postgres

package kvdb

// GetTestBackend opens (or creates if doesn't exist) a bbolt backed database
// (for testing), and returns a kvdb.Backend and a cleanup func. The passed path
// is used to hold all db files. Name is the bolt database filename.
func GetTestBackend(path, name string) (Backend, func(), error) {
	db, err := GetBoltBackend(&BoltBackendConfig{
		DBPath:         path,
		DBFileName:     name,
		NoFreelistSync: true,
		DBTimeout:      DefaultDBTimeout,
	})
	if err != nil {
		return nil, nil, err
	}
	return db, func() {}, nil
}

func SetupTestBackend() error {
	return nil
}

func TearDownTestBackend() {

}
