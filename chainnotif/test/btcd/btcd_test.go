//go:build dev
// +build dev

package btcd_test

import (
	"testing"

	chainnotiftest "github.com/lightningnetwork/lnd/chainnotif/test"
)

// TestInterfaces executes the generic notifier test suite against a btcd
// powered chain notifier.
func TestInterfaces(t *testing.T) {
	chainnotiftest.TestInterfaces(t, "btcd")
}
