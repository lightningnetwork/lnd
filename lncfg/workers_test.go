package lncfg_test

import (
	"testing"

	"github.com/lightningnetwork/lnd/lncfg"
)

const (
	maxUint = ^uint(0)
	maxInt  = int(maxUint >> 1)
	minInt  = -maxInt - 1
)

// TestValidateWorkers asserts that validating the Workers config only succeeds
// if all fields specify a positive number of workers.
func TestValidateWorkers(t *testing.T) {
	tests := []struct {
		name  string
		cfg   *lncfg.Workers
		valid bool
	}{
		{
			name: "min valid",
			cfg: &lncfg.Workers{
				Read:  1,
				Write: 1,
				Sig:   1,
			},
			valid: true,
		},
		{
			name: "max valid",
			cfg: &lncfg.Workers{
				Read:  maxInt,
				Write: maxInt,
				Sig:   maxInt,
			},
			valid: true,
		},
		{
			name: "read max invalid",
			cfg: &lncfg.Workers{
				Read:  0,
				Write: 1,
				Sig:   1,
			},
		},
		{
			name: "write max invalid",
			cfg: &lncfg.Workers{
				Read:  1,
				Write: 0,
				Sig:   1,
			},
		},
		{
			name: "sig max invalid",
			cfg: &lncfg.Workers{
				Read:  1,
				Write: 1,
				Sig:   0,
			},
		},
		{
			name: "read min invalid",
			cfg: &lncfg.Workers{
				Read:  minInt,
				Write: 1,
				Sig:   1,
			},
		},
		{
			name: "write min invalid",
			cfg: &lncfg.Workers{
				Read:  1,
				Write: minInt,
				Sig:   1,
			},
		},
		{
			name: "sig min invalid",
			cfg: &lncfg.Workers{
				Read:  1,
				Write: 1,
				Sig:   minInt,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.cfg.Validate()
			switch {
			case test.valid && err != nil:
				t.Fatalf("valid config was invalid: %v", err)
			case !test.valid && err == nil:
				t.Fatalf("invalid config was valid")
			}
		})
	}
}
