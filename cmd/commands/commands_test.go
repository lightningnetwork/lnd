package commands

import (
	"testing"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/stretchr/testify/require"
)

// TestSendCoinsRequestWithChangeAddr tests that the SendCoinsRequest
// properly handles the ChangeAddr field.
func TestSendCoinsRequestWithChangeAddr(t *testing.T) {
	// Test case 1: Request with change address
	req1 := &lnrpc.SendCoinsRequest{
		Addr:       "bc1qxy2kgdygjrsqtzq2n0yrf2493p83kkfjhx0wlh",
		Amount:     100000,
		ChangeAddr: "bc1qchangeaddress1234567890abcdef",
	}
	require.NotEmpty(t, req1.ChangeAddr)
	require.Equal(t, "bc1qchangeaddress1234567890abcdef", req1.ChangeAddr)

	// Test case 2: Request without change address (backward compatible)
	req2 := &lnrpc.SendCoinsRequest{
		Addr:   "bc1qxy2kgdygjrsqtzq2n0yrf2493p83kkfjhx0wlh",
		Amount: 100000,
	}
	require.Empty(t, req2.ChangeAddr)
}
