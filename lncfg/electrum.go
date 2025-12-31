package lncfg

import "time"

const (
	// DefaultElectrumPort is the default port that Electrum servers use
	// for TCP connections.
	DefaultElectrumPort = "50001"

	// DefaultElectrumSSLPort is the default port that Electrum servers use
	// for SSL/TLS connections.
	DefaultElectrumSSLPort = "50002"

	// DefaultElectrumReconnectInterval is the default interval between
	// reconnection attempts when the connection to the Electrum server is
	// lost.
	DefaultElectrumReconnectInterval = 10 * time.Second

	// DefaultElectrumRequestTimeout is the default timeout for RPC
	// requests to the Electrum server.
	DefaultElectrumRequestTimeout = 30 * time.Second

	// DefaultElectrumPingInterval is the default interval at which ping
	// messages are sent to the Electrum server to keep the connection
	// alive.
	DefaultElectrumPingInterval = 60 * time.Second

	// DefaultElectrumMaxRetries is the default number of times to retry
	// a failed request before giving up.
	DefaultElectrumMaxRetries = 3
)

// Electrum holds the configuration options for the daemon's connection to
// an Electrum server.
//
//nolint:ll
type Electrum struct {
	// Server is the host:port of the Electrum server to connect to.
	Server string `long:"server" description:"The host:port of the Electrum server to connect to."`

	// UseSSL specifies whether to use SSL/TLS for the connection to the
	// Electrum server.
	UseSSL bool `long:"ssl" description:"Use SSL/TLS for the connection to the Electrum server."`

	// TLSCertPath is the path to the Electrum server's TLS certificate.
	// If not set and UseSSL is true, the system's certificate pool will
	// be used for verification.
	TLSCertPath string `long:"tlscertpath" description:"Path to the Electrum server's TLS certificate for verification."`

	// TLSSkipVerify skips TLS certificate verification. This is insecure
	// and should only be used for testing.
	TLSSkipVerify bool `long:"tlsskipverify" description:"Skip TLS certificate verification. Insecure, use for testing only."`

	// ReconnectInterval is the time to wait between reconnection attempts
	// when the connection to the Electrum server is lost.
	ReconnectInterval time.Duration `long:"reconnectinterval" description:"Interval between reconnection attempts."`

	// RequestTimeout is the timeout for RPC requests sent to the Electrum
	// server.
	RequestTimeout time.Duration `long:"requesttimeout" description:"Timeout for RPC requests to the Electrum server."`

	// PingInterval is the interval at which ping messages are sent to keep
	// the connection alive.
	PingInterval time.Duration `long:"pinginterval" description:"Interval at which ping messages are sent to keep the connection alive."`

	// MaxRetries is the maximum number of times to retry a failed request.
	MaxRetries int `long:"maxretries" description:"Maximum number of times to retry a failed request."`
}

// DefaultElectrumConfig returns a new Electrum config with default values
// populated.
func DefaultElectrumConfig() *Electrum {
	return &Electrum{
		UseSSL:            true,
		ReconnectInterval: DefaultElectrumReconnectInterval,
		RequestTimeout:    DefaultElectrumRequestTimeout,
		PingInterval:      DefaultElectrumPingInterval,
		MaxRetries:        DefaultElectrumMaxRetries,
	}
}