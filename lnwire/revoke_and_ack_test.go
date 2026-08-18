package lnwire

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRevokeAndAckEncodeDecode tests RevokeAndAck message encoding and
// decoding with and without custom records, asserting that a message without
// custom records is byte-identical to its pre-CustomRecords encoding.
func TestRevokeAndAckEncodeDecode(t *testing.T) {
	t.Parallel()

	chanIDBytes, err := generateRandomBytes(32)
	require.NoError(t, err)
	var chanID ChannelID
	copy(chanID[:], chanIDBytes)

	revBytes, err := generateRandomBytes(32)
	require.NoError(t, err)
	var revocation [32]byte
	copy(revocation[:], revBytes)

	nextRevKey, err := pubkeyFromHex(
		"0228f2af0abe322403480fb3ee172f7f1601e67d1da6cad40b54c4468d4" +
			"8236c39",
	)
	require.NoError(t, err)

	recordValue, err := generateRandomBytes(10)
	require.NoError(t, err)
	customRecords := CustomRecords{
		uint64(MinCustomRecordsTlvType): recordValue,
	}

	testCases := []struct {
		name              string
		msg               RevokeAndAck
		expectEncodeError bool
	}{{
		name: "with custom records",
		msg: RevokeAndAck{
			ChanID:            chanID,
			Revocation:        revocation,
			NextRevocationKey: nextRevKey,
			CustomRecords:     customRecords,
		},
	}, {
		name: "without custom records",
		msg: RevokeAndAck{
			ChanID:            chanID,
			Revocation:        revocation,
			NextRevocationKey: nextRevKey,
		},
	}, {
		name: "invalid custom records",
		msg: RevokeAndAck{
			ChanID:            chanID,
			Revocation:        revocation,
			NextRevocationKey: nextRevKey,
			CustomRecords: CustomRecords{
				MinCustomRecordsTlvType - 1: recordValue,
			},
		},
		expectEncodeError: true,
	}}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			err := tc.msg.Encode(&buf, 0)
			if tc.expectEncodeError {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)

			var decoded RevokeAndAck
			err = decoded.Decode(bytes.NewReader(buf.Bytes()), 0)
			require.NoError(t, err)
			require.Equal(t, tc.msg, decoded)
		})
	}

	// A message without custom records must be byte-identical to the
	// legacy encoding: fixed fields only, no TLV appendix at all.
	var legacyBuf bytes.Buffer
	require.NoError(
		t, WriteChannelID(&legacyBuf, chanID),
	)
	require.NoError(t, WriteBytes(&legacyBuf, revocation[:]))
	require.NoError(t, WritePublicKey(&legacyBuf, nextRevKey))

	var noRecordsBuf bytes.Buffer
	msg := RevokeAndAck{
		ChanID:            chanID,
		Revocation:        revocation,
		NextRevocationKey: nextRevKey,
	}
	require.NoError(t, msg.Encode(&noRecordsBuf, 0))
	require.Equal(
		t, legacyBuf.Bytes(), noRecordsBuf.Bytes(),
		fmt.Sprintf("no-records encoding must stay byte-identical "+
			"to the legacy wire format, got %x",
			noRecordsBuf.Bytes()),
	)
}
