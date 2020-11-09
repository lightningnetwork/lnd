package kvdb

// BoltBackendName is the name of the backend that should be passed into
// kvdb.Create to initialize a new instance of kvdb.Backend backed by a live
// instance of bbolt.
const BoltBackendName = "bdb"

// EtcdBackendName is the name of the backend that should be passed into
// kvdb.Create to initialize a new instance of kvdb.Backend backed by a live
// instance of etcd.
const EtcdBackendName = "etcd"

// BoltConfig holds bolt configuration.
type BoltConfig struct {
	SyncFreelist bool `long:"nofreelistsync" description:"Whether the databases used within lnd should sync their freelist to disk. This is disabled by default resulting in improved memory performance during operation, but with an increase in startup time."`
}

// EtcdConfig holds etcd configuration.
type EtcdConfig struct {
	Host string `long:"host" description:"Etcd database host."`

	User string `long:"user" description:"Etcd database user."`

	Pass string `long:"pass" description:"Password for the database user."`

	CertFile string `long:"cert_file" description:"Path to the TLS certificate for etcd RPC."`

	KeyFile string `long:"key_file" description:"Path to the TLS private key for etcd RPC."`

	InsecureSkipVerify bool `long:"insecure_skip_verify" description:"Whether we intend to skip TLS verification"`

	CollectStats bool `long:"collect_stats" description:"Whether to collect etcd commit stats."`
}
