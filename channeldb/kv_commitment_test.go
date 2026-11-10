package channeldb

import (
	"bytes"
	"testing"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// TestSerializeHtlcsReadableBound checks that an HTLC is only written while
// its combined onion and extra data stays within what the reader accepts, so
// every HTLC that is written can be read back.
func TestSerializeHtlcsReadableBound(t *testing.T) {
	t.Parallel()

	const customType = uint64(lnwire.MinCustomRecordsTlvType + 7)

	// newHtlc returns an HTLC with one custom record holding a value of
	// the given size.
	newHtlc := func(valueLen int) HTLC {
		return HTLC{
			CustomRecords: lnwire.CustomRecords{
				customType: bytes.Repeat([]byte{1}, valueLen),
			},
		}
	}

	// Measure the TLV overhead of the custom record, which is the same
	// for every value length between 253 and 65535 bytes.
	const probeLen = 1_000
	probe := newHtlc(probeLen)
	require.NoError(t, probe.serializeExtraData())
	overhead := len(probe.ExtraData) - probeLen

	// The largest value that keeps the HTLC within the reader's limit.
	maxValueLen := maxVarBytesLen - lnwire.OnionPacketSize - overhead

	// An HTLC exactly at the limit is written and reads back intact.
	var b bytes.Buffer
	require.NoError(t, SerializeHtlcs(&b, newHtlc(maxValueLen)))

	htlcs, err := DeserializeHtlcs(&b)
	require.NoError(t, err)
	require.Len(t, htlcs, 1)
	require.Len(t, htlcs[0].CustomRecords[customType], maxValueLen)

	// One more byte would make the record unreadable, so it is refused.
	b.Reset()
	err = SerializeHtlcs(&b, newHtlc(maxValueLen+1))
	require.ErrorContains(t, err, "too large")
}
