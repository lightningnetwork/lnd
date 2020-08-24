package lncfg

import (
	"fmt"
	"time"
)

var (
	// MinHealthCheckInterval is the minimum interval we allow between
	// health checks.
	MinHealthCheckInterval = time.Minute

	// MinHealthCheckTimeout is the minimum timeout we allow for health
	// check calls.
	MinHealthCheckTimeout = time.Second

	// MinHealthCheckBackoff is the minimum back off we allow between health
	// check retries.
	MinHealthCheckBackoff = time.Second
)

// HealthCheckConfig contains the configuration for the different health checks
// the lnd runs.
type HealthCheckConfig struct {
	ChainCheck *CheckConfig `group:"chainbackend" namespace:"chainbackend"`
}

// Validate checks the values configured for our health checks.
func (h *HealthCheckConfig) Validate() error {
	return h.ChainCheck.validate("chain backend")
}

type CheckConfig struct {
	Interval time.Duration `long:"interval" description:"How often to run a health check."`

	Attempts int `long:"attempts" description:"The number of calls we will make for the check before failing. Set this value to 0 to disable a check."`

	Timeout time.Duration `long:"timeout" description:"The amount of time we allow the health check to take before failing due to timeout."`

	Backoff time.Duration `long:"backoff" description:"The amount of time to back-off between failed health checks."`
}

// validate checks the values in a health check config entry if it is enabled.
func (c *CheckConfig) validate(name string) error {
	if c.Attempts == 0 {
		return nil
	}

	if c.Backoff < MinHealthCheckBackoff {
		return fmt.Errorf("%v backoff: %v below minimum: %v", name,
			c.Backoff, MinHealthCheckBackoff)
	}

	if c.Timeout < MinHealthCheckTimeout {
		return fmt.Errorf("%v timeout: %v below minimum: %v", name,
			c.Timeout, MinHealthCheckTimeout)
	}

	if c.Interval < MinHealthCheckInterval {
		return fmt.Errorf("%v interval: %v below minimum: %v", name,
			c.Interval, MinHealthCheckInterval)
	}

	return nil
}
