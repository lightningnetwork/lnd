//go:build walletrpc
// +build walletrpc

package commands

import (
	"bytes"
	"testing"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/stretchr/testify/require"
)

// TestMarshallLocksNoExpiry verifies that the JSON lease output reports the
// no-expiry policy.
func TestMarshallLocksNoExpiry(t *testing.T) {
	t.Parallel()

	leases := []*walletrpc.UtxoLease{
		{
			Id: bytes.Repeat([]byte{0x03}, 32),
			Outpoint: &lnrpc.OutPoint{
				TxidBytes: make([]byte, 32),
			},
			Expiration: 100,
			NoExpiry:   true,
		},
		{
			Id: bytes.Repeat([]byte{0x04}, 32),
			Outpoint: &lnrpc.OutPoint{
				TxidBytes: make([]byte, 32),
			},
			Expiration: 200,
		},
	}

	jsonLocks := marshallLocks(leases)
	require.Len(t, jsonLocks, 2)
	require.True(t, jsonLocks[0].NoExpiry)
	require.False(t, jsonLocks[1].NoExpiry)
}
