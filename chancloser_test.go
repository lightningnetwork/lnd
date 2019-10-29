package lnd

import (
	"math/rand"
	"testing"

	"github.com/lightningnetwork/lnd/lnwire"
)

// randDeliveryAddress generates a random delivery address for testing.
func randDeliveryAddress(t *testing.T) lnwire.DeliveryAddress {
	// Generate an address of maximum length.
	da := lnwire.DeliveryAddress(make([]byte, 34))

	_, err := rand.Read(da)
	if err != nil {
		t.Fatalf("cannot generate random address: %v", err)
	}

	return da
}

// TestCheckRemoteDeliveryAddress tests that the checkRemoteDeliveryAddress
// errors appropriately when shutdown is set and the address provided does not
// match, and does not error in any other case.
func TestCheckRemoteDeliveryAddress(t *testing.T) {
	addr1 := randDeliveryAddress(t)
	addr2 := randDeliveryAddress(t)

	tests := []struct {
		name         string
		shutdownAddr lnwire.DeliveryAddress
		upfrontAddr  lnwire.DeliveryAddress
		expectErr    bool
	}{
		{
			name:         "No shutdown set, address ok",
			shutdownAddr: addr1,
			upfrontAddr:  []byte{},
			expectErr:    false,
		},
		{
			name:         "Shutdown set, address ok",
			shutdownAddr: addr1,
			upfrontAddr:  addr1,
			expectErr:    false,
		},
		{
			name:         "Shutdown set, address not ok",
			shutdownAddr: addr1,
			upfrontAddr:  addr2,
			expectErr:    true,
		},
		{
			// This case is not expected, since shutdown messages must include
			// an address, but it is included for completeness because failure
			// of this function results in disconnection from a peer.
			name:         "No shutdown set, address not ok",
			shutdownAddr: []byte{},
			upfrontAddr:  []byte{},
			expectErr:    false,
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			err := checkRemoteDeliveryAddress(
				test.upfrontAddr, test.shutdownAddr,
			)
			if (err != nil) != (test.expectErr) {
				t.Fatalf("Error: %v, expected error: %v", err, test.expectErr)
			}
		})
	}
}
