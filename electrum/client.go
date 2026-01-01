package electrum

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/checksum0/go-electrum/electrum"
	"github.com/lightningnetwork/lnd/lncfg"
)

var (
	// ErrClientShutdown is returned when the client has been shut down.
	ErrClientShutdown = errors.New("electrum client has been shut down")

	// ErrNotConnected is returned when an operation is attempted but
	// the client is not connected to the server.
	ErrNotConnected = errors.New("not connected to electrum server")

	// ErrConnectionFailed is returned when unable to establish a
	// connection to the Electrum server.
	ErrConnectionFailed = errors.New("failed to connect to electrum server")
)

// ClientConfig holds the configuration for the Electrum client.
type ClientConfig struct {
	// Server is the host:port of the Electrum server.
	Server string

	// RESTURL is the optional URL for the mempool/electrs REST API.
	// If provided, this will be used to fetch full blocks and other data
	// that the Electrum protocol doesn't support directly.
	RESTURL string

	// UseSSL indicates whether to use SSL/TLS for the connection.
	UseSSL bool

	// TLSCertPath is the optional path to a custom TLS certificate.
	TLSCertPath string

	// TLSSkipVerify skips TLS certificate verification if true.
	TLSSkipVerify bool

	// ReconnectInterval is the time between reconnection attempts.
	ReconnectInterval time.Duration

	// RequestTimeout is the timeout for individual RPC requests.
	RequestTimeout time.Duration

	// PingInterval is the interval for sending ping messages.
	PingInterval time.Duration

	// MaxRetries is the maximum number of retries for failed requests.
	MaxRetries int
}

// NewClientConfigFromLncfg creates a ClientConfig from the lncfg.Electrum
// configuration.
func NewClientConfigFromLncfg(cfg *lncfg.Electrum) *ClientConfig {
	return &ClientConfig{
		Server:            cfg.Server,
		RESTURL:           cfg.RESTURL,
		UseSSL:            cfg.UseSSL,
		TLSCertPath:       cfg.TLSCertPath,
		TLSSkipVerify:     cfg.TLSSkipVerify,
		ReconnectInterval: cfg.ReconnectInterval,
		RequestTimeout:    cfg.RequestTimeout,
		PingInterval:      cfg.PingInterval,
		MaxRetries:        cfg.MaxRetries,
	}
}

// Client is a wrapper around the go-electrum client that provides connection
// management, automatic reconnection, and integration with LND's patterns.
type Client struct {
	cfg *ClientConfig

	// client is the underlying electrum client from the go-electrum
	// library. Access must be synchronized via clientMu.
	client   *electrum.Client
	clientMu sync.RWMutex

	// connected indicates whether the client is currently connected.
	connected atomic.Bool

	// started indicates whether the client has been started.
	started atomic.Bool

	// protocolVersion stores the negotiated protocol version.
	protocolVersion string

	// serverVersion stores the server's software version string.
	serverVersion string

	wg   sync.WaitGroup
	quit chan struct{}
}

// NewClient creates a new Electrum client with the given configuration.
func NewClient(cfg *ClientConfig) *Client {
	return &Client{
		cfg:  cfg,
		quit: make(chan struct{}),
	}
}

// Start initializes the client and establishes a connection to the Electrum
// server. It also starts background goroutines for connection management.
func (c *Client) Start() error {
	if c.started.Swap(true) {
		return nil
	}

	log.Infof("Starting Electrum client, server=%s, ssl=%v",
		c.cfg.Server, c.cfg.UseSSL)

	// Attempt initial connection.
	if err := c.connect(); err != nil {
		log.Warnf("Initial connection to Electrum server failed: %v",
			err)

		// Start reconnection loop in background rather than failing
		// immediately. This allows LND to start even if the Electrum
		// server is temporarily unavailable.
	}

	// Start the connection manager goroutine.
	c.wg.Add(1)
	go c.connectionManager()

	return nil
}

// Stop shuts down the client and closes the connection to the Electrum server.
func (c *Client) Stop() error {
	if !c.started.Load() {
		return nil
	}

	log.Info("Stopping Electrum client")

	close(c.quit)
	c.wg.Wait()

	c.disconnect()

	return nil
}

// connect establishes a connection to the Electrum server.
func (c *Client) connect() error {
	c.clientMu.Lock()
	defer c.clientMu.Unlock()

	// Close any existing connection.
	if c.client != nil {
		c.client.Shutdown()
		c.client = nil
	}

	ctx, cancel := context.WithTimeout(
		context.Background(), c.cfg.RequestTimeout,
	)
	defer cancel()

	var (
		client *electrum.Client
		err    error
	)

	if c.cfg.UseSSL {
		client, err = c.connectSSL(ctx)
	} else {
		client, err = electrum.NewClientTCP(ctx, c.cfg.Server)
	}

	if err != nil {
		return fmt.Errorf("%w: %v", ErrConnectionFailed, err)
	}

	// Negotiate protocol version with the server.
	serverVer, protoVer, err := client.ServerVersion(ctx)
	if err != nil {
		client.Shutdown()
		return fmt.Errorf("failed to negotiate protocol version: %w",
			err)
	}

	c.client = client
	c.serverVersion = serverVer
	c.protocolVersion = protoVer
	c.connected.Store(true)

	log.Infof("Connected to Electrum server: version=%s, protocol=%s",
		serverVer, protoVer)

	return nil
}

// connectSSL establishes an SSL/TLS connection to the Electrum server.
func (c *Client) connectSSL(ctx context.Context) (*electrum.Client, error) {
	tlsConfig := &tls.Config{
		InsecureSkipVerify: c.cfg.TLSSkipVerify, //nolint:gosec
	}

	// Load custom certificate if specified.
	if c.cfg.TLSCertPath != "" {
		certPEM, err := os.ReadFile(c.cfg.TLSCertPath)
		if err != nil {
			return nil, fmt.Errorf("failed to read TLS cert: %w",
				err)
		}

		certPool := x509.NewCertPool()
		if !certPool.AppendCertsFromPEM(certPEM) {
			return nil, errors.New("failed to parse TLS certificate")
		}

		tlsConfig.RootCAs = certPool
	}

	return electrum.NewClientSSL(ctx, c.cfg.Server, tlsConfig)
}

// disconnect closes the connection to the Electrum server.
func (c *Client) disconnect() {
	c.clientMu.Lock()
	defer c.clientMu.Unlock()

	if c.client != nil {
		c.client.Shutdown()
		c.client = nil
	}

	c.connected.Store(false)
}

// connectionManager handles automatic reconnection and keep-alive pings.
func (c *Client) connectionManager() {
	defer c.wg.Done()

	reconnectTicker := time.NewTicker(c.cfg.ReconnectInterval)
	defer reconnectTicker.Stop()

	pingTicker := time.NewTicker(c.cfg.PingInterval)
	defer pingTicker.Stop()

	for {
		select {
		case <-c.quit:
			return

		case <-reconnectTicker.C:
			if !c.connected.Load() {
				log.Debug("Attempting to reconnect to " +
					"Electrum server")

				if err := c.connect(); err != nil {
					log.Warnf("Reconnection failed: %v", err)
				}
			}

		case <-pingTicker.C:
			if c.connected.Load() {
				if err := c.ping(); err != nil {
					log.Warnf("Ping failed, marking "+
						"disconnected: %v", err)
					c.connected.Store(false)
				}
			}
		}
	}
}

// ping sends a ping to the server to keep the connection alive.
func (c *Client) ping() error {
	c.clientMu.RLock()
	client := c.client
	c.clientMu.RUnlock()

	if client == nil {
		return ErrNotConnected
	}

	ctx, cancel := context.WithTimeout(
		context.Background(), c.cfg.RequestTimeout,
	)
	defer cancel()

	return client.Ping(ctx)
}

// IsConnected returns true if the client is currently connected.
func (c *Client) IsConnected() bool {
	return c.connected.Load()
}

// ServerVersion returns the server's software version string.
func (c *Client) ServerVersion() string {
	return c.serverVersion
}

// ProtocolVersion returns the negotiated protocol version.
func (c *Client) ProtocolVersion() string {
	return c.protocolVersion
}

// getClient returns the underlying client with proper locking. Returns an
// error if not connected.
func (c *Client) getClient() (*electrum.Client, error) {
	c.clientMu.RLock()
	defer c.clientMu.RUnlock()

	if c.client == nil || !c.connected.Load() {
		return nil, ErrNotConnected
	}

	return c.client, nil
}

// withRetry executes the given function with retry logic.
func (c *Client) withRetry(ctx context.Context,
	fn func(context.Context, *electrum.Client) error) error {

	var lastErr error

	for i := 0; i <= c.cfg.MaxRetries; i++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.quit:
			return ErrClientShutdown
		default:
		}

		client, err := c.getClient()
		if err != nil {
			lastErr = err

			// Wait before retrying if not connected.
			select {
			case <-time.After(c.cfg.ReconnectInterval):
			case <-ctx.Done():
				return ctx.Err()
			case <-c.quit:
				return ErrClientShutdown
			}

			continue
		}

		reqCtx, cancel := context.WithTimeout(ctx, c.cfg.RequestTimeout)
		err = fn(reqCtx, client)
		cancel()

		if err == nil {
			return nil
		}

		lastErr = err
		log.Debugf("Request failed (attempt %d/%d): %v",
			i+1, c.cfg.MaxRetries+1, err)

		// Check if this looks like a connection error.
		if isConnectionError(err) {
			c.connected.Store(false)
		}
	}

	return fmt.Errorf("request failed after %d attempts: %w",
		c.cfg.MaxRetries+1, lastErr)
}

// isConnectionError checks if the error indicates a connection problem.
func isConnectionError(err error) bool {
	if err == nil {
		return false
	}

	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}

	// Check for the electrum library's shutdown error.
	if errors.Is(err, electrum.ErrServerShutdown) {
		return true
	}

	// Check for common connection-related error messages.
	errStr := err.Error()
	return errors.Is(err, net.ErrClosed) ||
		strings.Contains(errStr, "connection refused") ||
		strings.Contains(errStr, "connection reset") ||
		strings.Contains(errStr, "broken pipe") ||
		strings.Contains(errStr, "EOF")
}
