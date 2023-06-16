package watchtower

import (
	"strconv"
	"time"
)

// Conf specifies the watchtower options that can be configured from the command
// line or configuration file.
type Conf struct {
	// RawListeners configures the watchtower's listening ports/interfaces.
	RawListeners []string `long:"listen" description:"Add interfaces/ports to listen for peer connections"`

	// RawExternalIPs configures the watchtower's external ports/interfaces.
	RawExternalIPs []string `long:"externalip" description:"Add interfaces/ports where the watchtower can accept peer connections"`

	// ReadTimeout specifies the duration the tower will wait when trying to
	// read a message from a client before hanging up.
	ReadTimeout time.Duration `long:"readtimeout" description:"Duration the watchtower server will wait for messages to be received before hanging up on clients"`

	// WriteTimeout specifies the duration the tower will wait when trying
	// to write a message from a client before hanging up.
	WriteTimeout time.Duration `long:"writetimeout" description:"Duration the watchtower server will wait for messages to be written before hanging up on client connections"`
}

// DefaultConf returns a Conf with some default values filled in.
func DefaultConf() *Conf {
	return &Conf{
		ReadTimeout:  DefaultReadTimeout,
		WriteTimeout: DefaultWriteTimeout,
	}
}

// Apply completes the passed Config struct by applying any parsed Conf options.
// If the corresponding values parsed by Conf are already set in the Config,
// those fields will be not be modified.
func (c *Conf) Apply(cfg *Config,
	normalizer AddressNormalizer) (*Config, error) {

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
			addr := DefaultListenAddr
			c.RawListeners = append(c.RawListeners, addr)
		}

		// Normalize the raw listening addresses so that they can be
		// used by the brontide listener.
		var err error
		cfg.ListenAddrs, err = normalizer(
			c.RawListeners, strconv.Itoa(DefaultPeerPort),
			cfg.Net.ResolveTCPAddr,
		)
		if err != nil {
			return nil, err
		}
	}

	// Set the Config's external ips if they are empty.
	if cfg.ExternalIPs == nil {
		// Without a network, we will be unable to resolve the external
		// IP addresses.
		if cfg.Net == nil {
			return nil, ErrNoNetwork
		}

		var err error
		cfg.ExternalIPs, err = normalizer(
			c.RawExternalIPs, strconv.Itoa(DefaultPeerPort),
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
