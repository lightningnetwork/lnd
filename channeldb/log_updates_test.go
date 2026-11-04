package channeldb

import (
	"bytes"
	"encoding/binary"
	"math"
	"testing"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

func TestDeserializeWrappedLogUpdates(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, binary.Write(&buf, binary.BigEndian, uint16(0)))

	fee := &lnwire.UpdateFee{FeePerKw: 1000}
	for i := 0; i <= math.MaxUint16; i++ {
		var update lnwire.Message = fee
		if i == 0 {
			update = &lnwire.UpdateFulfillHTLC{ID: 7}
		}

		require.NoError(t, WriteElements(
			&buf, uint64(i), update,
		))
	}

	updates, err := deserializeLogUpdates(bytes.NewReader(buf.Bytes()))
	require.NoError(t, err)
	require.Len(t, updates, math.MaxUint16+1)
	require.Equal(t, uint64(0), updates[0].LogIndex)
	require.IsType(t, &lnwire.UpdateFulfillHTLC{}, updates[0].UpdateMsg)
	require.Equal(
		t, uint64(math.MaxUint16), updates[len(updates)-1].LogIndex,
	)
}

func TestDeserializeLogUpdatesRejectsTrailingData(t *testing.T) {
	_, err := deserializeLogUpdates(bytes.NewReader([]byte{0, 0, 1}))
	require.ErrorContains(t, err, "unable to read trailing log update")

	var buf bytes.Buffer
	require.NoError(t, binary.Write(&buf, binary.BigEndian, uint16(0)))
	require.NoError(t, WriteElements(
		&buf, uint64(1), &lnwire.UpdateFee{FeePerKw: 1000},
	))

	_, err = deserializeLogUpdates(bytes.NewReader(buf.Bytes()))
	require.ErrorContains(t, err, "log update count mismatch")
}
