package pool_test

import (
	"bytes"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/buffer"
	"github.com/lightningnetwork/lnd/pool"
)

type mockRecycler bool

func (m *mockRecycler) Recycle() {
	*m = false
}

// TestRecyclers verifies that known recyclable types properly return to their
// zero-value after invoking Recycle.
func TestRecyclers(t *testing.T) {
	tests := []struct {
		name    string
		newItem func() interface{}
	}{
		{
			"mock recycler",
			func() interface{} { return new(mockRecycler) },
		},
		{
			"write_buffer",
			func() interface{} { return new(buffer.Write) },
		},
		{
			"read_buffer",
			func() interface{} { return new(buffer.Read) },
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Initialize the Recycler to test.
			r := test.newItem().(pool.Recycler)

			// Dirty the item.
			dirtyGeneric(t, r)

			// Invoke Recycle to clear the item.
			r.Recycle()

			// Assert the item is now clean.
			isCleanGeneric(t, r)
		})
	}
}

type recyclePoolTest struct {
	name    string
	newPool func() interface{}
}

// TestGenericRecyclePoolTests generically tests that pools derived from the
// base Recycle pool properly are properly configured.
func TestConcreteRecyclePoolTests(t *testing.T) {
	const (
		gcInterval     = time.Second
		expiryInterval = 250 * time.Millisecond
	)

	tests := []recyclePoolTest{
		{
			name: "write buffer pool",
			newPool: func() interface{} {
				return pool.NewWriteBuffer(
					gcInterval, expiryInterval,
				)
			},
		},
		{
			name: "read buffer pool",
			newPool: func() interface{} {
				return pool.NewReadBuffer(
					gcInterval, expiryInterval,
				)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			testRecyclePool(t, test)
		})
	}
}

func testRecyclePool(t *testing.T, test recyclePoolTest) {
	p := test.newPool()

	// Take an item from the pool.
	r1 := takeGeneric(t, p)

	// Dirty the item.
	dirtyGeneric(t, r1)

	// Return the item to the pool.
	returnGeneric(t, p, r1)

	// Take items from the pool until we find the original. We expect at
	// most two, in the event that a fresh item is populated after the
	// first is taken.
	for i := 0; i < 2; i++ {
		// Wait a small duration to ensure the tests are reliable, and
		// don't to active the non-blocking case unintentionally.
		<-time.After(time.Millisecond)

		r2 := takeGeneric(t, p)

		// Take an item, skipping those whose pointer does not match the
		// one we dirtied.
		if r1 != r2 {
			continue
		}

		// Finally, verify that the item has been properly cleaned.
		isCleanGeneric(t, r2)

		return
	}

	t.Fatalf("original item not found")
}

func takeGeneric(t *testing.T, p interface{}) pool.Recycler {
	t.Helper()

	switch pp := p.(type) {
	case *pool.WriteBuffer:
		return pp.Take()

	case *pool.ReadBuffer:
		return pp.Take()

	default:
		t.Fatalf("unknown pool type: %T", p)
	}

	return nil
}

func returnGeneric(t *testing.T, p, item interface{}) {
	t.Helper()

	switch pp := p.(type) {
	case *pool.WriteBuffer:
		pp.Return(item.(*buffer.Write))

	case *pool.ReadBuffer:
		pp.Return(item.(*buffer.Read))

	default:
		t.Fatalf("unknown pool type: %T", p)
	}
}

func dirtyGeneric(t *testing.T, i interface{}) {
	t.Helper()

	switch item := i.(type) {
	case *mockRecycler:
		*item = true

	case *buffer.Write:
		dirtySlice(item[:])

	case *buffer.Read:
		dirtySlice(item[:])

	default:
		t.Fatalf("unknown item type: %T", i)
	}

}

func dirtySlice(slice []byte) {
	for i := range slice {
		slice[i] = 0xff
	}
}

func isCleanGeneric(t *testing.T, i interface{}) {
	t.Helper()

	switch item := i.(type) {
	case *mockRecycler:
		if isDirty := *item; isDirty {
			t.Fatalf("mock recycler still diry")
		}

	case *buffer.Write:
		isCleanSlice(t, item[:])

	case *buffer.Read:
		isCleanSlice(t, item[:])

	default:
		t.Fatalf("unknown item type: %T", i)
	}
}

func isCleanSlice(t *testing.T, slice []byte) {
	t.Helper()

	expSlice := make([]byte, len(slice))
	if !bytes.Equal(expSlice, slice) {
		t.Fatalf("slice not recycled, want: %v, got: %v",
			expSlice, slice)
	}
}
