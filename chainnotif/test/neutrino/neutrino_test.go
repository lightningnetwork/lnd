//go:build dev
// +build dev

package neutrino_test

import (
	"testing"

	chainnotiftest "github.com/lightningnetwork/lnd/chainnotif/test"
)

// TestInterfaces executes the generic notifier test suite against a neutrino
// powered chain notifier.
func TestInterfaces(t *testing.T) {
	chainnotiftest.TestInterfaces(t, "neutrino")
}
