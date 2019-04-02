// +build experimental

package watchtower

import (
	"time"

	"github.com/lightningnetwork/lnd/lncfg"
)

// Conf specifies the watchtower options that can be configured from the command
// line or configuration file.
type Conf struct {
	RawListeners []string `long:"listen" description:"Add interfaces/ports to listen for peer connections"`

	ReadTimeout time.Duration `long:"readtimeout" description:"Duration the watchtower server will wait for messages to be received before hanging up on clients"`

	WriteTimeout time.Duration `long:"writetimeout" description:"Duration the watchtower server will wait for messages to be written before hanging up on client connections"`
}

// Apply completes the passed Config struct by applying any parsed Conf options.
// If the corresponding values parsed by Conf are already set in the Config,
// those fields will be not be modified.
func (c *Conf) Apply(cfg *Config) (*Config, error) {
	// Set the Config's listening addresses if they are empty.
	if cfg.ListenAddrs == nil {
		// Without a network, we will be unable to resolve the listening
		// addresses.
		if cfg.Net == nil {
			return nil, ErrNoNetwork
		}

		// If no addresses are specified by the Config, we will resort
		// to the default peer port.
		if len(c.RawListeners) == 0 {
			addr := DefaultPeerPortStr
			c.RawListeners = append(c.RawListeners, addr)
		}

		// Normalize the raw listening addresses so that they can be
		// used by the brontide listener.
		var err error
		cfg.ListenAddrs, err = lncfg.NormalizeAddresses(
			c.RawListeners, DefaultPeerPortStr,
			cfg.Net.ResolveTCPAddr,
		)
		if err != nil {
			return nil, err
		}
	}

	// If the Config has no read timeout, we will use the parsed Conf
	// value.
	if cfg.ReadTimeout == 0 && c.ReadTimeout != 0 {
		cfg.ReadTimeout = c.ReadTimeout
	}

	// If the Config has no write timeout, we will use the parsed Conf
	// value.
	if cfg.WriteTimeout == 0 && c.WriteTimeout != 0 {
		cfg.WriteTimeout = c.WriteTimeout
	}

	return cfg, nil
}
