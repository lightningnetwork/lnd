package lnwire

import (
	"bytes"
	"testing"
)

// TestTypedDeliveryAddressEncodeDecode tests that we're able to properly
// encode and decode typed delivery addresses.
func TestTypedDeliveryAddressEncodeDecode(t *testing.T) {
	t.Parallel()

	addr := TypedDeliveryAddress(
		bytes.Repeat([]byte("a"), deliveryAddressMaxSize),
	)

	var b bytes.Buffer
	if err := addr.Encode(&b); err != nil {
		t.Fatalf("unable to encode addr: %v", err)
	}

	var addr2 TypedDeliveryAddress
	if err := addr2.Decode(&b); err != nil {
		t.Fatalf("unable to decode addr: %v", err)
	}

	if !bytes.Equal(addr, addr2) {
		t.Fatalf("addr mismatch: expected %x, got %x", addr[:],
			addr2[:])
	}
}
