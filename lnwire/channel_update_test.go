package lnwire

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestChannelUpdate(t *testing.T) {
	update := ChannelUpdate{
		ExtraOpaqueData: []byte{},
		TimeLockDelta:   144,
	}

	fmt.Println(update.serializedSize())

	var eob ExtraOpaqueData
	require.NoError(t, eob.PackRecords(&update))

	var extractedUpdate ChannelUpdate
	_, err := eob.ExtractRecords(&extractedUpdate)
	require.NoError(t, err)

	require.Equal(t, update, extractedUpdate)
}
