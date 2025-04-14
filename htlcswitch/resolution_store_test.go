package htlcswitch

import (
	"path/filepath"
	"testing"

	"github.com/lightningnetwork/lnd/contractcourt"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// TestInsertAndDelete tests that an inserted resolution message can be
// deleted.
func TestInsertAndDelete(t *testing.T) {
	t.Parallel()

	scid := lnwire.NewShortChanIDFromInt(1)

	failResMsg := &contractcourt.ResolutionMsg{
		SourceChan: scid,
		HtlcIndex:  2,
		Failure:    &lnwire.FailTemporaryChannelFailure{},
	}

	settleBytes := [32]byte{
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01,
	}

	settleResMsg := &contractcourt.ResolutionMsg{
		SourceChan: scid,
		HtlcIndex:  3,
		PreImage:   &settleBytes,
	}

	// Create the backend database and use it to create the resolution
	// store.
	dbPath := filepath.Join(t.TempDir(), "testdb")
	db, err := kvdb.Create(
		kvdb.BoltBackendName, dbPath, true, kvdb.DefaultDBTimeout,
		false,
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		db.Close()
	})

	resStore := newResolutionStore(db)

	// We'll add the failure resolution message first, then check that it
	// exists in the store.
	err = resStore.addResolutionMsg(failResMsg)
	require.NoError(t, err)

	// Assert that checkResolutionMsg returns nil, signalling that the
	// resolution message was properly stored.
	outKey := &CircuitKey{
		ChanID: failResMsg.SourceChan,
		HtlcID: failResMsg.HtlcIndex,
	}
	err = resStore.checkResolutionMsg(outKey)
	require.NoError(t, err)

	resMsgs, err := resStore.fetchAllResolutionMsg()
	require.NoError(t, err)
	require.Equal(t, 1, len(resMsgs))

	// It should match failResMsg above.
	require.Equal(t, failResMsg.SourceChan, resMsgs[0].SourceChan)
	require.Equal(t, failResMsg.HtlcIndex, resMsgs[0].HtlcIndex)
	require.NotNil(t, resMsgs[0].Failure)
	require.Nil(t, resMsgs[0].PreImage)

	// We'll add the settleResMsg now.
	err = resStore.addResolutionMsg(settleResMsg)
	require.NoError(t, err)

	// Check that checkResolutionMsg returns nil for the settle CircuitKey.
	outKey.ChanID = settleResMsg.SourceChan
	outKey.HtlcID = settleResMsg.HtlcIndex
	err = resStore.checkResolutionMsg(outKey)
	require.NoError(t, err)

	// We should have two resolution messages in the store, one failure and
	// one success.
	resMsgs, err = resStore.fetchAllResolutionMsg()
	require.NoError(t, err)
	require.Equal(t, 2, len(resMsgs))

	// The first resolution message should be the failure.
	require.Equal(t, failResMsg.SourceChan, resMsgs[0].SourceChan)
	require.Equal(t, failResMsg.HtlcIndex, resMsgs[0].HtlcIndex)
	require.NotNil(t, resMsgs[0].Failure)
	require.Nil(t, resMsgs[0].PreImage)

	// The second resolution message should be the success.
	require.Equal(t, settleResMsg.SourceChan, resMsgs[1].SourceChan)
	require.Equal(t, settleResMsg.HtlcIndex, resMsgs[1].HtlcIndex)
	require.Nil(t, resMsgs[1].Failure)
	require.Equal(t, settleBytes, *resMsgs[1].PreImage)

	// We'll now delete the failure resolution message and assert that only
	// the success is left.
	failKey := &CircuitKey{
		ChanID: scid,
		HtlcID: failResMsg.HtlcIndex,
	}

	err = resStore.deleteResolutionMsg(failKey)
	require.NoError(t, err)

	// Assert that checkResolutionMsg returns errResMsgNotFound.
	err = resStore.checkResolutionMsg(failKey)
	require.ErrorIs(t, err, errResMsgNotFound)

	resMsgs, err = resStore.fetchAllResolutionMsg()
	require.NoError(t, err)
	require.Equal(t, 1, len(resMsgs))

	// Assert that the success is left.
	require.Equal(t, settleResMsg.SourceChan, resMsgs[0].SourceChan)
	require.Equal(t, settleResMsg.HtlcIndex, resMsgs[0].HtlcIndex)
	require.Nil(t, resMsgs[0].Failure)
	require.Equal(t, settleBytes, *resMsgs[0].PreImage)

	// Now we'll delete the settle resolution message and assert that the
	// store is empty.
	settleKey := &CircuitKey{
		ChanID: scid,
		HtlcID: settleResMsg.HtlcIndex,
	}

	err = resStore.deleteResolutionMsg(settleKey)
	require.NoError(t, err)

	// Assert that checkResolutionMsg returns errResMsgNotFound for the
	// settle key.
	err = resStore.checkResolutionMsg(settleKey)
	require.ErrorIs(t, err, errResMsgNotFound)

	resMsgs, err = resStore.fetchAllResolutionMsg()
	require.NoError(t, err)
	require.Equal(t, 0, len(resMsgs))
}
