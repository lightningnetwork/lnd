package tlv_test

import (
	"fmt"
	"testing"

	"github.com/lightningnetwork/lnd/tlv"
)

var tuint16Tests = []struct {
	value uint16
	size  uint64
}{
	{
		value: 0x0000,
		size:  0,
	},
	{
		value: 0x0001,
		size:  1,
	},
	{
		value: 0x00ff,
		size:  1,
	},
	{
		value: 0x0100,
		size:  2,
	},
	{
		value: 0xffff,
		size:  2,
	},
}

// TestSizeTUint16 asserts that SizeTUint16 computes the proper truncated size
// along boundary conditions of the input space.
func TestSizeTUint16(t *testing.T) {
	for _, test := range tuint16Tests {
		name := fmt.Sprintf("0x%x", test.value)
		t.Run(name, func(t *testing.T) {
			size := tlv.SizeTUint16(test.value)
			if test.size != size {
				t.Fatalf("size mismatch, expected: %d got: %d",
					test.size, size)
			}
		})
	}
}

var tuint32Tests = []struct {
	value uint32
	size  uint64
}{
	{
		value: 0x00000000,
		size:  0,
	},
	{
		value: 0x00000001,
		size:  1,
	},
	{
		value: 0x000000ff,
		size:  1,
	},
	{
		value: 0x00000100,
		size:  2,
	},
	{
		value: 0x0000ffff,
		size:  2,
	},
	{
		value: 0x00010000,
		size:  3,
	},
	{
		value: 0x00ffffff,
		size:  3,
	},
	{
		value: 0x01000000,
		size:  4,
	},
	{
		value: 0xffffffff,
		size:  4,
	},
}

// TestSizeTUint32 asserts that SizeTUint32 computes the proper truncated size
// along boundary conditions of the input space.
func TestSizeTUint32(t *testing.T) {
	for _, test := range tuint32Tests {
		name := fmt.Sprintf("0x%x", test.value)
		t.Run(name, func(t *testing.T) {
			size := tlv.SizeTUint32(test.value)
			if test.size != size {
				t.Fatalf("size mismatch, expected: %d got: %d",
					test.size, size)
			}
		})
	}
}

var tuint64Tests = []struct {
	value uint64
	size  uint64
}{
	{
		value: 0x0000000000000000,
		size:  0,
	},
	{
		value: 0x0000000000000001,
		size:  1,
	},
	{
		value: 0x00000000000000ff,
		size:  1,
	},
	{
		value: 0x0000000000000100,
		size:  2,
	},
	{
		value: 0x000000000000ffff,
		size:  2,
	},
	{
		value: 0x0000000000010000,
		size:  3,
	},
	{
		value: 0x0000000000ffffff,
		size:  3,
	},
	{
		value: 0x0000000001000000,
		size:  4,
	},
	{
		value: 0x00000000ffffffff,
		size:  4,
	},
	{
		value: 0x0000000100000000,
		size:  5,
	},
	{
		value: 0x000000ffffffffff,
		size:  5,
	},
	{
		value: 0x0000010000000000,
		size:  6,
	},
	{
		value: 0x0000ffffffffffff,
		size:  6,
	},
	{
		value: 0x0001000000000000,
		size:  7,
	},
	{
		value: 0x00ffffffffffffff,
		size:  7,
	},
	{
		value: 0x0100000000000000,
		size:  8,
	},
	{
		value: 0xffffffffffffffff,
		size:  8,
	},
}

// TestSizeTUint64 asserts that SizeTUint64 computes the proper truncated size
// along boundary conditions of the input space.
func TestSizeTUint64(t *testing.T) {
	for _, test := range tuint64Tests {
		name := fmt.Sprintf("0x%x", test.value)
		t.Run(name, func(t *testing.T) {
			size := tlv.SizeTUint64(test.value)
			if test.size != size {
				t.Fatalf("size mismatch, expected: %d got: %d",
					test.size, size)
			}
		})
	}
}
