package channeldb

import (
	"bytes"
	"io"
	"math"
	"math/rand"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lntest/channels"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

const (
	// testType is used for creating a testing tlv record type. We use 10
	// here so it's easier to be recognized by its hex value, 0xa.
	testType tlv.Type = 10
)

var (
	// testValue is used for creating tlv record.
	testValue = uint8(255) // 0xff

	// testValueBytes is the tlv encoded testValue.
	testValueBytes = []byte{
		0x3,  // total length = 3
		0xa,  // type = 10
		0x1,  // length = 1
		0xff, // value = 255
	}

	testHTLCEntry = HTLCEntry{
		RefundTimeout: 740_000,
		OutputIndex:   10,
		Incoming:      true,
		Amt:           1000_000,
		amtTlv:        1000_000,
		incomingTlv:   1,
	}
	testHTLCEntryBytes = []byte{
		// Body length 23.
		0x16,
		// Rhash tlv.
		0x0, 0x0,
		// RefundTimeout tlv.
		0x1, 0x4, 0x0, 0xb, 0x4a, 0xa0,
		// OutputIndex tlv.
		0x2, 0x2, 0x0, 0xa,
		// Incoming tlv.
		0x3, 0x1, 0x1,
		// Amt tlv.
		0x4, 0x5, 0xfe, 0x0, 0xf, 0x42, 0x40,
	}

	localBalance  = lnwire.MilliSatoshi(9000)
	remoteBalance = lnwire.MilliSatoshi(3000)

	testChannelCommit = ChannelCommitment{
		CommitHeight:  999,
		LocalBalance:  localBalance,
		RemoteBalance: remoteBalance,
		CommitFee:     btcutil.Amount(rand.Int63()),
		FeePerKw:      btcutil.Amount(5000),
		CommitTx:      channels.TestFundingTx,
		CommitSig:     bytes.Repeat([]byte{1}, 71),
		Htlcs: []HTLC{{
			RefundTimeout: testHTLCEntry.RefundTimeout,
			OutputIndex:   int32(testHTLCEntry.OutputIndex),
			Incoming:      testHTLCEntry.Incoming,
			Amt: lnwire.NewMSatFromSatoshis(
				testHTLCEntry.Amt,
			),
		}},
	}

	testRevocationLogNoAmts = RevocationLog{
		OurOutputIndex:   0,
		TheirOutputIndex: 1,
		CommitTxHash:     testChannelCommit.CommitTx.TxHash(),
		HTLCEntries:      []*HTLCEntry{&testHTLCEntry},
	}
	testRevocationLogNoAmtsBytes = []byte{
		// Body length 42.
		0x2a,
		// OurOutputIndex tlv.
		0x0, 0x2, 0x0, 0x0,
		// TheirOutputIndex tlv.
		0x1, 0x2, 0x0, 0x1,
		// CommitTxHash tlv.
		0x2, 0x20,
		0x28, 0x76, 0x2, 0x59, 0x1d, 0x9d, 0x64, 0x86,
		0x6e, 0x60, 0x29, 0x23, 0x1d, 0x5e, 0xc5, 0xe6,
		0xbd, 0xf7, 0xd3, 0x9b, 0x16, 0x7d, 0x0, 0xff,
		0xc8, 0x22, 0x51, 0xb1, 0x5b, 0xa0, 0xbf, 0xd,
	}

	testRevocationLogWithAmts = RevocationLog{
		OurOutputIndex:   0,
		TheirOutputIndex: 1,
		CommitTxHash:     testChannelCommit.CommitTx.TxHash(),
		HTLCEntries:      []*HTLCEntry{&testHTLCEntry},
		OurBalance:       &localBalance,
		TheirBalance:     &remoteBalance,
	}
	testRevocationLogWithAmtsBytes = []byte{
		// Body length 52.
		0x34,
		// OurOutputIndex tlv.
		0x0, 0x2, 0x0, 0x0,
		// TheirOutputIndex tlv.
		0x1, 0x2, 0x0, 0x1,
		// CommitTxHash tlv.
		0x2, 0x20,
		0x28, 0x76, 0x2, 0x59, 0x1d, 0x9d, 0x64, 0x86,
		0x6e, 0x60, 0x29, 0x23, 0x1d, 0x5e, 0xc5, 0xe6,
		0xbd, 0xf7, 0xd3, 0x9b, 0x16, 0x7d, 0x0, 0xff,
		0xc8, 0x22, 0x51, 0xb1, 0x5b, 0xa0, 0xbf, 0xd,
		// OurBalance.
		0x3, 0x3, 0xfd, 0x23, 0x28,
		// Remote Balance.
		0x4, 0x3, 0xfd, 0x0b, 0xb8,
	}
)

func TestWriteTLVStream(t *testing.T) {
	t.Parallel()

	// Create a dummy tlv stream for testing.
	ts, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(testType, &testValue),
	)
	require.NoError(t, err)

	// Write the tlv stream.
	buf := bytes.NewBuffer([]byte{})
	err = writeTlvStream(buf, ts)
	require.NoError(t, err)

	// Check the bytes are written as expected.
	require.Equal(t, testValueBytes, buf.Bytes())
}

func TestReadTLVStream(t *testing.T) {
	t.Parallel()

	var valueRead uint8

	// Create a dummy tlv stream for testing.
	ts, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(testType, &valueRead),
	)
	require.NoError(t, err)

	// Read the tlv stream.
	buf := bytes.NewBuffer(testValueBytes)
	_, err = readTlvStream(buf, ts)
	require.NoError(t, err)

	// Check the bytes are read as expected.
	require.Equal(t, testValue, valueRead)
}

func TestReadTLVStreamErr(t *testing.T) {
	t.Parallel()

	var valueRead uint8

	// Create a dummy tlv stream for testing.
	ts, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(testType, &valueRead),
	)
	require.NoError(t, err)

	// Use empty bytes to cause an EOF.
	b := []byte{}

	// Read the tlv stream.
	buf := bytes.NewBuffer(b)
	_, err = readTlvStream(buf, ts)
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)

	// Check the bytes are not read.
	require.Zero(t, valueRead)
}

func TestSerializeHTLCEntriesEmptyRHash(t *testing.T) {
	t.Parallel()

	// Copy the testHTLCEntry.
	entry := testHTLCEntry

	// Set the internal fields to empty values so we can test the bytes are
	// padded.
	entry.incomingTlv = 0
	entry.amtTlv = 0

	// Write the tlv stream.
	buf := bytes.NewBuffer([]byte{})
	err := serializeHTLCEntries(buf, []*HTLCEntry{&entry})
	require.NoError(t, err)

	// Check the bytes are read as expected.
	require.Equal(t, testHTLCEntryBytes, buf.Bytes())
}

func TestSerializeHTLCEntries(t *testing.T) {
	t.Parallel()

	// Copy the testHTLCEntry.
	entry := testHTLCEntry

	// Create a fake rHash.
	rHashBytes := bytes.Repeat([]byte{10}, 32)
	copy(entry.RHash[:], rHashBytes)

	// Construct the serialized bytes.
	//
	// Exclude the first 3 bytes, which are total length, RHash type and
	// RHash length(0).
	partialBytes := testHTLCEntryBytes[3:]

	// Write the total length and RHash tlv.
	expectedBytes := []byte{0x36, 0x0, 0x20}
	expectedBytes = append(expectedBytes, rHashBytes...)

	// Append the rest.
	expectedBytes = append(expectedBytes, partialBytes...)

	buf := bytes.NewBuffer([]byte{})
	err := serializeHTLCEntries(buf, []*HTLCEntry{&entry})
	require.NoError(t, err)

	// Check the bytes are read as expected.
	require.Equal(t, expectedBytes, buf.Bytes())
}

// TestSerializeAndDeserializeRevLog tests the serialization and deserialization
// of various forms of the revocation log.
func TestSerializeAndDeserializeRevLog(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		revLog      RevocationLog
		revLogBytes []byte
	}{
		{
			name:        "with no amount fields",
			revLog:      testRevocationLogNoAmts,
			revLogBytes: testRevocationLogNoAmtsBytes,
		},
		{
			name:        "with amount fields",
			revLog:      testRevocationLogWithAmts,
			revLogBytes: testRevocationLogWithAmtsBytes,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			testSerializeRevocationLog(
				t, &test.revLog, test.revLogBytes,
			)

			testDerializeRevocationLog(
				t, &test.revLog, test.revLogBytes,
			)
		})
	}
}

func testSerializeRevocationLog(t *testing.T, rl *RevocationLog,
	revLogBytes []byte) {

	// Copy the testRevocationLogWithAmts and testHTLCEntry.
	htlc := testHTLCEntry
	rl.HTLCEntries = []*HTLCEntry{&htlc}

	// Write the tlv stream.
	buf := bytes.NewBuffer([]byte{})
	err := serializeRevocationLog(buf, rl)
	require.NoError(t, err)

	// Check the expected bytes on the body of the revocation log.
	bodyIndex := buf.Len() - len(testHTLCEntryBytes)
	require.Equal(t, revLogBytes, buf.Bytes()[:bodyIndex])
}

func testDerializeRevocationLog(t *testing.T, revLog *RevocationLog,
	revLogBytes []byte) {

	// Construct the full bytes.
	revLogBytes = append(revLogBytes, testHTLCEntryBytes...)

	// Read the tlv stream.
	buf := bytes.NewBuffer(revLogBytes)
	rl, err := deserializeRevocationLog(buf)
	require.NoError(t, err)

	// Check the bytes are read as expected.
	require.Len(t, rl.HTLCEntries, 1)
	require.Equal(t, *revLog, rl)
}

func TestDerializeHTLCEntriesEmptyRHash(t *testing.T) {
	t.Parallel()

	// Read the tlv stream.
	buf := bytes.NewBuffer(testHTLCEntryBytes)
	htlcs, err := deserializeHTLCEntries(buf)
	require.NoError(t, err)

	// Check the bytes are read as expected.
	require.Len(t, htlcs, 1)
	require.Equal(t, &testHTLCEntry, htlcs[0])
}

func TestDerializeHTLCEntries(t *testing.T) {
	t.Parallel()

	// Copy the testHTLCEntry.
	entry := testHTLCEntry

	// Create a fake rHash.
	rHashBytes := bytes.Repeat([]byte{10}, 32)
	copy(entry.RHash[:], rHashBytes)

	// Construct the serialized bytes.
	//
	// Exclude the first 3 bytes, which are total length, RHash type and
	// RHash length(0).
	partialBytes := testHTLCEntryBytes[3:]

	// Write the total length and RHash tlv.
	testBytes := append([]byte{0x36, 0x0, 0x20}, rHashBytes...)

	// Append the rest.
	testBytes = append(testBytes, partialBytes...)

	// Read the tlv stream.
	buf := bytes.NewBuffer(testBytes)
	htlcs, err := deserializeHTLCEntries(buf)
	require.NoError(t, err)

	// Check the bytes are read as expected.
	require.Len(t, htlcs, 1)
	require.Equal(t, &entry, htlcs[0])
}

func TestFetchLogBucket(t *testing.T) {
	t.Parallel()

	fullDB, err := MakeTestDB(t)
	require.NoError(t, err)

	backend := fullDB.ChannelStateDB().backend

	// Test that when neither of the buckets exists, an error is returned.
	err = kvdb.Update(backend, func(tx kvdb.RwTx) error {
		chanBucket, err := tx.CreateTopLevelBucket(openChannelBucket)
		require.NoError(t, err)

		// Check an error is returned when there's no sub bucket.
		_, err = fetchLogBucket(chanBucket)
		return err
	}, func() {})
	require.ErrorIs(t, err, ErrNoPastDeltas)

	// Test a successful fetch.
	err = kvdb.Update(backend, func(tx kvdb.RwTx) error {
		chanBucket, err := tx.CreateTopLevelBucket(openChannelBucket)
		require.NoError(t, err)

		_, err = chanBucket.CreateBucket(revocationLogBucket)
		require.NoError(t, err)

		// Check an error is returned when there's no sub bucket.
		_, err = fetchLogBucket(chanBucket)
		return err
	}, func() {})
	require.NoError(t, err)
}

func TestDeleteLogBucket(t *testing.T) {
	t.Parallel()

	fullDB, err := MakeTestDB(t)
	require.NoError(t, err)

	backend := fullDB.ChannelStateDB().backend

	err = kvdb.Update(backend, func(tx kvdb.RwTx) error {
		// Create the buckets.
		chanBucket, _, err := createTestRevocatoinLogBuckets(tx)
		require.NoError(t, err)

		// Create the buckets again should give us an error.
		_, _, err = createTestRevocatoinLogBuckets(tx)
		require.ErrorIs(t, err, kvdb.ErrBucketExists)

		// Delete both buckets.
		err = deleteLogBucket(chanBucket)
		require.NoError(t, err)

		// Create the buckets again should give us NO error.
		_, _, err = createTestRevocatoinLogBuckets(tx)
		return err
	}, func() {})
	require.NoError(t, err)
}

func TestPutRevocationLog(t *testing.T) {
	t.Parallel()

	// Create a test commit that has a large htlc output index.
	testHtlc := HTLC{OutputIndex: math.MaxUint16 + 1}
	testCommit := testChannelCommit
	testCommit.Htlcs = []HTLC{testHtlc}

	// Create a test commit that has a dust HTLC.
	testHtlcDust := HTLC{OutputIndex: -1}
	testCommitDust := testChannelCommit
	testCommitDust.Htlcs = append(testCommitDust.Htlcs, testHtlcDust)

	testCases := []struct {
		name        string
		commit      ChannelCommitment
		ourIndex    uint32
		theirIndex  uint32
		noAmtData   bool
		expectedErr error
		expectedLog RevocationLog
	}{
		{
			// Test a normal put operation.
			name:        "successful put with amount data",
			commit:      testChannelCommit,
			ourIndex:    0,
			theirIndex:  1,
			expectedErr: nil,
			expectedLog: testRevocationLogWithAmts,
		},
		{
			// Test a normal put operation.
			name:        "successful put with no amount data",
			commit:      testChannelCommit,
			ourIndex:    0,
			theirIndex:  1,
			noAmtData:   true,
			expectedErr: nil,
			expectedLog: testRevocationLogNoAmts,
		},
		{
			// Test our index too big.
			name:        "our index too big",
			commit:      testChannelCommit,
			ourIndex:    math.MaxUint16 + 1,
			theirIndex:  1,
			expectedErr: ErrOutputIndexTooBig,
			expectedLog: RevocationLog{},
		},
		{
			// Test their index too big.
			name:        "their index too big",
			commit:      testChannelCommit,
			ourIndex:    0,
			theirIndex:  math.MaxUint16 + 1,
			expectedErr: ErrOutputIndexTooBig,
			expectedLog: RevocationLog{},
		},
		{
			// Test htlc output index too big.
			name:        "htlc index too big",
			commit:      testCommit,
			ourIndex:    0,
			theirIndex:  1,
			expectedErr: ErrOutputIndexTooBig,
			expectedLog: RevocationLog{},
		},
		{
			// Test dust htlc is not saved.
			name:        "dust htlc not saved with amout data",
			commit:      testCommitDust,
			ourIndex:    0,
			theirIndex:  1,
			expectedErr: nil,
			expectedLog: testRevocationLogWithAmts,
		},
		{
			// Test dust htlc is not saved.
			name:        "dust htlc not saved with no amount data",
			commit:      testCommitDust,
			ourIndex:    0,
			theirIndex:  1,
			noAmtData:   true,
			expectedErr: nil,
			expectedLog: testRevocationLogNoAmts,
		},
	}

	for _, tc := range testCases {
		tc := tc

		fullDB, err := MakeTestDB(t)
		require.NoError(t, err)

		backend := fullDB.ChannelStateDB().backend

		// Construct the testing db transaction.
		dbTx := func(tx kvdb.RwTx) (RevocationLog, error) {
			// Create the buckets.
			_, bucket, err := createTestRevocatoinLogBuckets(tx)
			require.NoError(t, err)

			// Save the log.
			err = putRevocationLog(
				bucket, &tc.commit, tc.ourIndex, tc.theirIndex,
				tc.noAmtData,
			)
			if err != nil {
				return RevocationLog{}, err
			}

			// Read the saved log.
			return fetchRevocationLog(
				bucket, tc.commit.CommitHeight,
			)
		}

		t.Run(tc.name, func(t *testing.T) {
			var rl RevocationLog
			err := kvdb.Update(backend, func(tx kvdb.RwTx) error {
				record, err := dbTx(tx)
				rl = record
				return err
			}, func() {})

			require.Equal(t, tc.expectedErr, err)
			require.Equal(t, tc.expectedLog, rl)
		})
	}
}

func TestFetchRevocationLogCompatible(t *testing.T) {
	t.Parallel()

	knownHeight := testChannelCommit.CommitHeight
	unknownHeight := knownHeight + 1
	logKey := makeLogKey(knownHeight)

	testCases := []struct {
		name         string
		updateNum    uint64
		expectedErr  error
		createRl     bool
		createCommit bool
		expectRl     bool
		expectCommit bool
	}{
		{
			// Test we can fetch the new log.
			name:        "fetch new log",
			updateNum:   knownHeight,
			expectedErr: nil,
			createRl:    true,
			expectRl:    true,
		},
		{
			// Test we can fetch the legacy log.
			name:         "fetch legacy log",
			updateNum:    knownHeight,
			expectedErr:  nil,
			createCommit: true,
			expectCommit: true,
		},
		{
			// Test we only fetch the new log when both logs exist.
			name:         "fetch new log only",
			updateNum:    knownHeight,
			expectedErr:  nil,
			createRl:     true,
			createCommit: true,
			expectRl:     true,
		},
		{
			// Test no past deltas when the buckets do not exist.
			name:        "no buckets created",
			updateNum:   unknownHeight,
			expectedErr: ErrNoPastDeltas,
		},
		{
			// Test no logs found when the height is unknown.
			name:         "no log found",
			updateNum:    unknownHeight,
			expectedErr:  ErrLogEntryNotFound,
			createRl:     true,
			createCommit: true,
		},
	}

	for _, tc := range testCases {
		tc := tc

		fullDB, err := MakeTestDB(t)
		require.NoError(t, err)

		backend := fullDB.ChannelStateDB().backend

		var (
			rl     *RevocationLog
			commit *ChannelCommitment
		)

		// Setup the buckets and fill the test data if specified.
		err = kvdb.Update(backend, func(tx kvdb.RwTx) error {
			// Create the root bucket.
			cb, err := tx.CreateTopLevelBucket(openChannelBucket)
			require.NoError(t, err)

			// Create the revocation log if specified.
			if tc.createRl {
				lb, err := cb.CreateBucket(revocationLogBucket)
				require.NoError(t, err)

				err = putRevocationLog(
					lb, &testChannelCommit, 0, 1, false,
				)
				require.NoError(t, err)
			}

			// Create the channel commit if specified.
			if tc.createCommit {
				legacyBucket, err := cb.CreateBucket(
					revocationLogBucketDeprecated,
				)
				require.NoError(t, err)

				buf := bytes.NewBuffer([]byte{})
				err = serializeChanCommit(
					buf, &testChannelCommit,
				)
				require.NoError(t, err)

				err = legacyBucket.Put(logKey[:], buf.Bytes())
				require.NoError(t, err)
			}

			return nil
		}, func() {})

		// Construct the testing db transaction.
		dbTx := func(tx kvdb.RTx) error {
			cb := tx.ReadBucket(openChannelBucket)

			rl, commit, err = fetchRevocationLogCompatible(
				cb, tc.updateNum,
			)
			return err
		}

		t.Run(tc.name, func(t *testing.T) {
			err := kvdb.View(backend, dbTx, func() {})
			require.Equal(t, tc.expectedErr, err)

			// Check the expected revocation log is returned.
			if tc.expectRl {
				require.NotNil(t, rl)
			} else {
				require.Nil(t, rl)
			}

			// Check the expected channel commit is returned.
			if tc.expectCommit {
				require.NotNil(t, commit)
			} else {
				require.Nil(t, commit)
			}
		})
	}
}

func createTestRevocatoinLogBuckets(tx kvdb.RwTx) (kvdb.RwBucket,
	kvdb.RwBucket, error) {

	chanBucket, err := tx.CreateTopLevelBucket(openChannelBucket)
	if err != nil {
		return nil, nil, err
	}

	logBucket, err := chanBucket.CreateBucket(revocationLogBucket)
	if err != nil {
		return nil, nil, err
	}

	_, err = chanBucket.CreateBucket(revocationLogBucketDeprecated)
	if err != nil {
		return nil, nil, err
	}

	return chanBucket, logBucket, nil
}
