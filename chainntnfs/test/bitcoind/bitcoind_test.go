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
	chainntnfstest.TestInterfaces(t, "bitcoind")
}
