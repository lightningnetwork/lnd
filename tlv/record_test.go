package tlv

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/davecgh/go-spew/spew"
)

// TestSortRecords tests that SortRecords is able to properly sort records in
// place.
func TestSortRecords(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		preSort  []Record
		postSort []Record
	}{
		// An empty slice requires no sorting.
		{
			preSort:  []Record{},
			postSort: []Record{},
		},

		// An already sorted slice should be passed through.
		{
			preSort: []Record{
				MakeStaticRecord(1, nil, 0, nil, nil),
				MakeStaticRecord(2, nil, 0, nil, nil),
				MakeStaticRecord(3, nil, 0, nil, nil),
			},
			postSort: []Record{
				MakeStaticRecord(1, nil, 0, nil, nil),
				MakeStaticRecord(2, nil, 0, nil, nil),
				MakeStaticRecord(3, nil, 0, nil, nil),
			},
		},

		// We should be able to sort a randomized set of records .
		{
			preSort: []Record{
				MakeStaticRecord(9, nil, 0, nil, nil),
				MakeStaticRecord(43, nil, 0, nil, nil),
				MakeStaticRecord(1, nil, 0, nil, nil),
				MakeStaticRecord(0, nil, 0, nil, nil),
			},
			postSort: []Record{
				MakeStaticRecord(0, nil, 0, nil, nil),
				MakeStaticRecord(1, nil, 0, nil, nil),
				MakeStaticRecord(9, nil, 0, nil, nil),
				MakeStaticRecord(43, nil, 0, nil, nil),
			},
		},
	}

	for i, testCase := range testCases {
		SortRecords(testCase.preSort)

		if !reflect.DeepEqual(testCase.preSort, testCase.postSort) {
			t.Fatalf("#%v: wrong order: expected %v, got %v", i,
				spew.Sdump(testCase.preSort),
				spew.Sdump(testCase.postSort))
		}
	}
}

// TestRecordMapTransformation tests that we're able to properly morph a set of
// records into a map using TlvRecordsToMap, then the other way around using
// the MapToTlvRecords method.
func TestRecordMapTransformation(t *testing.T) {
	t.Parallel()

	tlvBytes := []byte{1, 2, 3, 4}
	encoder := StubEncoder(tlvBytes)

	testCases := []struct {
		records []Record

		tlvMap map[uint64][]byte
	}{
		// An empty set of records should yield an empty map, and the other
		// way around.
		{
			records: []Record{},
			tlvMap:  map[uint64][]byte{},
		},

		// We should be able to transform this set of records, then obtain
		// the records back in the same order.
		{
			records: []Record{
				MakeStaticRecord(1, nil, 4, encoder, nil),
				MakeStaticRecord(2, nil, 4, encoder, nil),
				MakeStaticRecord(3, nil, 4, encoder, nil),
			},
			tlvMap: map[uint64][]byte{
				1: tlvBytes,
				2: tlvBytes,
				3: tlvBytes,
			},
		},
	}

	for i, testCase := range testCases {
		mappedRecords, err := RecordsToMap(testCase.records)
		if err != nil {
			t.Fatalf("#%v: unable to map records: %v", i, err)
		}

		if !reflect.DeepEqual(mappedRecords, testCase.tlvMap) {
			t.Fatalf("#%v: incorrect record map: expected %v, got %v",
				i, spew.Sdump(testCase.tlvMap),
				spew.Sdump(mappedRecords))
		}

		unmappedRecords := MapToRecords(mappedRecords)

		for i := 0; i < len(testCase.records); i++ {
			if unmappedRecords[i].Type() != testCase.records[i].Type() {
				t.Fatalf("#%v: wrong type: expected %v, got %v",
					i, unmappedRecords[i].Type(),
					testCase.records[i].Type())
			}

			var b bytes.Buffer
			if err := unmappedRecords[i].Encode(&b); err != nil {
				t.Fatalf("#%v: unable to encode record: %v",
					i, err)
			}

			if !bytes.Equal(b.Bytes(), tlvBytes) {
				t.Fatalf("#%v: wrong raw record: "+
					"expected %x, got %x",
					i, tlvBytes, b.Bytes())
			}

			if unmappedRecords[i].Size() != testCase.records[0].Size() {
				t.Fatalf("#%v: wrong size: expected %v, "+
					"got %v", i,
					unmappedRecords[i].Size(),
					testCase.records[i].Size())
			}
		}
	}
}
