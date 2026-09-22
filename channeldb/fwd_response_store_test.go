package channeldb

import (
	"bytes"
	"testing"

	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// testFwdSource is the short channel ID used as the outgoing channel in the
// forwarding response store tests.
var testFwdSource = lnwire.NewShortChanIDFromInt(42)

// addTestFwdPkg writes a forwarding package holding the given settle/fail
// updates for the test source channel and returns it.
func addTestFwdPkg(t *testing.T, cdb *ChannelStateDB, height uint64,
	settleFails []LogUpdate) *FwdPkg {

	t.Helper()

	fwdPkg := NewFwdPkg(testFwdSource, height, nil, settleFails)

	packager := NewChannelPackager(testFwdSource)
	err := kvdb.Update(cdb.backend, func(tx kvdb.RwTx) error {
		return packager.AddFwdPkg(tx, fwdPkg)
	}, func() {})
	require.NoError(t, err, "unable to add fwd pkg")

	return fwdPkg
}

// extractTestFwdResponses runs the extraction for the test source channel.
func extractTestFwdResponses(t *testing.T, cdb *ChannelStateDB) {
	t.Helper()

	err := kvdb.Update(cdb.backend, func(tx kvdb.RwTx) error {
		return extractFwdResponses(tx, testFwdSource)
	}, func() {})
	require.NoError(t, err, "unable to extract responses")
}

// TestFwdResponseStoreRoundTrip asserts that unacknowledged settles and fails
// are recovered from a channel's forwarding packages, keyed by their outgoing
// circuit key, and that the exact wire message survives the round trip.
func TestFwdResponseStoreRoundTrip(t *testing.T) {
	t.Parallel()

	fullDB, err := MakeTestDB(t)
	require.NoError(t, err)
	cdb := fullDB.ChannelStateDB()

	// The settle carries a custom record, and the fail carries a specific
	// reason. Both must survive: a reconstructed response would lose them.
	customRecords := lnwire.CustomRecords{
		uint64(lnwire.MinCustomRecordsTlvType): []byte{1, 2, 3},
	}
	settle := &lnwire.UpdateFulfillHTLC{
		ChanID:          lnwire.ChannelID{2},
		ID:              7,
		PaymentPreimage: [32]byte{1},
		CustomRecords:   customRecords,
		ExtraData:       lnwire.ExtraOpaqueData{1, 1, 7},
	}
	fail := &lnwire.UpdateFailHTLC{
		ChanID:    lnwire.ChannelID{3},
		ID:        9,
		Reason:    bytes.Repeat([]byte{4}, 292),
		ExtraData: lnwire.ExtraOpaqueData{3, 1, 8},
	}

	var settleWire, failWire bytes.Buffer
	require.NoError(t, serializeFwdResponse(&settleWire, settle))
	require.NoError(t, serializeFwdResponse(&failWire, fail))

	addTestFwdPkg(t, cdb, 1, []LogUpdate{
		{LogIndex: 0, UpdateMsg: settle},
		{LogIndex: 1, UpdateMsg: fail},
	})

	extractTestFwdResponses(t, cdb)

	responses, err := cdb.FetchFwdResponses()
	require.NoError(t, err)
	require.Len(t, responses, 2)

	// Index the results by HTLC ID so the assertions do not depend on the
	// store's iteration order.
	byID := make(map[uint64]*FwdResponse)
	for _, response := range responses {
		require.Equal(t, testFwdSource, response.OutKey.ChanID)
		byID[response.OutKey.HtlcID] = response
	}

	gotSettle, ok := byID[settle.ID].Message.(*lnwire.UpdateFulfillHTLC)
	require.True(t, ok, "expected a settle for HTLC %d", settle.ID)
	require.Equal(t, settle, gotSettle)
	var gotSettleWire bytes.Buffer
	require.NoError(t, serializeFwdResponse(&gotSettleWire, gotSettle))
	require.Equal(t, settleWire.Bytes(), gotSettleWire.Bytes())

	gotFail, ok := byID[fail.ID].Message.(*lnwire.UpdateFailHTLC)
	require.True(t, ok, "expected a fail for HTLC %d", fail.ID)
	require.Equal(t, fail, gotFail)
	var gotFailWire bytes.Buffer
	require.NoError(t, serializeFwdResponse(&gotFailWire, gotFail))
	require.Equal(t, failWire.Bytes(), gotFailWire.Bytes())

	// A retained response must be visible to circuit cleanup, and a clean
	// miss must be distinguishable from a storage failure.
	settleKey := models.CircuitKey{
		ChanID: testFwdSource,
		HtlcID: settle.ID,
	}
	require.NoError(t, cdb.CheckFwdResponse(&settleKey))

	unknownKey := models.CircuitKey{
		ChanID: testFwdSource,
		HtlcID: 1000,
	}
	require.ErrorIs(
		t, cdb.CheckFwdResponse(&unknownKey), ErrFwdResponseNotFound,
	)

	require.NoError(t, cdb.DeleteFwdResponse(&settleKey))
	require.ErrorIs(
		t, cdb.CheckFwdResponse(&settleKey), ErrFwdResponseNotFound,
	)

	responses, err = cdb.FetchFwdResponses()
	require.NoError(t, err)
	require.Len(t, responses, 1)
	require.Equal(t, fail.ID, responses[0].OutKey.HtlcID)
}

// TestFwdResponseStoreSkipsAcked asserts that responses the incoming link has
// already locked in are not retained, since they need no recovery.
func TestFwdResponseStoreSkipsAcked(t *testing.T) {
	t.Parallel()

	fullDB, err := MakeTestDB(t)
	require.NoError(t, err)
	cdb := fullDB.ChannelStateDB()

	acked := &lnwire.UpdateFulfillHTLC{ID: 1}
	pending := &lnwire.UpdateFulfillHTLC{ID: 2}

	fwdPkg := addTestFwdPkg(t, cdb, 1, []LogUpdate{
		{LogIndex: 0, UpdateMsg: acked},
		{LogIndex: 1, UpdateMsg: pending},
	})

	// Ack the first response the same way the incoming link would.
	packager := NewChannelPackager(testFwdSource)
	err = kvdb.Update(cdb.backend, func(tx kvdb.RwTx) error {
		return packager.AckSettleFails(tx, SettleFailRef{
			Source: testFwdSource,
			Height: fwdPkg.Height,
			Index:  0,
		})
	}, func() {})
	require.NoError(t, err)

	extractTestFwdResponses(t, cdb)

	responses, err := cdb.FetchFwdResponses()
	require.NoError(t, err)
	require.Len(t, responses, 1)
	require.Equal(t, pending.ID, responses[0].OutKey.HtlcID)
}

// TestFwdResponseStoreSkipsUnknownUpdate asserts that an update which
// addresses no outgoing HTLC is skipped during extraction.
func TestFwdResponseStoreSkipsUnknownUpdate(t *testing.T) {
	t.Parallel()

	fullDB, err := MakeTestDB(t)
	require.NoError(t, err)
	cdb := fullDB.ChannelStateDB()

	// An Add in the settle/fail list resolves nothing and must not be
	// mistaken for a response.
	addTestFwdPkg(t, cdb, 1, []LogUpdate{
		{LogIndex: 0, UpdateMsg: &lnwire.UpdateAddHTLC{ID: 1}},
	})

	extractTestFwdResponses(t, cdb)

	responses, err := cdb.FetchFwdResponses()
	require.NoError(t, err)
	require.Empty(t, responses)
}

// TestSerializeFwdResponseRejectsUnknown asserts that the serializer refuses
// any message that is not a settle or fail.
func TestSerializeFwdResponseRejectsUnknown(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	err := serializeFwdResponse(&buf, &lnwire.UpdateAddHTLC{})
	require.ErrorIs(t, err, ErrUnknownFwdResponse)
}

// TestFetchFwdResponsesRejectsUnknownMessage asserts that an unexpected
// stored message is reported rather than returned as a response.
func TestFetchFwdResponsesRejectsUnknownMessage(t *testing.T) {
	t.Parallel()

	fullDB, err := MakeTestDB(t)
	require.NoError(t, err)
	cdb := fullDB.ChannelStateDB()

	outKey := models.CircuitKey{ChanID: testFwdSource, HtlcID: 3}
	err = kvdb.Update(cdb.backend, func(tx kvdb.RwTx) error {
		var raw bytes.Buffer
		_, err := lnwire.WriteMessage(
			&raw, &lnwire.UpdateAddHTLC{}, 0,
		)
		require.NoError(t, err)

		bucket, err := tx.CreateTopLevelBucket(fwdResponseBucketKey)
		require.NoError(t, err)

		return bucket.Put(outKey.Bytes(), raw.Bytes())
	}, func() {})
	require.NoError(t, err)

	_, err = cdb.FetchFwdResponses()
	require.ErrorIs(t, err, ErrUnknownFwdResponse)
}

// TestFetchFwdResponsesRejectsInvalidKey asserts that a malformed key is
// rejected instead of being decoded from a valid prefix.
func TestFetchFwdResponsesRejectsInvalidKey(t *testing.T) {
	t.Parallel()

	fullDB, err := MakeTestDB(t)
	require.NoError(t, err)
	cdb := fullDB.ChannelStateDB()

	outKey := models.CircuitKey{ChanID: testFwdSource, HtlcID: 4}
	err = kvdb.Update(cdb.backend, func(tx kvdb.RwTx) error {
		var raw bytes.Buffer
		err := serializeFwdResponse(
			&raw, &lnwire.UpdateFulfillHTLC{ID: 4},
		)
		require.NoError(t, err)

		bucket, err := tx.CreateTopLevelBucket(fwdResponseBucketKey)
		require.NoError(t, err)

		invalidKey := append(outKey.Bytes(), 0)

		return bucket.Put(invalidKey, raw.Bytes())
	}, func() {})
	require.NoError(t, err)

	_, err = cdb.FetchFwdResponses()
	require.ErrorIs(t, err, models.ErrInvalidCircuitKeyLen)
}
