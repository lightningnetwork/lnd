package channeldb

import "github.com/lightningnetwork/lnd/kvdb"

type paginator struct {
	// cursor is the cursor which we are using to iterate through a bucket.
	cursor kvdb.RCursor

	// reversed indicates whether we are paginating forwards or backwards.
	reversed bool

	// indexOffset is the index from which we will begin querying.
	indexOffset uint64

	// totalItems is the total number of items we allow in our response.
	totalItems uint64
}

// NewPaginator returns a struct which can be used to query an indexed bucket
// in pages.
func NewPaginator(c kvdb.RCursor, reversed bool,
	indexOffset, totalItems uint64) paginator {

	return paginator{
		cursor:      c,
		reversed:    reversed,
		indexOffset: indexOffset,
		totalItems:  totalItems,
	}
}

// keyValueForIndex seeks our cursor to a given index and returns the key and
// value at that position.
func (p paginator) keyValueForIndex(index uint64) ([]byte, []byte) {
	var keyIndex [8]byte
	byteOrder.PutUint64(keyIndex[:], index)
	return p.cursor.Seek(keyIndex[:])
}

// lastIndex returns the last value in our index, if our index is empty it
// returns 0.
func (p paginator) lastIndex() uint64 {
	keyIndex, _ := p.cursor.Last()
	if keyIndex == nil {
		return 0
	}

	return byteOrder.Uint64(keyIndex)
}

// nextKey is a helper closure to determine what key we should use next when
// we are iterating, depending on whether we are iterating forwards or in
// reverse.
func (p paginator) nextKey() ([]byte, []byte) {
	if p.reversed {
		return p.cursor.Prev()
	}
	return p.cursor.Next()
}

// cursorStart gets the index key and value for the first item we are looking
// up, taking into account that we may be paginating in reverse. The index
// offset provided is *excusive* so we will start with the item after the offset
// for forwards queries, and the item before the index for backwards queries.
func (p paginator) cursorStart() ([]byte, []byte) {
	indexKey, indexValue := p.keyValueForIndex(p.indexOffset + 1)

	// If the query is specifying reverse iteration, then we must
	// handle a few offset cases.
	if p.reversed {
		switch {
		// This indicates the default case, where no offset was
		// specified. In that case we just start from the last
		// entry.
		case p.indexOffset == 0:
			indexKey, indexValue = p.cursor.Last()

		// This indicates the offset being set to the very
		// first entry. Since there are no entries before
		// this offset, and the direction is reversed, we can
		// return without adding any invoices to the response.
		case p.indexOffset == 1:
			return nil, nil

		// If we have been given an index offset that is beyond our last
		// index value, we just return the last indexed value in our set
		// since we are querying in reverse. We do not cover the case
		// where our index offset equals our last index value, because
		// index offset is exclusive, so we would want to start at the
		// value before our last index.
		case p.indexOffset > p.lastIndex():
			return p.cursor.Last()

		// Otherwise we have an index offset which is within our set of
		// indexed keys, and we want to start at the item before our
		// offset. We seek to our index offset, then return the element
		// before it. We do this rather than p.indexOffset-1 to account
		// for indexes that have gaps.
		default:
			p.keyValueForIndex(p.indexOffset)
			indexKey, indexValue = p.cursor.Prev()
		}
	}

	return indexKey, indexValue
}

// Query gets the start point for our index offset and iterates through keys
// in our index until we reach the total number of items required for the query
// or we run out of cursor values. This function takes a fetchAndAppend function
// which is responsible for looking up the entry at that index, adding the entry
// to its set of return items (if desired) and return a boolean which indicates
// whether the item was added. This is required to allow the paginator to
// determine when the response has the maximum number of required items.
func (p paginator) Query(fetchAndAppend func(k, v []byte) (bool, error)) error {
	indexKey, indexValue := p.cursorStart()

	var totalItems int
	for ; indexKey != nil; indexKey, indexValue = p.nextKey() {
		// If our current return payload exceeds the max number
		// of invoices, then we'll exit now.
		if uint64(totalItems) >= p.totalItems {
			break
		}

		added, err := fetchAndAppend(indexKey, indexValue)
		if err != nil {
			return err
		}

		// If we added an item to our set in the latest fetch and append
		// we increment our total count.
		if added {
			totalItems++
		}
	}

	return nil
}
