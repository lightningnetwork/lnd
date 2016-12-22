package lnwire

import (
	"reflect"
	"testing"

	"github.com/davecgh/go-spew/spew"
)

func TestChannelIDEncoding(t *testing.T) {
	var testCases = []ChannelID{
		{
			BlockHeight: (1 << 24) - 1,
			TxIndex:     (1 << 24) - 1,
			TxPosition:  (1 << 16) - 1,
		},
		{
			BlockHeight: 2304934,
			TxIndex:     2345,
			TxPosition:  5,
		},
		{
			BlockHeight: 9304934,
			TxIndex:     2345,
			TxPosition:  5233,
		},
	}

	for _, testCase := range testCases {
		chanInt := testCase.ToUint64()

		newChanID := NewChanIDFromInt(chanInt)

		if !reflect.DeepEqual(testCase, newChanID) {
			t.Fatalf("chan ID's don't match: expected %v got %v",
				spew.Sdump(testCase), spew.Sdump(newChanID))
		}
	}
}
