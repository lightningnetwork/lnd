//go:build dev
// +build dev

package bitcoind_test

import (
	"testing"

	chainnotiftest "github.com/lightningnetwork/lnd/chainnotif/test"
)

// TestInterfaces executes the generic notifier test suite against a bitcoind
// powered chain notifier.
func TestInterfaces(t *testing.T) {
	t.Run("bitcoind", func(st *testing.T) {
		st.Parallel()
		chainnotiftest.TestInterfaces(st, "bitcoind")
	})

	t.Run("bitcoind rpc polling", func(st *testing.T) {
		st.Parallel()
		chainnotiftest.TestInterfaces(st, "bitcoind-rpc-polling")
	})
}
