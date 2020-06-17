package chancloser

import (
	"crypto/rand"
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

// TestMaybeMatchScript tests that the maybeMatchScript errors appropriately
// when an upfront shutdown script is set and the script provided does not
// match, and does not error in any other case.
func TestMaybeMatchScript(t *testing.T) {
	addr1 := randDeliveryAddress(t)
	addr2 := randDeliveryAddress(t)

	tests := []struct {
		name           string
		shutdownScript lnwire.DeliveryAddress
		upfrontScript  lnwire.DeliveryAddress
		expectedErr    error
	}{
		{
			name:           "no upfront shutdown set, script ok",
			shutdownScript: addr1,
			upfrontScript:  []byte{},
			expectedErr:    nil,
		},
		{
			name:           "upfront shutdown set, script ok",
			shutdownScript: addr1,
			upfrontScript:  addr1,
			expectedErr:    nil,
		},
		{
			name:           "upfront shutdown set, script not ok",
			shutdownScript: addr1,
			upfrontScript:  addr2,
			expectedErr:    ErrUpfrontShutdownScriptMismatch,
		},
		{
			name:           "nil shutdown and empty upfront",
			shutdownScript: nil,
			upfrontScript:  []byte{},
			expectedErr:    nil,
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			err := maybeMatchScript(
				func() error { return nil }, test.upfrontScript,
				test.shutdownScript,
			)

			if err != test.expectedErr {
				t.Fatalf("Error: %v, expected error: %v", err, test.expectedErr)
			}
		})
	}
}
