package lnwire

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// TestOutPointTLVEncoding tests the TLV encoding and decoding of OutPoint
// structs using the Record interface.
func TestOutPointTLVEncoding(t *testing.T) {
	t.Parallel()

	testOutPoint := OutPoint(wire.OutPoint{
		Hash: chainhash.Hash{
			0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
			0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10,
			0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18,
			0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f, 0x20,
		},
		Index: 12345,
	})

	var extraData ExtraOpaqueData
	require.NoError(t, extraData.PackRecords(&testOutPoint))

	var decodedOutPoint OutPoint
	tlvs, err := extraData.ExtractRecords(&decodedOutPoint)
	require.NoError(t, err)

	require.Contains(t, tlvs, tlv.Type(0))
	require.Equal(t, testOutPoint, decodedOutPoint)
}

// TestOutPointRecord tests the TLV Record interface of OutPoint
// by directly encoding and decoding using the Record method.
func TestOutPointRecord(t *testing.T) {
	t.Parallel()

	testOutPoint := OutPoint(wire.OutPoint{
		Hash: chainhash.Hash{
			0xff, 0xfe, 0xfd, 0xfc, 0xfb, 0xfa, 0xf9, 0xf8,
			0xf7, 0xf6, 0xf5, 0xf4, 0xf3, 0xf2, 0xf1, 0xf0,
			0xef, 0xee, 0xed, 0xec, 0xeb, 0xea, 0xe9, 0xe8,
			0xe7, 0xe6, 0xe5, 0xe4, 0xe3, 0xe2, 0xe1, 0xe0,
		},
		Index: 65535,
	})

	var buf bytes.Buffer
	record := testOutPoint.Record()
	require.NoError(t, record.Encode(&buf))

	var decodedOutPoint OutPoint
	decodedRecord := decodedOutPoint.Record()
	require.NoError(t, decodedRecord.Decode(&buf, uint64(buf.Len())))

	require.Equal(t, testOutPoint, decodedOutPoint)
}

// TestOutPointProperty uses property-based testing to verify that OutPoint
// TLV encoding and decoding is correct for random OutPoint values.
func TestOutPointProperty(t *testing.T) {
	t.Parallel()

	scenario := func(t *rapid.T) {
		wireOutPoint := RandOutPoint(t)
		lnOutPoint := OutPoint(wireOutPoint)

		var buf bytes.Buffer
		record := lnOutPoint.Record()
		err := record.Encode(&buf)
		require.NoError(t, err)

		var decodedOutPoint OutPoint
		decodedRecord := decodedOutPoint.Record()
		err = decodedRecord.Decode(&buf, uint64(buf.Len()))
		require.NoError(t, err)

		require.Equal(t, lnOutPoint, decodedOutPoint)
		require.Equal(t, wireOutPoint, wire.OutPoint(decodedOutPoint))
	}

	rapid.Check(t, scenario)
}

// TestOutPointZeroValues tests that OutPoint handles zero values correctly.
func TestOutPointZeroValues(t *testing.T) {
	t.Parallel()

	zeroOutPoint := OutPoint(wire.OutPoint{})

	var buf bytes.Buffer
	record := zeroOutPoint.Record()
	require.NoError(t, record.Encode(&buf))

	var decodedOutPoint OutPoint
	decodedRecord := decodedOutPoint.Record()
	require.NoError(t, decodedRecord.Decode(&buf, uint64(buf.Len())))

	require.Equal(t, zeroOutPoint, decodedOutPoint)
}
