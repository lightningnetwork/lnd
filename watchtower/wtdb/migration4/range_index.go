package migration4

import (
	"fmt"
	"sync"
)

// rangeItem represents the start and end values of a range.
type rangeItem struct {
	start uint64
	end   uint64
}

// RangeIndexOption describes the signature of a functional option that can be
// used to modify the behaviour of a RangeIndex.
type RangeIndexOption func(*RangeIndex)

// WithSerializeUint64Fn is a functional option that can be used to set the
// function to be used to do the serialization of a uint64 into a byte slice.
func WithSerializeUint64Fn(fn func(uint64) ([]byte, error)) RangeIndexOption {
	return func(index *RangeIndex) {
		index.serializeUint64 = fn
	}
}

// RangeIndex can be used to keep track of which numbers have been added to a
// set. It does so by keeping track of a sorted list of rangeItems. Each
// rangeItem has a start and end value of a range where all values in-between
// have been added to the set. It works well in situations where it is expected
// numbers in the set are not sparse.
type RangeIndex struct {
	// set is a sorted list of rangeItem.
	set []rangeItem

	// mu is used to ensure safe access to set.
	mu sync.Mutex

	// serializeUint64 is the function that can be used to convert a uint64
	// to a byte slice.
	serializeUint64 func(uint64) ([]byte, error)
}

// NewRangeIndex constructs a new RangeIndex. An initial set of ranges may be
// passed to the function in the form of a map.
func NewRangeIndex(ranges map[uint64]uint64,
	opts ...RangeIndexOption) (*RangeIndex, error) {

	index := &RangeIndex{
		serializeUint64: defaultSerializeUint64,
		set:             make([]rangeItem, 0),
	}

	// Apply any functional options.
	for _, o := range opts {
		o(index)
	}

	for s, e := range ranges {
		if err := index.addRange(s, e); err != nil {
			return nil, err
		}
	}

	return index, nil
}

// addRange can be used to add an entire new range to the set. This method
// should only ever be called by NewRangeIndex to initialise the in-memory
// structure and so the RangeIndex mutex is not held during this method.
func (a *RangeIndex) addRange(start, end uint64) error {
	// Check that the given range is valid.
	if start > end {
		return fmt.Errorf("invalid range. Start height %d is larger "+
			"than end height %d", start, end)
	}

	// Collect the ranges that fall before and after the new range along
	// with the start and end values of the new range.
	var before, after []rangeItem
	for _, x := range a.set {
		// If the new start value can't extend the current ranges end
		// value, then the two cannot be merged. The range is added to
		// the group of ranges that fall before the new range.
		if x.end+1 < start {
			before = append(before, x)
			continue
		}

		// If the current ranges start value does not follow on directly
		// from the new end value, then the two cannot be merged. The
		// range is added to the group of ranges that fall after the new
		// range.
		if end+1 < x.start {
			after = append(after, x)
			continue
		}

		// Otherwise, there is an overlap and so the two can be merged.
		start = min(start, x.start)
		end = max(end, x.end)
	}

	// Re-construct the range index set.
	a.set = append(append(before, rangeItem{
		start: start,
		end:   end,
	}), after...)

	return nil
}

// IsInIndex returns true if the given number is in the range set.
func (a *RangeIndex) IsInIndex(n uint64) bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	_, isCovered := a.lowerBoundIndex(n)

	return isCovered
}

// NumInSet returns the number of items covered by the range set.
func (a *RangeIndex) NumInSet() uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()

	var numItems uint64
	for _, r := range a.set {
		numItems += r.end - r.start + 1
	}

	return numItems
}

// MaxHeight returns the highest number covered in the range.
func (a *RangeIndex) MaxHeight() uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()

	if len(a.set) == 0 {
		return 0
	}

	return a.set[len(a.set)-1].end
}

// GetAllRanges returns a copy of the range set in the form of a map.
func (a *RangeIndex) GetAllRanges() map[uint64]uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()

	cp := make(map[uint64]uint64, len(a.set))
	for _, item := range a.set {
		cp[item.start] = item.end
	}

	return cp
}

// lowerBoundIndex returns the index of the RangeIndex that is most appropriate
// for the new value, n. In other words, it returns the index of the rangeItem
// set of the range where the start value is the highest start value in the set
// that is still lower than or equal to the given number, n. The returned
// boolean is true if the given number is already covered in the RangeIndex.
// A returned index of -1 indicates that no lower bound range exists in the set.
// Since the most likely case is that the new number will just extend the
// highest range, a check is first done to see if this is the case which will
// make the methods' computational complexity O(1). Otherwise, a binary search
// is done which brings the computational complexity to O(log N).
func (a *RangeIndex) lowerBoundIndex(n uint64) (int, bool) {
	// If the set is empty, then there is no such index and the value
	// definitely is not in the set.
	if len(a.set) == 0 {
		return -1, false
	}

	// In most cases, the last index item will be the one we want. So just
	// do a quick check on that index first to avoid doing the binary
	// search.
	lastIndex := len(a.set) - 1
	lastRange := a.set[lastIndex]
	if lastRange.start <= n {
		return lastIndex, lastRange.end >= n
	}

	// Otherwise, do a binary search to find the index of interest.
	var (
		low        = 0
		high       = len(a.set) - 1
		rangeIndex = -1
	)
	for {
		mid := (low + high) / 2
		currentRange := a.set[mid]

		switch {
		case currentRange.start > n:
			// If the start of the range is greater than n, we can
			// completely cut out that entire part of the array.
			high = mid

		case currentRange.start < n:
			// If the range already includes the given height, we
			// can stop searching now.
			if currentRange.end >= n {
				return mid, true
			}

			// If the start of the range is smaller than n, we can
			// store this as the new best index to return.
			rangeIndex = mid

			// If low and mid are already equal, then increment low
			// by 1. Exit if this means that low is now greater than
			// high.
			if low == mid {
				low = mid + 1
				if low > high {
					return rangeIndex, false
				}
			} else {
				low = mid
			}

			continue

		default:
			// If the height is equal to the start value of the
			// current range that mid is pointing to, then the
			// height is already covered.
			return mid, true
		}

		// Exit if we have checked all the ranges.
		if low == high {
			break
		}
	}

	return rangeIndex, false
}

// KVStore is an interface representing a key-value store.
type KVStore interface {
	// Put saves the specified key/value pair to the store. Keys that do not
	// already exist are added and keys that already exist are overwritten.
	Put(key, value []byte) error

	// Delete removes the specified key from the bucket. Deleting a key that
	// does not exist does not return an error.
	Delete(key []byte) error
}

// Add adds a single number to the range set. It first attempts to apply the
// necessary changes to the passed KV store and then only if this succeeds, will
// the changes be applied to the in-memory structure.
func (a *RangeIndex) Add(newHeight uint64, kv KVStore) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	// Compute the changes that will need to be applied to both the sorted
	// rangeItem array representation and the key-value store representation
	// of the range index.
	arrayChanges, kvStoreChanges := a.getChanges(newHeight)

	// First attempt to apply the KV store changes. Only if this succeeds
	// will we apply the changes to our in-memory range index structure.
	err := a.applyKVChanges(kv, kvStoreChanges)
	if err != nil {
		return err
	}

	// Since the DB changes were successful, we can now commit the
	// changes to our in-memory representation of the range set.
	a.applyArrayChanges(arrayChanges)

	return nil
}

// applyKVChanges applies the given set of kvChanges to a KV store. It is
// assumed that a transaction is being held on the kv store so that if any
// of the actions of the function fails, the changes will be reverted.
func (a *RangeIndex) applyKVChanges(kv KVStore, changes *kvChanges) error {
	// Exit early if there are no changes to apply.
	if kv == nil || changes == nil {
		return nil
	}

	// Check if any range pair needs to be deleted.
	if changes.deleteKVKey != nil {
		del, err := a.serializeUint64(*changes.deleteKVKey)
		if err != nil {
			return err
		}

		if err := kv.Delete(del); err != nil {
			return err
		}
	}

	start, err := a.serializeUint64(changes.key)
	if err != nil {
		return err
	}

	end, err := a.serializeUint64(changes.value)
	if err != nil {
		return err
	}

	return kv.Put(start, end)
}

// applyArrayChanges applies the given arrayChanges to the in-memory RangeIndex
// itself. This should only be done once the persisted kv store changes have
// already been applied.
func (a *RangeIndex) applyArrayChanges(changes *arrayChanges) {
	if changes == nil {
		return
	}

	if changes.indexToDelete != nil {
		a.set = append(
			a.set[:*changes.indexToDelete],
			a.set[*changes.indexToDelete+1:]...,
		)
	}

	if changes.newIndex != nil {
		switch {
		case *changes.newIndex == 0:
			a.set = append([]rangeItem{{
				start: changes.start,
				end:   changes.end,
			}}, a.set...)

		case *changes.newIndex == len(a.set):
			a.set = append(a.set, rangeItem{
				start: changes.start,
				end:   changes.end,
			})

		default:
			a.set = append(
				a.set[:*changes.newIndex+1],
				a.set[*changes.newIndex:]...,
			)
			a.set[*changes.newIndex] = rangeItem{
				start: changes.start,
				end:   changes.end,
			}
		}

		return
	}

	if changes.indexToEdit != nil {
		a.set[*changes.indexToEdit] = rangeItem{
			start: changes.start,
			end:   changes.end,
		}
	}
}

// arrayChanges encompasses the diff to apply to the sorted rangeItem array
// representation of a range index. Such a diff will either include adding a
// new range or editing an existing range. If an existing range is edited, then
// the diff might also include deleting an index (this will be the case if the
// editing of the one range results in the merge of another range).
type arrayChanges struct {
	start uint64
	end   uint64

	// newIndex, if set, is the index of the in-memory range array where a
	// new range, [start:end], should be added. newIndex should never be
	// set at the same time as indexToEdit or indexToDelete.
	newIndex *int

	// indexToDelete, if set, is the index of the sorted rangeItem array
	// that should be deleted. This should be applied before reading the
	// index value of indexToEdit. This should not be set at the same time
	// as newIndex.
	indexToDelete *int

	// indexToEdit is the index of the in-memory range array that should be
	// edited. The range at this index will be changed to [start:end]. This
	// should only be read after indexToDelete index has been deleted.
	indexToEdit *int
}

// kvChanges encompasses the diff to apply to a KV-store representation of a
// range index. A kv-store diff for the addition of a single number to the range
// index will include either a brand new key-value pair or the altering of the
// value of an existing key. Optionally, the diff may also include the deletion
// of an existing key. A deletion will be required if the addition of the new
// number results in the merge of two ranges.
type kvChanges struct {
	key   uint64
	value uint64

	// deleteKVKey, if set, is the key of the kv store representation that
	// should be deleted.
	deleteKVKey *uint64
}

// getChanges will calculate and return the changes that need to be applied to
// both the sorted-rangeItem-array representation and the key-value store
// representation of the range index.
func (a *RangeIndex) getChanges(n uint64) (*arrayChanges, *kvChanges) {
	// If the set is empty then a new range item is added.
	if len(a.set) == 0 {
		// For the array representation, a new range [n:n] is added to
		// the first index of the array.
		firstIndex := 0
		ac := &arrayChanges{
			newIndex: &firstIndex,
			start:    n,
			end:      n,
		}

		// For the KV representation, a new [n:n] pair is added.
		kvc := &kvChanges{
			key:   n,
			value: n,
		}

		return ac, kvc
	}

	// Find the index of the lower bound range to the new number.
	indexOfRangeBelow, alreadyCovered := a.lowerBoundIndex(n)

	switch {
	// The new number is already covered by the range index. No changes are
	// required.
	case alreadyCovered:
		return nil, nil

	// No lower bound index exists.
	case indexOfRangeBelow < 0:
		// Check if the very first range can be merged into this new
		// one.
		if n+1 == a.set[0].start {
			// If so, the two ranges can be merged and so the start
			// value of the range is n and the end value is the end
			// of the existing first range.
			start := n
			end := a.set[0].end

			// For the array representation, we can just edit the
			// first entry of the array
			editIndex := 0
			ac := &arrayChanges{
				indexToEdit: &editIndex,
				start:       start,
				end:         end,
			}

			// For the KV store representation, we add a new kv pair
			// and delete the range with the key equal to the start
			// value of the range we are merging.
			kvKeyToDelete := a.set[0].start
			kvc := &kvChanges{
				key:         start,
				value:       end,
				deleteKVKey: &kvKeyToDelete,
			}

			return ac, kvc
		}

		// Otherwise, we add a new index.

		// For the array representation, a new range [n:n] is added to
		// the first index of the array.
		newIndex := 0
		ac := &arrayChanges{
			newIndex: &newIndex,
			start:    n,
			end:      n,
		}

		// For the KV representation, a new [n:n] pair is added.
		kvc := &kvChanges{
			key:   n,
			value: n,
		}

		return ac, kvc

	// A lower range does exist, and it can be extended to include this new
	// number.
	case a.set[indexOfRangeBelow].end+1 == n:
		start := a.set[indexOfRangeBelow].start
		end := n
		indexToChange := indexOfRangeBelow

		// If there are no intervals above this one or if there are, but
		// they can't be merged into this one then we just need to edit
		// this interval.
		if indexOfRangeBelow == len(a.set)-1 ||
			a.set[indexOfRangeBelow+1].start != n+1 {

			// For the array representation, we just edit the index.
			ac := &arrayChanges{
				indexToEdit: &indexToChange,
				start:       start,
				end:         end,
			}

			// For the key-value representation, we just overwrite
			// the end value at the existing start key.
			kvc := &kvChanges{
				key:   start,
				value: end,
			}

			return ac, kvc
		}

		// There is a range above this one that we need to merge into
		// this one.
		delIndex := indexOfRangeBelow + 1
		end = a.set[delIndex].end

		// For the array representation, we delete the range above this
		// one and edit this range to include the end value of the range
		// above.
		ac := &arrayChanges{
			indexToDelete: &delIndex,
			indexToEdit:   &indexToChange,
			start:         start,
			end:           end,
		}

		// For the kv representation, we tweak the end value of an
		// existing key and delete the key of the range we are deleting.
		deleteKey := a.set[delIndex].start
		kvc := &kvChanges{
			key:         start,
			value:       end,
			deleteKVKey: &deleteKey,
		}

		return ac, kvc

	// A lower range does exist, but it can't be extended to include this
	// new number, and so we need to add a new range after the lower bound
	// range.
	default:
		newIndex := indexOfRangeBelow + 1

		// If there are no ranges above this new one or if there are,
		// but they can't be merged into this new one, then we can just
		// add the new one as is.
		if newIndex == len(a.set) || a.set[newIndex].start != n+1 {
			ac := &arrayChanges{
				newIndex: &newIndex,
				start:    n,
				end:      n,
			}

			kvc := &kvChanges{
				key:   n,
				value: n,
			}

			return ac, kvc
		}

		// Else, we merge the above index.
		start := n
		end := a.set[newIndex].end
		toEdit := newIndex

		// For the array representation, we edit the range above to
		// include the new start value.
		ac := &arrayChanges{
			indexToEdit: &toEdit,
			start:       start,
			end:         end,
		}

		// For the kv representation, we insert the new start-end key
		// value pair and delete the key using the old start value.
		delKey := a.set[newIndex].start
		kvc := &kvChanges{
			key:         start,
			value:       end,
			deleteKVKey: &delKey,
		}

		return ac, kvc
	}
}

func defaultSerializeUint64(i uint64) ([]byte, error) {
	var b [8]byte
	byteOrder.PutUint64(b[:], i)
	return b[:], nil
}
