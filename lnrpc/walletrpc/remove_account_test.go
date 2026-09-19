//go:build walletrpc
// +build walletrpc

package walletrpc

import (
	"testing"

	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/stretchr/testify/require"
)

// TestKeyScopeFromAddrType asserts the mapping from the RPC address types onto
// the key scopes accounts live in. RemoveAccount and ListAccounts both resolve
// accounts through this mapping, so a wrong entry would make a caller operate
// on a different scope than the one they named.
func TestKeyScopeFromAddrType(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		addrType      AddressType
		expectedScope *waddrmgr.KeyScope
		expectedErr   string
	}{{
		name:          "unknown means no scope filter",
		addrType:      AddressType_UNKNOWN,
		expectedScope: nil,
	}, {
		name:          "witness pubkey hash",
		addrType:      AddressType_WITNESS_PUBKEY_HASH,
		expectedScope: &waddrmgr.KeyScopeBIP0084,
	}, {
		name:          "nested witness pubkey hash",
		addrType:      AddressType_NESTED_WITNESS_PUBKEY_HASH,
		expectedScope: &waddrmgr.KeyScopeBIP0049Plus,
	}, {
		name:          "hybrid nested witness pubkey hash",
		addrType:      AddressType_HYBRID_NESTED_WITNESS_PUBKEY_HASH,
		expectedScope: &waddrmgr.KeyScopeBIP0049Plus,
	}, {
		name:          "taproot pubkey",
		addrType:      AddressType_TAPROOT_PUBKEY,
		expectedScope: &waddrmgr.KeyScopeBIP0086,
	}, {
		name:        "unhandled type rejected",
		addrType:    AddressType(999),
		expectedErr: "unhandled address type",
	}}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			scope, err := keyScopeFromAddrType(test.addrType)

			if test.expectedErr != "" {
				require.ErrorContains(t, err, test.expectedErr)
				return
			}

			require.NoError(t, err)
			if test.expectedScope == nil {
				require.Nil(t, scope)
				return
			}

			require.NotNil(t, scope)
			require.Equal(t, *test.expectedScope, *scope)
		})
	}
}
