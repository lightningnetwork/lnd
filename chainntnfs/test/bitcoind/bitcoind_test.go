//go:build dev
// +build dev

package bitcoind_test

import (
	"testing"

	chainntnfstest "github.com/lightningnetwork/lnd/chainntnfs/test"
)

// TestInterfaces executes the generic notifier test suite against a bitcoind
// powered chain notifier.
func TestInterfaces(t *testing.T) {
	success := t.Run("bitcoind", func(st *testing.T) {
		st.Parallel()
		chainntnfstest.TestInterfaces(st, "bitcoind")
	})

	if !success {
		return
	}

	t.Run("bitcoind rpc polling", func(st *testing.T) {
		st.Parallel()
		chainntnfstest.TestInterfaces(st, "bitcoind-rpc-polling")
	})
}
