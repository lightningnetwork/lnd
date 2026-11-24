//go:build test_db_postgres || test_db_sqlite || test_native_sql

package lnd

import (
	"testing"

	"github.com/lightningnetwork/lnd/chainreg"
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/stretchr/testify/require"
)

// TestValidateBolt12OffersNativeSQL verifies that builds with the development
// migrations accept BOLT 12 offers when native SQL is enabled.
func TestValidateBolt12OffersNativeSQL(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		ProtocolOptions: &lncfg.ProtocolOptions{},
		DB:              &lncfg.DB{UseNativeSQL: true},
		Bitcoin: &lncfg.Chain{
			TimeLockDelta: chainreg.DefaultBitcoinTimeLockDelta,
		},
	}
	require.NoError(t, validateBolt12Offers(cfg))
}
