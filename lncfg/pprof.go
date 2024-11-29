package lncfg

import (
	"net"
	"strconv"
)

// Pprof holds the configuration options for LND's built-in pprof server.
//
//nolint:ll
type Pprof struct {
	CPUProfile string `long:"cpuprofile" description:"Write CPU profile to the specified file"`

	Profile string `long:"profile" description:"Enable HTTP profiling on either a port or host:port"`

	BlockingProfile int `long:"blockingprofile" description:"Used to enable a blocking profile to be served on the profiling port. This takes a value from 0 to 1, with 1 including every blocking event, and 0 including no events."`

	MutexProfile int `long:"mutexprofile" description:"Used to Enable a mutex profile to be served on the profiling port. This takes a value from 0 to 1, with 1 including every mutex event, and 0 including no events."`
}

// Validate checks the values configured for the profiler.
func (p *Pprof) Validate() error {
	if p.BlockingProfile > 0 {
		log.Warn("Blocking profile enabled only useful for " +
			"debugging because of significant performance impact")
	}

	if p.MutexProfile > 0 {
		log.Warn("Mutex profile enabled only useful for " +
			"debugging because of significant performance impact")
	}

	if p.CPUProfile != "" {
		log.Warn("CPU profile enabled only useful for " +
			"debugging because of significant performance impact")
	}

	if p.Profile != "" {
		str := "%v: The profile port must be between 1024 and 65535"

		// Try to parse Profile as a host:port.
		_, hostPort, err := net.SplitHostPort(p.Profile)
		if err == nil {
			// Determine if the port is valid.
			profilePort, err := strconv.Atoi(hostPort)

			if err != nil || profilePort < 1024 ||
				profilePort > 65535 {

				return &UsageError{Err: mkErr(str, hostPort)}
			}
		} else {
			// Try to parse Profile as a port.
			profilePort, err := strconv.Atoi(p.Profile)
			if err != nil || profilePort < 1024 ||
				profilePort > 65535 {

				return &UsageError{Err: mkErr(str, p.Profile)}
			}

			// Since the user just set a port, we will serve
			// debugging information over localhost.
			p.Profile = net.JoinHostPort("127.0.0.1", p.Profile)
		}
	}

	return nil
}
