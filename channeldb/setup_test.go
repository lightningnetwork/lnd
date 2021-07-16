package channeldb

import (
	"fmt"
	"os"
	"testing"

	"github.com/lightningnetwork/lnd/kvdb"
)

func TestMain(m *testing.M) {
	err := kvdb.SetupTestBackend()
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}

	// os.Exit() does not respect defer statements
	code := m.Run()
	kvdb.TearDownTestBackend()

	os.Exit(code)
}
