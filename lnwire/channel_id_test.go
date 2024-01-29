package lnwire

import "testing"

// TestChannelIDOutPointConversion ensures that the IsChanPoint always
// recognizes its seed OutPoint for all possible values of an output index.
func TestChannelIDOutPointConversion(t *testing.T) {
	t.Parallel()

	testChanPoint := *outpoint1

	// For a given OutPoint, we'll run through all the possible output
	// index values, mutating our test outpoint to match that output index.
	var prevChanID ChannelID
	for i := uint32(0); i < MaxFundingTxOutputs; i++ {
		testChanPoint.Index = i

		// With the output index mutated, we'll convert it into a
		// ChannelID.
		cid := NewChanIDFromOutPoint(testChanPoint)

		// Once the channel point has been converted to a channelID, it
		// should recognize its original outpoint.
		if !cid.IsChanPoint(&testChanPoint) {
			t.Fatalf("channelID not recognized as seed channel "+
				"point: cid=%v, op=%v", cid, testChanPoint)
		}

		// We also ensure that the channel ID itself have changed
		// between iterations. This case is meant to catch an issue
		// where the transformation function itself is a no-op.
		if prevChanID == cid {
			t.Fatalf("#%v: channelID not modified: old=%v, new=%v",
				i, prevChanID, cid)
		}

		prevChanID = cid
	}
}

// TestGenPossibleOutPoints ensures that the GenPossibleOutPoints generates a
// valid set of outpoints for a channelID. A set of outpoints is valid iff, the
// root outpoint (the outpoint that generated the ChannelID) is included in the
// returned set of outpoints.
func TestGenPossibleOutPoints(t *testing.T) {
	t.Parallel()

	// We'll first convert out test outpoint into a ChannelID.
	testChanPoint := *outpoint1
	testChanPoint.Index = 24
	chanID := NewChanIDFromOutPoint(testChanPoint)

	// With the chan ID created, we'll generate all possible root outpoints
	// given this channel ID.
	possibleOutPoints := chanID.GenPossibleOutPoints()

	// If we run through the generated possible outpoints, the original
	// root outpoint MUST be found in this generated set.
	var opFound bool
	for _, op := range possibleOutPoints {
		if op == testChanPoint {
			opFound = true
			break
		}
	}

	// If we weren't able to locate the original outpoint in the set of
	// possible outpoints, then we'll fail the test.
	if !opFound {
		t.Fatalf("possible outpoints did not contain the root outpoint")
	}
}
