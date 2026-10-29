package channeldb

import (
	"testing"

	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// writeTestChannelResponse writes a forwarding package for the given channel
// holding a single unacknowledged settle, and returns it.
func writeTestChannelResponse(t *testing.T, cdb *ChannelStateDB,
	ch *OpenChannel) *lnwire.UpdateFulfillHTLC {

	t.Helper()

	settle := &lnwire.UpdateFulfillHTLC{
		ID:              3,
		PaymentPreimage: [32]byte{9},
	}

	packager := NewChannelPackager(ch.ShortChanID())
	err := kvdb.Update(cdb.backend, func(tx kvdb.RwTx) error {
		pkg := NewFwdPkg(ch.ShortChanID(), 0, nil, []LogUpdate{
			{LogIndex: 0, UpdateMsg: settle},
		})

		return packager.AddFwdPkg(tx, pkg)
	}, func() {})
	require.NoError(t, err)

	return settle
}

// TestCloseChannelPersistsResponses asserts that both channel-close strategies
// retain eligible responses. The synchronous path must also remove the
// forwarding package in the same transaction.
func TestCloseChannelPersistsResponses(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		tombstone bool
	}{
		{
			name: "synchronous",
		},
		{
			name:      "tombstone",
			tombstone: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			dbOption := OptionTombstoneClosedChannels(
				test.tombstone,
			)
			fullDB, err := MakeTestDB(t, dbOption)
			require.NoError(t, err)

			cdb := fullDB.ChannelStateDB()
			ch := createTestChannel(t, cdb, openChannelOption())
			settle := writeTestChannelResponse(t, cdb, ch)

			closeChannelForTest(t, cdb, ch)

			if !test.tombstone {
				packager := NewChannelPackager(ch.ShortChanID())
				fwdPkgs := loadFwdPkgs(t, cdb.backend, packager)
				require.Empty(t, fwdPkgs)
			}

			responses, err := fullDB.FwdResponseStore().
				FetchFwdResponses()
			require.NoError(t, err)
			require.Len(t, responses, 1)
			response := responses[0]
			require.Equal(
				t, ch.ShortChanID(), response.OutKey.ChanID,
			)
			require.Equal(t, settle.ID, response.OutKey.HtlcID)

			msg := response.Message
			persisted, ok := msg.(*lnwire.UpdateFulfillHTLC)
			require.True(t, ok)
			require.Equal(
				t, settle.PaymentPreimage,
				persisted.PaymentPreimage,
			)
		})
	}
}
