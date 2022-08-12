package lnd

import (
	"testing"

	"github.com/lightningnetwork/lnd/lncfg"
)

// TestShouldPeerBootstrap tests that we properly skip network bootstrap for
// the developer networks, and also if bootstrapping is explicitly disabled.
func TestShouldPeerBootstrap(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		cfg            *Config
		shouldBoostrap bool
	}{
		// Simnet active, no bootstrap.
		{
			cfg: &Config{
				Bitcoin: &lncfg.Chain{
					SimNet: true,
				},
				Litecoin: &lncfg.Chain{},
			},
		},

		// Regtest active, no bootstrap.
		{
			cfg: &Config{
				Bitcoin: &lncfg.Chain{
					RegTest: true,
				},
				Litecoin: &lncfg.Chain{},
			},
		},

		// Signet active, no bootstrap.
		{
			cfg: &Config{
				Bitcoin: &lncfg.Chain{
					SigNet: true,
				},
				Litecoin: &lncfg.Chain{},
			},
		},

		// Mainnet active, but bootstrap disabled, no bootstrap.
		{
			cfg: &Config{
				Bitcoin: &lncfg.Chain{
					MainNet: true,
				},
				Litecoin:       &lncfg.Chain{},
				NoNetBootstrap: true,
			},
		},

		// Mainnet active, should bootstrap.
		{
			cfg: &Config{
				Bitcoin: &lncfg.Chain{
					MainNet: true,
				},
				Litecoin: &lncfg.Chain{},
			},
			shouldBoostrap: true,
		},

		// Testnet active, should bootstrap.
		{
			cfg: &Config{
				Bitcoin: &lncfg.Chain{
					TestNet3: true,
				},
				Litecoin: &lncfg.Chain{},
			},
			shouldBoostrap: true,
		},
	}
	for i, testCase := range testCases {
		bootstrapped := shouldPeerBootstrap(testCase.cfg)
		if bootstrapped != testCase.shouldBoostrap {
			t.Fatalf("#%v: expected bootstrap=%v, got bootstrap=%v",
				i, testCase.shouldBoostrap, bootstrapped)
		}
	}
}
