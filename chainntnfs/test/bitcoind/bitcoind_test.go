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

	success = t.Run("bitcoind rpc polling", func(st *testing.T) {
		st.Parallel()
		chainntnfstest.TestInterfaces(st, "bitcoind-rpc-polling")
	})

	if !success {
		return
	}

	// Run the suite against a bitcoind backend without a transaction
	// index, which forces the notifier through its manual historical
	// confirmation and spend lookup fallbacks.
	t.Run("bitcoind no txindex", func(st *testing.T) {
		st.Parallel()
		chainntnfstest.TestInterfaces(st, "bitcoind-no-txindex")
	})
}
