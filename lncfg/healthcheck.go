package lncfg

import (
	"errors"
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
//
//nolint:ll
type HealthCheckConfig struct {
	ChainCheck *CheckConfig `group:"chainbackend" namespace:"chainbackend"`

	DiskCheck *DiskCheckConfig `group:"diskspace" namespace:"diskspace"`

	TLSCheck *CheckConfig `group:"tls" namespace:"tls"`

	TorConnection *CheckConfig `group:"torconnection" namespace:"torconnection"`

	RemoteSigner *CheckConfig `group:"remotesigner" namespace:"remotesigner"`

	LeaderCheck *CheckConfig `group:"leader" namespace:"leader"`
}

// Validate checks the values configured for our health checks.
func (h *HealthCheckConfig) Validate() error {
	if err := h.ChainCheck.validate("chain backend"); err != nil {
		return err
	}

	if err := h.DiskCheck.validate("disk space"); err != nil {
		return err
	}

	if err := h.TLSCheck.validate("tls"); err != nil {
		return err
	}

	if h.DiskCheck.RequiredRemaining < 0 ||
		h.DiskCheck.RequiredRemaining >= 1 {

		return errors.New("disk required ratio must be in [0:1)")
	}

	if err := h.TorConnection.validate("tor connection"); err != nil {
		return err
	}

	return nil
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

// DiskCheckConfig contains configuration for ensuring that our node has
// sufficient disk space.
type DiskCheckConfig struct {
	RequiredRemaining float64 `long:"diskrequired" description:"The minimum ratio of free disk space to total capacity that we allow before shutting lnd down safely."`

	*CheckConfig
}
