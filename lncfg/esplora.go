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

	// DefaultGapLimit is the default gap limit for address scanning.
	// This follows BIP-44 which specifies 20 consecutive unused addresses
	// as the stopping point for address discovery.
	DefaultGapLimit = 20

	// DefaultAddressBatchSize is the default number of addresses to query
	// concurrently when scanning with gap limit.
	DefaultAddressBatchSize = 10
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

	// UseGapLimit enables gap limit optimization for wallet recovery.
	// When enabled, address scanning stops after finding GapLimit
	// consecutive unused addresses, dramatically improving recovery time.
	UseGapLimit bool `long:"usegaplimit" description:"Enable gap limit optimization for wallet recovery (recommended)."`

	// GapLimit is the number of consecutive unused addresses to scan
	// before stopping. BIP-44 specifies 20 as the standard gap limit.
	// Higher values may be needed for wallets with non-sequential usage.
	GapLimit int `long:"gaplimit" description:"Number of consecutive unused addresses before stopping scan (default: 20)."`

	// AddressBatchSize is the number of addresses to query concurrently
	// when using gap limit scanning. Higher values increase speed but
	// may trigger rate limiting on public APIs.
	AddressBatchSize int `long:"addressbatchsize" description:"Number of addresses to query concurrently (default: 10)."`
}

// DefaultEsploraConfig returns a new Esplora config with default values
// populated.
func DefaultEsploraConfig() *Esplora {
	return &Esplora{
		RequestTimeout:   DefaultEsploraRequestTimeout,
		MaxRetries:       DefaultEsploraMaxRetries,
		PollInterval:     DefaultEsploraPollInterval,
		UseGapLimit:      true,
		GapLimit:         DefaultGapLimit,
		AddressBatchSize: DefaultAddressBatchSize,
	}
}
