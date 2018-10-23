// +build !rpctest

package main

import (
	"io/ioutil"
	"os"
	"reflect"
	"testing"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/channeldb"
)

// makeTestDB creates a new instance of the ChannelDB for testing purposes. A
// callback which cleans up the created temporary directories is also returned
// and intended to be executed after the test completes.
func makeTestDB() (*channeldb.DB, func(), error) {
	// First, create a temporary directory to be used for the duration of
	// this test.
	tempDirName, err := ioutil.TempDir("", "channeldb")
	if err != nil {
		return nil, nil, err
	}

	// Next, create channeldb for the first time.
	cdb, err := channeldb.Open(tempDirName)
	if err != nil {
		return nil, nil, err
	}

	cleanUp := func() {
		cdb.Close()
		os.RemoveAll(tempDirName)
	}

	return cdb, cleanUp, nil
}

type incubateTest struct {
	nOutputs    int
	chanPoint   *wire.OutPoint
	commOutput  *kidOutput
	htlcOutputs []babyOutput
	err         error
}

// incubateTests holds the test vectors used to test the state transitions of
// outputs stored in the nursery store.
var incubateTests []incubateTest

// initIncubateTests instantiates the test vectors during package init, which
// properly captures the sign descriptors and public keys.
func initIncubateTests() {
	incubateTests = []incubateTest{
		{
			nOutputs:  0,
			chanPoint: &outPoints[3],
		},
		{
			nOutputs:   1,
			chanPoint:  &outPoints[0],
			commOutput: &kidOutputs[0],
		},
		{
			nOutputs:    4,
			chanPoint:   &outPoints[0],
			commOutput:  &kidOutputs[0],
			htlcOutputs: babyOutputs,
		},
	}

}

// TestNurseryStoreInit verifies basic properties of the nursery store before
// any modifying calls are made.
func TestNurseryStoreInit(t *testing.T) {
	cdb, cleanUp, err := makeTestDB()
	if err != nil {
		t.Fatalf("unable to open channel db: %v", err)
	}
	defer cleanUp()

	ns, err := newNurseryStore(&bitcoinTestnetGenesis, cdb)
	if err != nil {
		t.Fatalf("unable to open nursery store: %v", err)
	}

	assertNumChannels(t, ns, 0)
	assertNumPreschools(t, ns, 0)
}

// TestNurseryStoreIncubate tests the primary state transitions taken by outputs
// in the nursery store. The test is designed to walk both commitment or htlc
// outputs through the nursery store, verifying the properties of the
// intermediate states.
func TestNurseryStoreIncubate(t *testing.T) {
	cdb, cleanUp, err := makeTestDB()
	if err != nil {
		t.Fatalf("unable to open channel db: %v", err)
	}
	defer cleanUp()

	ns, err := newNurseryStore(&bitcoinTestnetGenesis, cdb)
	if err != nil {
		t.Fatalf("unable to open nursery store: %v", err)
	}

	for i, test := range incubateTests {
		// At the beginning of each test, we do not expect to the
		// nursery store to be tracking any outputs for this channel
		// point.
		assertNumChanOutputs(t, ns, test.chanPoint, 0)

		// Nursery store should be completely empty.
		assertNumChannels(t, ns, 0)
		assertNumPreschools(t, ns, 0)

		// Begin incubating all of the outputs provided in this test
		// vector.
		var kids []kidOutput
		if test.commOutput != nil {
			kids = append(kids, *test.commOutput)
		}
		err = ns.Incubate(kids, test.htlcOutputs)
		if err != nil {
			t.Fatalf("unable to incubate outputs"+
				"on test #%d: %v", i, err)
		}
		// Now that the outputs have been inserted, the nursery store
		// should see exactly that many outputs under this channel
		// point.
		// NOTE: This property should remain intact after every state
		// change until the channel has been completely removed.
		assertNumChanOutputs(t, ns, test.chanPoint, test.nOutputs)

		// If there were no inputs to be incubated, just check that the
		// no trace of the channel was left.
		if test.nOutputs == 0 {
			assertNumChannels(t, ns, 0)
			continue
		}

		// The test vector has a non-zero number of outputs, we will
		// expect to only see the one channel from this test case.
		assertNumChannels(t, ns, 1)

		// The channel should be shown as immature, since none of the
		// outputs should be graduated directly after being inserted.
		// It should also be impossible to remove the channel, if it is
		// also immature.
		// NOTE: These two tests properties should hold between every
		// state change until all outputs have been fully graduated.
		assertChannelMaturity(t, ns, test.chanPoint, false)
		assertCanRemoveChannel(t, ns, test.chanPoint, false)

		// Verify that the htlc outputs, if any, reside in the height
		// index at their first stage CLTV's expiry height.
		for _, htlcOutput := range test.htlcOutputs {
			assertCribAtExpiryHeight(t, ns, &htlcOutput)
		}

		// If the commitment output was not dust, we will move it from
		// the preschool bucket to the kindergarten bucket.
		if test.commOutput != nil {
			// If the commitment output was not considered dust, we
			// should see exactly one preschool output in the
			// nursery store.
			assertNumPreschools(t, ns, 1)

			// Now, move the commitment output to the kindergarten
			// bucket.
			err = ns.PreschoolToKinder(test.commOutput, 0)
			if err != test.err {
				t.Fatalf("unable to move commitment output from "+
					"pscl to kndr: %v", err)
			}

			// The total number of outputs for this channel should
			// not have changed, and the kindergarten output should
			// reside at its maturity height.
			assertNumChanOutputs(t, ns, test.chanPoint, test.nOutputs)
			assertKndrAtMaturityHeight(t, ns, test.commOutput)

			// The total number of channels should not have changed.
			assertNumChannels(t, ns, 1)

			// Channel maturity and removal should reflect that the
			// channel still has non-graduated outputs.
			assertChannelMaturity(t, ns, test.chanPoint, false)
			assertCanRemoveChannel(t, ns, test.chanPoint, false)

			// Moving the preschool output should have no effect on
			// the placement of crib outputs in the height index.
			for _, htlcOutput := range test.htlcOutputs {
				assertCribAtExpiryHeight(t, ns, &htlcOutput)
			}
		}

		// At this point, we should see no more preschool outputs in the
		// nursery store. Either it was moved to the kindergarten
		// bucket, or never inserted.
		assertNumPreschools(t, ns, 0)

		// If the commitment output is not-dust, we will graduate the
		// class at its maturity height.
		if test.commOutput != nil {
			// Compute the commitment output's maturity height, and
			// move proceed to graduate that class.
			maturityHeight := test.commOutput.ConfHeight() +
				test.commOutput.BlocksToMaturity()

			err = ns.GraduateKinder(maturityHeight, test.commOutput)
			if err != nil {
				t.Fatalf("unable to graduate kindergarten class at "+
					"height %d: %v", maturityHeight, err)
			}

			// The total number of outputs for this channel should
			// not have changed, but the kindergarten output should
			// have been removed from its maturity height.
			assertNumChanOutputs(t, ns, test.chanPoint, test.nOutputs)
			assertKndrNotAtMaturityHeight(t, ns, test.commOutput)

			// The total number of channels should not have changed.
			assertNumChannels(t, ns, 1)

			// Moving the preschool output should have no effect on
			// the placement of crib outputs in the height index.
			for _, htlcOutput := range test.htlcOutputs {
				assertCribAtExpiryHeight(t, ns, &htlcOutput)
			}
		}

		// If there are any htlc outputs to incubate, we will walk them
		// through their two-stage incubation process.
		if len(test.htlcOutputs) > 0 {
			for i, htlcOutput := range test.htlcOutputs {
				// Begin by moving each htlc output from the
				// crib to kindergarten state.
				err = ns.CribToKinder(&htlcOutput)
				if err != nil {
					t.Fatalf("unable to move htlc output from "+
						"crib to kndr: %v", err)
				}
				// Number of outputs for this channel should
				// remain unchanged.
				assertNumChanOutputs(t, ns, test.chanPoint,
					test.nOutputs)

				// If the output hasn't moved to kndr, it should
				// be at its crib expiry height, otherwise is
				// should have been removed.
				for j := range test.htlcOutputs {
					if j > i {
						assertCribAtExpiryHeight(t, ns,
							&test.htlcOutputs[j])
						assertKndrNotAtMaturityHeight(t,
							ns, &test.htlcOutputs[j].kidOutput)
					} else {
						assertCribNotAtExpiryHeight(t, ns,
							&test.htlcOutputs[j])
						assertKndrAtMaturityHeight(t,
							ns, &test.htlcOutputs[j].kidOutput)
					}
				}
			}

			// Total number of channels in the nursery store should
			// be the same, no outputs should be marked as
			// preschool.
			assertNumChannels(t, ns, 1)
			assertNumPreschools(t, ns, 0)

			// Channel should also not be mature, as it we should
			// still have outputs in kindergarten.
			assertChannelMaturity(t, ns, test.chanPoint, false)
			assertCanRemoveChannel(t, ns, test.chanPoint, false)

			// Now, graduate each htlc kindergarten output,
			// asserting the invariant number of outputs being
			// tracked in this channel
			for _, htlcOutput := range test.htlcOutputs {
				maturityHeight := htlcOutput.ConfHeight() +
					htlcOutput.BlocksToMaturity()

				err = ns.GraduateKinder(maturityHeight,
					&htlcOutput.kidOutput)
				if err != nil {
					t.Fatalf("unable to graduate htlc output "+
						"from kndr to grad: %v", err)
				}
				assertNumChanOutputs(t, ns, test.chanPoint,
					test.nOutputs)
			}
		}

		// All outputs have been advanced through the nursery store, but
		// no attempt has been made to clean up this channel. We expect
		// to see the same channel remaining, and no kindergarten
		// outputs.
		assertNumChannels(t, ns, 1)
		assertNumPreschools(t, ns, 0)

		// Since all outputs have now been graduated, the nursery store
		// should recognize that the channel is mature, and attempting
		// to remove it should succeed.
		assertChannelMaturity(t, ns, test.chanPoint, true)
		assertCanRemoveChannel(t, ns, test.chanPoint, true)

		// Now that the channel has been removed, the nursery store
		// should be no channels in the nursery store, and no outputs
		// being tracked for this channel point.
		assertNumChannels(t, ns, 0)
		assertNumChanOutputs(t, ns, test.chanPoint, 0)

		// If we had a commitment output, ensure it was removed from the
		// height index.
		if test.commOutput != nil {
			assertKndrNotAtMaturityHeight(t, ns, test.commOutput)
		}

		// Check that all htlc outputs are no longer stored in their
		// crib or kindergarten height buckets.
		for _, htlcOutput := range test.htlcOutputs {
			assertCribNotAtExpiryHeight(t, ns, &htlcOutput)
			assertKndrNotAtMaturityHeight(t, ns, &htlcOutput.kidOutput)
		}

		// Lastly, there should be no lingering preschool outputs.
		assertNumPreschools(t, ns, 0)
	}
}

// TestNurseryStoreGraduate verifies that the nursery store properly removes
// populated entries from the height index as it is purged, and that the last
// purged height is set appropriately.
func TestNurseryStoreGraduate(t *testing.T) {
	cdb, cleanUp, err := makeTestDB()
	if err != nil {
		t.Fatalf("unable to open channel db: %v", err)
	}
	defer cleanUp()

	ns, err := newNurseryStore(&bitcoinTestnetGenesis, cdb)
	if err != nil {
		t.Fatalf("unable to open nursery store: %v", err)
	}

	kid := &kidOutputs[3]

	// Compute the height at which this output will be inserted in the
	// height index.
	maturityHeight := kid.ConfHeight() + kid.BlocksToMaturity()

	// First, add a commitment output to the nursery store, which is
	// initially inserted in the preschool bucket.
	err = ns.Incubate([]kidOutput{*kid}, nil)
	if err != nil {
		t.Fatalf("unable to incubate commitment output: %v", err)
	}

	// Then, move the commitment output to the kindergarten bucket, such
	// that it resides in the height index at its maturity height.
	err = ns.PreschoolToKinder(kid, 0)
	if err != nil {
		t.Fatalf("unable to move pscl output to kndr: %v", err)
	}

	// Now, iteratively purge all height below the target maturity height,
	// checking that each class is now empty, and that the last purged
	// height is set correctly.
	for i := 0; i < int(maturityHeight); i++ {
		assertHeightIsPurged(t, ns, uint32(i))
	}

	// Check that the commitment output currently exists at its maturity
	// height.
	assertKndrAtMaturityHeight(t, ns, kid)

	err = ns.GraduateKinder(maturityHeight, kid)
	if err != nil {
		t.Fatalf("unable to graduate kindergarten outputs at height=%d: "+
			"%v", maturityHeight, err)
	}

	assertHeightIsPurged(t, ns, maturityHeight)
}

// assertNumChanOutputs checks that the channel bucket has the expected number
// of outputs.
func assertNumChanOutputs(t *testing.T, ns NurseryStore,
	chanPoint *wire.OutPoint, expectedNum int) {

	var count int
	err := ns.ForChanOutputs(chanPoint, func([]byte, []byte) error {
		count++
		return nil
	})

	if count == 0 && err == ErrContractNotFound {
		return
	} else if err != nil {
		t.Fatalf("unable to count num outputs for channel %v: %v",
			chanPoint, err)
	}

	if count != expectedNum {
		t.Fatalf("nursery store should have %d outputs, found %d",
			expectedNum, count)
	}
}

// assertNumPreschools loads all preschool outputs and verifies their count
// matches the expected number.
func assertNumPreschools(t *testing.T, ns NurseryStore, expected int) {
	psclOutputs, err := ns.FetchPreschools()
	if err != nil {
		t.Fatalf("unable to retrieve preschool outputs: %v", err)
	}

	if len(psclOutputs) != expected {
		t.Fatalf("expected number of pscl outputs to be %d, got %v",
			expected, len(psclOutputs))
	}
}

// assertNumChannels checks that the nursery has a given number of active
// channels.
func assertNumChannels(t *testing.T, ns NurseryStore, expected int) {
	channels, err := ns.ListChannels()
	if err != nil {
		t.Fatalf("unable to fetch channels from nursery store: %v",
			err)
	}

	if len(channels) != expected {
		t.Fatalf("expected number of active channels to be %d, got %d",
			expected, len(channels))
	}
}

// assertHeightIsPurged checks that the finalized transaction, kindergarten, and
// htlc outputs at a particular height are all nil.
func assertHeightIsPurged(t *testing.T, ns NurseryStore,
	height uint32) {

	kndrOutputs, cribOutputs, err := ns.FetchClass(height)
	if err != nil {
		t.Fatalf("unable to retrieve class at height=%d: %v",
			height, err)
	}

	if kndrOutputs != nil {
		t.Fatalf("height=%d not purged, kndr outputs should be nil", height)
	}

	if cribOutputs != nil {
		t.Fatalf("height=%d not purged, crib outputs should be nil", height)
	}
}

// assertCribAtExpiryHeight loads the class at the given height, and verifies
// that the given htlc output is one of the crib outputs.
func assertCribAtExpiryHeight(t *testing.T, ns NurseryStore,
	htlcOutput *babyOutput) {

	expiryHeight := htlcOutput.expiry
	_, cribOutputs, err := ns.FetchClass(expiryHeight)
	if err != nil {
		t.Fatalf("unable to retrieve class at height=%d: %v",
			expiryHeight, err)
	}

	for _, crib := range cribOutputs {
		if reflect.DeepEqual(&crib, htlcOutput) {
			return
		}
	}

	t.Fatalf("could not find crib output %v at height %d",
		htlcOutput.OutPoint(), expiryHeight)
}

// assertCribNotAtExpiryHeight loads the class at the given height, and verifies
// that the given htlc output is not one of the crib outputs.
func assertCribNotAtExpiryHeight(t *testing.T, ns NurseryStore,
	htlcOutput *babyOutput) {

	expiryHeight := htlcOutput.expiry
	_, cribOutputs, err := ns.FetchClass(expiryHeight)
	if err != nil {
		t.Fatalf("unable to retrieve class at height %d: %v",
			expiryHeight, err)
	}

	for _, crib := range cribOutputs {
		if reflect.DeepEqual(&crib, htlcOutput) {
			t.Fatalf("found find crib output %v at height %d",
				htlcOutput.OutPoint(), expiryHeight)
		}
	}
}

// assertKndrAtMaturityHeight loads the class at the provided height and
// verifies that the provided kid output is one of the kindergarten outputs
// returned.
func assertKndrAtMaturityHeight(t *testing.T, ns NurseryStore,
	kndrOutput *kidOutput) {

	maturityHeight := kndrOutput.ConfHeight() +
		kndrOutput.BlocksToMaturity()
	kndrOutputs, _, err := ns.FetchClass(maturityHeight)
	if err != nil {
		t.Fatalf("unable to retrieve class at height %d: %v",
			maturityHeight, err)
	}

	for _, kndr := range kndrOutputs {
		if reflect.DeepEqual(&kndr, kndrOutput) {
			return
		}
	}

	t.Fatalf("could not find kndr output %v at height %d",
		kndrOutput.OutPoint(), maturityHeight)
}

// assertKndrNotAtMaturityHeight loads the class at the provided height and
// verifies that the provided kid output is not one of the kindergarten outputs
// returned.
func assertKndrNotAtMaturityHeight(t *testing.T, ns NurseryStore,
	kndrOutput *kidOutput) {

	maturityHeight := kndrOutput.ConfHeight() +
		kndrOutput.BlocksToMaturity()

	kndrOutputs, _, err := ns.FetchClass(maturityHeight)
	if err != nil {
		t.Fatalf("unable to retrieve class at height %d: %v",
			maturityHeight, err)
	}

	for _, kndr := range kndrOutputs {
		if reflect.DeepEqual(&kndr, kndrOutput) {
			t.Fatalf("found find kndr output %v at height %d",
				kndrOutput.OutPoint(), maturityHeight)
		}
	}
}

// assertChannelMaturity queries the nursery store for the maturity of the given
// channel, failing if the result does not match the expectedMaturity.
func assertChannelMaturity(t *testing.T, ns NurseryStore,
	chanPoint *wire.OutPoint, expectedMaturity bool) {

	isMature, err := ns.IsMatureChannel(chanPoint)
	if err != nil {
		t.Fatalf("unable to fetch channel maturity: %v", err)
	}

	if isMature != expectedMaturity {
		t.Fatalf("expected channel maturity: %v, actual: %v",
			expectedMaturity, isMature)
	}
}

// assertCanRemoveChannel tries to remove a channel from the nursery store,
// failing if the result does match expected canRemove.
func assertCanRemoveChannel(t *testing.T, ns NurseryStore,
	chanPoint *wire.OutPoint, canRemove bool) {

	err := ns.RemoveChannel(chanPoint)
	if canRemove && err != nil {
		t.Fatalf("expected nil when removing active channel, got: %v",
			err)
	} else if !canRemove && err != ErrImmatureChannel {
		t.Fatalf("expected ErrImmatureChannel when removing "+
			"active channel: %v", err)
	}
}
