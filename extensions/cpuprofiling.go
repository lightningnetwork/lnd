// Copyright (c) 2013-2017 The btcsuite developers
// Copyright (c) 2015-2016 The Decred developers
// Copyright (C) 2015-2018 The Lightning Network Developers

package extensions

import (
	"fmt"
	"os"
	"runtime/pprof"

	"github.com/lightningnetwork/lnd/config"
	"github.com/lightningnetwork/lnd/extpoints"
)

func init() {
	extpoints.RegisterExtension(new(CPUProfiling), "cpuprofiling")
}

// createError wraps any error to provide a descriptive failure message
type createError struct {
	cause error
}

func (e createError) Error() string {
	return fmt.Sprintf("Unable to create cpu profile: %v", e.cause)
}

// CPUProfiling is an extension that sets up CPU profiling for LND
type CPUProfiling struct{}

// StartLnd uses pprof to write the CPU profile if requested
func (p *CPUProfiling) StartLnd(cfg *config.Config) error {
	if cfg.CPUProfile != "" {
		f, err := os.Create(cfg.CPUProfile)
		if err != nil {
			return createError{err}
		}
		pprof.StartCPUProfile(f)
		defer f.Close()
		defer pprof.StopCPUProfile()
	}
	return nil
}
