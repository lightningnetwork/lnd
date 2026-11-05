package channeldb

import (
	"bytes"
	"math"
	"testing"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

func TestSerializeLogUpdatesCountLimit(t *testing.T) {
	updates := make([]LogUpdate, math.MaxUint16+1)
	fee := &lnwire.UpdateFee{FeePerKw: 1000}
	for i := range updates {
		updates[i] = LogUpdate{
			LogIndex:  uint64(i),
			UpdateMsg: fee,
		}
	}

	var buf bytes.Buffer
	require.NoError(t, serializeLogUpdates(
		&buf, updates[:math.MaxUint16],
	))

	decoded, err := deserializeLogUpdateStream(
		bytes.NewReader(buf.Bytes()),
	)
	require.NoError(t, err)
	require.Len(t, decoded, math.MaxUint16)
	require.Equal(
		t, uint64(math.MaxUint16-1), decoded[len(decoded)-1].LogIndex,
	)

	buf.Reset()
	err = serializeLogUpdates(&buf, updates)
	require.ErrorContains(t, err, "too many log updates")
	require.Zero(t, buf.Len())
}
