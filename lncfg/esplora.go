package lncfg

import "time"

const (
	// DefaultEsploraPollInterval is the default interval for polling
	// the Esplora API for new blocks.
	DefaultEsploraPollInterval = 10 * time.Second

	// DefaultEsploraRequestTimeout is the default timeout for HTTP
	// requests to the Esplora API.
	DefaultEsploraRequestTimeout = 30 * time.Second

	// DefaultEsploraMaxRetries is the default number of times to retry
	// a failed request before giving up.
	DefaultEsploraMaxRetries = 3
)

// Esplora holds the configuration options for the daemon's connection to
// an Esplora HTTP API server (e.g., mempool.space, blockstream.info, or
// a local electrs/mempool instance).
//
//nolint:ll
type Esplora struct {
	// URL is the base URL of the Esplora API to connect to.
	// Examples:
	//   - http://localhost:3002 (local electrs/mempool)
	//   - https://blockstream.info/api (Blockstream mainnet)
	//   - https://mempool.space/api (mempool.space mainnet)
	//   - https://mempool.space/testnet/api (mempool.space testnet)
	URL string `long:"url" description:"The base URL of the Esplora API (e.g., http://localhost:3002)"`

	// RequestTimeout is the timeout for HTTP requests sent to the Esplora
	// API.
	RequestTimeout time.Duration `long:"requesttimeout" description:"Timeout for HTTP requests to the Esplora API."`

	// MaxRetries is the maximum number of times to retry a failed request.
	MaxRetries int `long:"maxretries" description:"Maximum number of times to retry a failed request."`

	// PollInterval is the interval at which to poll for new blocks.
	// Since Esplora is HTTP-only, we need to poll rather than subscribe.
	PollInterval time.Duration `long:"pollinterval" description:"Interval at which to poll for new blocks."`
}

// DefaultEsploraConfig returns a new Esplora config with default values
// populated.
func DefaultEsploraConfig() *Esplora {
	return &Esplora{
		RequestTimeout: DefaultEsploraRequestTimeout,
		MaxRetries:     DefaultEsploraMaxRetries,
		PollInterval:   DefaultEsploraPollInterval,
	}
}
