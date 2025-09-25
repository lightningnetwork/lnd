package lnwire

import (
	"reflect"
	"testing"

	"github.com/davecgh/go-spew/spew"
	"github.com/stretchr/testify/require"
)

func TestShortChannelIDEncoding(t *testing.T) {
	t.Parallel()

	var testCases = []ShortChannelID{
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

		newChanID := NewShortChanIDFromInt(chanInt)

		if !reflect.DeepEqual(testCase, newChanID) {
			t.Fatalf("chan ID's don't match: expected %v got %v",
				spew.Sdump(testCase), spew.Sdump(newChanID))
		}
	}
}

// TestScidTypeEncodeDecode tests that we're able to properly encode and decode
// ShortChannelID within TLV streams.
func TestScidTypeEncodeDecode(t *testing.T) {
	t.Parallel()

	aliasScid := ShortChannelID{
		BlockHeight: (1 << 24) - 1,
		TxIndex:     (1 << 24) - 1,
		TxPosition:  (1 << 16) - 1,
	}

	var extraData ExtraOpaqueData
	require.NoError(t, extraData.PackRecords(&aliasScid))

	var aliasScid2 ShortChannelID
	tlvs, err := extraData.ExtractRecords(&aliasScid2)
	require.NoError(t, err)

	require.Contains(t, tlvs, AliasScidRecordType)
	require.Equal(t, aliasScid, aliasScid2)
}

// TestScidTypeDecodeInvalidLength ensures that decoding a ShortChannelID TLV
// with an invalid length (anything other than 8 bytes) fails with an error.
func TestScidTypeDecodeInvalidLength(t *testing.T) {
	t.Parallel()

	aliasScid := ShortChannelID{
		BlockHeight: 1, TxIndex: 1, TxPosition: 1,
	}

	var extraData ExtraOpaqueData
	require.NoError(t, extraData.PackRecords(&aliasScid))

	// Corrupt the TLV length field to simulate malformed input.
	extraData[1] = 8 + 1

	var out ShortChannelID
	_, err := extraData.ExtractRecords(&out)
	require.Error(t, err)

	extraData[1] = 8 - 1

	_, err = extraData.ExtractRecords(&out)
	require.Error(t, err)
}
