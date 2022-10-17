package wtdb

import (
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestRangeIndex tests that the RangeIndex works as expected.
func TestRangeIndex(t *testing.T) {
	t.Parallel()

	assertRanges := func(index *RangeIndex, kvStore *mockKVStore,
		v map[uint64]uint64) {

		require.EqualValues(t, v, index.GetAllRanges())
		require.EqualValues(t, v, kvStore.kv)
	}

	t.Run("test zero value height", func(t *testing.T) {
		t.Parallel()

		kvStore := newMockKVStore(nil)
		index, err := NewRangeIndex(nil)
		require.NoError(t, err)

		// Since zero values are tricky, assert that the empty index
		// does not include zero.
		require.False(t, index.IsInIndex(0))

		// Now add zero to the index.
		_ = index.Add(0, kvStore)
		assertRanges(index, kvStore, map[uint64]uint64{0: 0})
		require.True(t, index.IsInIndex(0))
	})

	t.Run("add duplicates", func(t *testing.T) {
		t.Parallel()

		kvStore := newMockKVStore(nil)
		index, err := NewRangeIndex(nil)
		require.NoError(t, err)

		require.False(t, index.IsInIndex(1))

		// Add 1 to the range.
		err = index.Add(1, kvStore)
		require.NoError(t, err)

		assertRanges(index, kvStore, map[uint64]uint64{1: 1})
		require.EqualValues(t, 1, index.MaxHeight())
		require.True(t, index.IsInIndex(1))

		// Add 1 again and assert that nothing has changed.
		err = index.Add(1, kvStore)
		require.NoError(t, err)

		assertRanges(index, kvStore, map[uint64]uint64{1: 1})
		require.EqualValues(t, 1, index.MaxHeight())
		require.True(t, index.IsInIndex(1))
	})

	t.Run("extend an existing range", func(t *testing.T) {
		t.Parallel()

		kvStore := newMockKVStore(nil)
		index, err := NewRangeIndex(nil)
		require.NoError(t, err)

		assertRanges(index, kvStore, map[uint64]uint64{})

		// Add 2.
		_ = index.Add(2, kvStore)
		assertRanges(index, kvStore, map[uint64]uint64{
			2: 2,
		})

		// Add 3, 4 and 5 and assert that these just extend the existing
		// range by incrementing its end value.
		_ = index.Add(3, kvStore)
		_ = index.Add(4, kvStore)
		_ = index.Add(5, kvStore)
		assertRanges(index, kvStore, map[uint64]uint64{
			2: 5,
		})

		// Now add 1 and 0 and assert that these just extend the
		// existing range by decrementing its start value.
		_ = index.Add(1, kvStore)
		_ = index.Add(0, kvStore)
		assertRanges(index, kvStore, map[uint64]uint64{
			0: 5,
		})

		// Assert various other properties of the current range.
		require.True(t, index.IsInIndex(3))
		require.EqualValues(t, 5, index.MaxHeight())
	})

	t.Run("add new ranges above and below", func(t *testing.T) {
		t.Parallel()

		// Initialise the index with an initial range.
		initialState := map[uint64]uint64{
			4: 10,
		}

		kvStore := newMockKVStore(initialState)
		index, err := NewRangeIndex(initialState)
		require.NoError(t, err)

		// Add 2 and 12. This should create two new ranges in the index.
		_ = index.Add(12, kvStore)
		_ = index.Add(2, kvStore)
		assertRanges(index, kvStore, map[uint64]uint64{
			2:  2,
			4:  10,
			12: 12,
		})

		// Assert various other properties of the current range.
		require.EqualValues(t, 12, index.MaxHeight())
		require.False(t, index.IsInIndex(3))
		require.False(t, index.IsInIndex(11))
		require.True(t, index.IsInIndex(2))
		require.True(t, index.IsInIndex(5))
		require.True(t, index.IsInIndex(12))
	})

	t.Run("merging two ranges", func(t *testing.T) {
		t.Parallel()

		// Initialise the index with an initial set of ranges.
		initialState := map[uint64]uint64{
			2:  2,
			4:  10,
			12: 12,
		}

		kvStore := newMockKVStore(initialState)
		index, err := NewRangeIndex(initialState)
		require.NoError(t, err)

		// Adding 3 should merge the first and second ranges.
		_ = index.Add(3, kvStore)
		assertRanges(index, kvStore, map[uint64]uint64{
			2:  10,
			12: 12,
		})

		// Adding 11 should merge the first and second ranges.
		_ = index.Add(11, kvStore)
		assertRanges(index, kvStore, map[uint64]uint64{
			2: 12,
		})

		// Assert various other properties of the current range.
		require.EqualValues(t, 12, index.MaxHeight())
		require.True(t, index.IsInIndex(2))
		require.True(t, index.IsInIndex(5))
		require.True(t, index.IsInIndex(12))
		require.False(t, index.IsInIndex(1))
	})

	t.Run("failure applying KV store updates", func(t *testing.T) {
		t.Parallel()

		kvStore := newMockKVStore(nil)
		index, err := NewRangeIndex(nil)
		require.NoError(t, err)

		assertRanges(index, kvStore, map[uint64]uint64{})

		// Ensure that the kv store will return an error when its
		// methods are called.
		kvStore.setError(fmt.Errorf("db error"))

		// Now attempt to add a new item to the range.
		err = index.Add(20, kvStore)
		require.Error(t, err)

		// Assert that the update failed for both the kv store and the
		// array store.
		assertRanges(index, kvStore, map[uint64]uint64{})

		// Now let the kv store again not return an error.
		kvStore.setError(nil)

		// Again attempt to add a new item to the range.
		err = index.Add(20, kvStore)
		require.NoError(t, err)

		// It should now succeed.
		assertRanges(index, kvStore, map[uint64]uint64{
			20: 20,
		})
	})

	t.Run("initialising with different ranges", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			name          string
			input         map[uint64]uint64
			expectErr     bool
			expectedIndex map[uint64]uint64
		}{
			{
				name: "invalid ranges",
				input: map[uint64]uint64{
					1: 5,
					6: 4,
				},
				expectErr: true,
			},
			{
				name: "non-overlapping ranges",
				input: map[uint64]uint64{
					1: 2,
					4: 6,
					8: 20,
				},
				expectedIndex: map[uint64]uint64{
					1: 2,
					4: 6,
					8: 20,
				},
			},
			{
				name: "merge-able ranges",
				input: map[uint64]uint64{
					1: 2,
					3: 6,
					7: 20,
				},
				expectedIndex: map[uint64]uint64{
					1: 20,
				},
			},
			{
				name: "overlapping ranges",
				input: map[uint64]uint64{
					1: 4,
					3: 7,
					6: 20,
				},
				expectedIndex: map[uint64]uint64{
					1: 20,
				},
			},
		}

		for _, test := range tests {
			index, err := NewRangeIndex(test.input)
			if test.expectErr {
				require.Error(t, err)
				continue
			}
			require.NoError(t, err)

			require.EqualValues(
				t, test.expectedIndex, index.GetAllRanges(),
			)
		}
	})

	t.Run("test large number of random inserts", func(t *testing.T) {
		t.Parallel()

		size := 10000

		// Construct an array with values from 0 to size.
		arr := make([]uint64, size)
		for i := 0; i < size; i++ {
			arr[i] = uint64(i)
		}

		// Shuffle the array so that the values are not added in order.
		rand.Seed(time.Now().UnixNano())
		rand.Shuffle(len(arr), func(i, j int) {
			arr[i], arr[j] = arr[j], arr[i]
		})

		kvStore := newMockKVStore(nil)
		index, err := NewRangeIndex(nil)
		require.NoError(t, err)

		// Now add each item in the array to the index.
		for _, n := range arr {
			_ = index.Add(n, kvStore)
		}

		// Assert that in the end, there is only a single range.
		assertRanges(index, kvStore, map[uint64]uint64{
			0: uint64(size - 1),
		})

		require.EqualValues(t, uint64(size-1), index.MaxHeight())
	})
}

type mockKVStore struct {
	kv map[uint64]uint64

	err error
}

func newMockKVStore(initialRanges map[uint64]uint64) *mockKVStore {
	if initialRanges != nil {
		return &mockKVStore{
			kv: initialRanges,
		}
	}

	return &mockKVStore{
		kv: make(map[uint64]uint64),
	}
}

func (m *mockKVStore) setError(err error) {
	m.err = err
}

func (m *mockKVStore) Put(key, value []byte) error {
	if m.err != nil {
		return m.err
	}

	k := byteOrder.Uint64(key)
	v := byteOrder.Uint64(value)

	m.kv[k] = v

	return nil
}

func (m *mockKVStore) Delete(key []byte) error {
	if m.err != nil {
		return m.err
	}

	k := byteOrder.Uint64(key)
	delete(m.kv, k)

	return nil
}
