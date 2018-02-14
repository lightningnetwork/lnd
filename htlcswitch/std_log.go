// +build stdlog

package htlcswitch

import (
	"os"

	"github.com/btcsuite/btclog"
)

// When compiling with "stdlog" build flag, we will init the package with its
// own logger that writes to stdout.
func init() {
	stdBackend := btclog.NewBackend(os.Stdout)
	stdLogger := stdBackend.Logger("HSWC")

	level, _ := btclog.LevelFromString("trace")
	stdLogger.SetLevel(level)

	UseLogger(stdLogger)
}
