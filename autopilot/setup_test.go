package autopilot

import (
	"testing"

	"github.com/lightningnetwork/lnd/kvdb"
)

func TestMain(m *testing.M) {
	kvdb.RunTests(m)
}
