package main

import (
	"fmt"
	"sync"
	"time"

	"github.com/lightningnetwork/lnd/channeldb"
)

// coinControl couples all interfaces necessary to handle a new coin
type coinControl struct {
	DefaultConfig func(*config) error
	RegisterChain func(*config, string) (*config, error)
	IsTestNet func(*bitcoinNetParams) bool
	ChainControlFromConfig func(cfg *config, chanDB *channeldb.DB,
		privateWalletPw, publicWalletPw []byte, birthday time.Time,
		recoveryWindow uint32) (*chainControl, func(), error)
}

// coinRegistry keeps track of other than Bitcoin coins (eg: litecoin)
type coinRegistry struct {
	sync.RWMutex

	coins map[chainCode]*coinControl
}

// newCoinRegistry creates a new coin registry.
func newCoinRegistry() *coinRegistry {
	return &coinRegistry{}
}

// RegisterCoin registers a new coin in the registry.
func (c *coinRegistry) RegisterCoin(newCoin chainCode, cc *coinControl) error {
	c.Lock()
	defer c.Unlock()

	if c.Any() {
		return fmt.Errorf("only one registered coin possible")
	}

	c.coins[newCoin] = cc
	return nil
}

// Any returns true if there is any coin registered
func (c *coinRegistry) Any() bool {
	c.RLock()
	defer c.RUnlock()

	return (c.NumCoins() > 0)
}

// None returns true if there are no coins registered
func (c *coinRegistry) None() bool {
	c.RLock()
	defer c.RUnlock()

	return (c.NumCoins() == 0)
}

// IsTestNet returns true if the coin is on the test net
func (c *coinRegistry) IsTestNet(params *bitcoinNetParams) bool {
	c.RLock()
	defer c.RUnlock()

	for _, control := range c.coins {
		// XXX(maurycy): just return, we do not support many coins atm
		return control.IsTestNet(params)
	}

	return false
}

// LookupCoin attempts to lookup a coinControl instance for the target chain.
func (c *coinRegistry) LookupCoin(targetCoin chainCode) (*coinControl, bool) {
	c.RLock()
	defer c.RUnlock()

	cc, ok := c.coins[targetCoin]
	return cc, ok
}

// ChainControlFromConfig returns a chainControl for registered coins
func (c *coinRegistry) ChainControlFromConfig(cfg *config,
	chanDB *channeldb.DB, privateWalletPw, publicWalletPw []byte,
	birthday time.Time, recoveryWindow uint32) (*chainControl, func(), error) {

	c.Lock()
	defer c.Unlock()

	for _, control := range c.coins {
		cc, cleanup, err := control.ChainControlFromConfig(cfg, chanDB,
			privateWalletPw, publicWalletPw, birthday, recoveryWindow)

		// XXX(maurycy): just return, we do not support many coins atm
		return cc, cleanup, err
	}

	return nil, nil, nil
}

// DefaultConfig runs the defaultConfig over all coins.
func (c *coinRegistry) DefaultConfig(cfg *config) error {
	c.Lock()
	defer c.Unlock()

	for _, control := range c.coins {
		if err := control.DefaultConfig(cfg); err != nil {
			return err
		}
	}

	return nil
}

// NumActiveChains returns the total number of coins.
func (c *coinRegistry) NumCoins() uint32 {
	c.RLock()
	defer c.RUnlock()

	return uint32(len(c.coins))
}
