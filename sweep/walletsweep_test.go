package sweep

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/wtxmgr"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lntest/mock"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
)

// TestFeeEstimateInfo checks `Estimate` method works as expected.
func TestFeeEstimateInfo(t *testing.T) {
	t.Parallel()

	dummyErr := errors.New("dummy")

	const (
		// Set the relay fee rate to be 10 sat/kw.
		relayFeeRate = 10

		// Set the max fee rate to be 1000 sat/vb.
		maxFeeRate = 1000

		// Create a valid fee rate to test the success case.
		validFeeRate = (relayFeeRate + maxFeeRate) / 2

		// Set the test conf target to be 1.
		conf uint32 = 1
	)

	// Create a mock fee estimator.
	estimator := &chainfee.MockEstimator{}

	testCases := []struct {
		name            string
		setupMocker     func()
		feePref         FeeEstimateInfo
		expectedFeeRate chainfee.SatPerKWeight
		expectedErr     error
	}{
		{
			// When the fee preference is empty, we should see an
			// error.
			name:        "empty fee preference",
			feePref:     FeeEstimateInfo{},
			expectedErr: ErrNoFeePreference,
		},
		{
			// When the fee preference has conflicts, we should see
			// an error.
			name: "conflict fee preference",
			feePref: FeeEstimateInfo{
				FeeRate:    validFeeRate,
				ConfTarget: conf,
			},
			expectedErr: ErrFeePreferenceConflict,
		},
		{
			// When an error is returned from the fee estimator, we
			// should return it.
			name: "error from Estimator",
			setupMocker: func() {
				estimator.On("EstimateFeePerKW", conf).Return(
					chainfee.SatPerKWeight(0), dummyErr,
				).Once()
			},
			feePref:     FeeEstimateInfo{ConfTarget: conf},
			expectedErr: dummyErr,
		},
		{
			// When FeeEstimateInfo uses a too small value, we
			// should return an error.
			name: "fee rate below relay fee rate",
			setupMocker: func() {
				// Mock the relay fee rate.
				estimator.On("RelayFeePerKW").Return(
					chainfee.SatPerKWeight(relayFeeRate),
				).Once()
			},
			feePref:     FeeEstimateInfo{FeeRate: relayFeeRate - 1},
			expectedErr: ErrFeePreferenceTooLow,
		},
		{
			// When FeeEstimateInfo gives a too large value, we
			// should cap it at the max fee rate.
			name: "fee rate above max fee rate",
			setupMocker: func() {
				// Mock the relay fee rate.
				estimator.On("RelayFeePerKW").Return(
					chainfee.SatPerKWeight(relayFeeRate),
				).Once()
			},
			feePref: FeeEstimateInfo{
				FeeRate: maxFeeRate + 1,
			},
			expectedFeeRate: maxFeeRate,
		},
		{
			// When Estimator gives a sane fee rate, we should
			// return it without any error.
			name: "success",
			setupMocker: func() {
				estimator.On("EstimateFeePerKW", conf).Return(
					chainfee.SatPerKWeight(validFeeRate),
					nil).Once()

				// Mock the relay fee rate.
				estimator.On("RelayFeePerKW").Return(
					chainfee.SatPerKWeight(relayFeeRate),
				).Once()
			},
			feePref:         FeeEstimateInfo{ConfTarget: conf},
			expectedFeeRate: validFeeRate,
		},
	}

	for _, tc := range testCases {
		tc := tc

		t.Run(tc.name, func(t *testing.T) {
			// Setup the mockers if specified.
			if tc.setupMocker != nil {
				tc.setupMocker()
			}

			// Call the function under test.
			feerate, err := tc.feePref.Estimate(
				estimator, maxFeeRate,
			)

			// Assert the expected error.
			require.ErrorIs(t, err, tc.expectedErr)

			// Assert the expected feerate.
			require.Equal(t, tc.expectedFeeRate, feerate)

			// Assert the mockers.
			estimator.AssertExpectations(t)
		})
	}
}

type mockUtxoSource struct {
	outputs []*lnwallet.Utxo
}

func newMockUtxoSource(utxos []*lnwallet.Utxo) *mockUtxoSource {
	return &mockUtxoSource{
		outputs: utxos,
	}
}

func (m *mockUtxoSource) ListUnspentWitnessFromDefaultAccount(minConfs int32,
	maxConfs int32) ([]*lnwallet.Utxo, error) {

	return m.outputs, nil
}

type mockCoinSelectionLocker struct {
	fail bool
}

func (m *mockCoinSelectionLocker) WithCoinSelectLock(f func() error) error {
	if err := f(); err != nil {
		return err
	}

	if m.fail {
		return fmt.Errorf("kek")
	}

	return nil

}

type mockOutputLeaser struct {
	leasedOutputs map[wire.OutPoint]struct{}

	releasedOutputs map[wire.OutPoint]struct{}
}

func newMockOutputLeaser() *mockOutputLeaser {
	return &mockOutputLeaser{
		leasedOutputs: make(map[wire.OutPoint]struct{}),

		releasedOutputs: make(map[wire.OutPoint]struct{}),
	}
}

func (m *mockOutputLeaser) LeaseOutput(_ wtxmgr.LockID, o wire.OutPoint,
	t time.Duration) (time.Time, error) {

	m.leasedOutputs[o] = struct{}{}

	return time.Now().Add(t), nil
}

func (m *mockOutputLeaser) ReleaseOutput(_ wtxmgr.LockID,
	o wire.OutPoint) error {

	m.releasedOutputs[o] = struct{}{}

	return nil
}

var sweepScript = []byte{
	0x0, 0x14, 0x64, 0x3d, 0x8b, 0x15, 0x69, 0x4a, 0x54,
	0x7d, 0x57, 0x33, 0x6e, 0x51, 0xdf, 0xfd, 0x38, 0xe3,
	0xe, 0x6e, 0xf8, 0xef,
}

var deliveryAddr = func() btcutil.Address {
	_, addrs, _, err := txscript.ExtractPkScriptAddrs(
		sweepScript, &chaincfg.TestNet3Params,
	)
	if err != nil {
		panic(err)
	}

	return addrs[0]
}()

var testUtxos = []*lnwallet.Utxo{
	{
		// A p2wkh output.
		AddressType: lnwallet.WitnessPubKey,
		PkScript: []byte{
			0x0, 0x14, 0x64, 0x3d, 0x8b, 0x15, 0x69, 0x4a, 0x54,
			0x7d, 0x57, 0x33, 0x6e, 0x51, 0xdf, 0xfd, 0x38, 0xe3,
			0xe, 0x6e, 0xf7, 0xef,
		},
		Value: 1000,
		OutPoint: wire.OutPoint{
			Index: 1,
		},
	},

	{
		// A np2wkh output.
		AddressType: lnwallet.NestedWitnessPubKey,
		PkScript: []byte{
			0xa9, 0x14, 0x97, 0x17, 0xf7, 0xd1, 0x5f, 0x6f, 0x8b,
			0x7, 0xe3, 0x58, 0x43, 0x19, 0xb9, 0x7e, 0xa9, 0x20,
			0x18, 0xc3, 0x17, 0xd7, 0x87,
		},
		Value: 2000,
		OutPoint: wire.OutPoint{
			Index: 2,
		},
	},

	// A p2wsh output.
	{
		AddressType: lnwallet.UnknownAddressType,
		PkScript: []byte{
			0x0, 0x20, 0x70, 0x1a, 0x8d, 0x40, 0x1c, 0x84, 0xfb, 0x13,
			0xe6, 0xba, 0xf1, 0x69, 0xd5, 0x96, 0x84, 0xe2, 0x7a, 0xbd,
			0x9f, 0xa2, 0x16, 0xc8, 0xbc, 0x5b, 0x9f, 0xc6, 0x3d, 0x62,
			0x2f, 0xf8, 0xc5, 0x8c,
		},
		Value: 3000,
		OutPoint: wire.OutPoint{
			Index: 3,
		},
	},
}

func assertUtxosLeased(t *testing.T, utxoLeaser *mockOutputLeaser,
	utxos []*lnwallet.Utxo) {

	t.Helper()

	for _, utxo := range utxos {
		if _, ok := utxoLeaser.leasedOutputs[utxo.OutPoint]; !ok {
			t.Fatalf("utxo %v was never leased", utxo.OutPoint)
		}
	}

}

func assertNoUtxosReleased(t *testing.T, utxoLeaser *mockOutputLeaser,
	utxos []*lnwallet.Utxo) {

	t.Helper()

	if len(utxoLeaser.releasedOutputs) != 0 {
		t.Fatalf("outputs have been released, but shouldn't have been")
	}
}

func assertUtxosReleased(t *testing.T, utxoLeaser *mockOutputLeaser,
	utxos []*lnwallet.Utxo) {

	t.Helper()

	for _, utxo := range utxos {
		if _, ok := utxoLeaser.releasedOutputs[utxo.OutPoint]; !ok {
			t.Fatalf("utxo %v was never released", utxo.OutPoint)
		}
	}
}

func assertUtxosLeasedAndReleased(t *testing.T, utxoLeaser *mockOutputLeaser,
	utxos []*lnwallet.Utxo) {

	t.Helper()

	for _, utxo := range utxos {
		if _, ok := utxoLeaser.leasedOutputs[utxo.OutPoint]; !ok {
			t.Fatalf("utxo %v was never leased", utxo.OutPoint)
		}

		if _, ok := utxoLeaser.releasedOutputs[utxo.OutPoint]; !ok {
			t.Fatalf("utxo %v was never released", utxo.OutPoint)
		}
	}
}

// TestCraftSweepAllTxCoinSelectFail tests that if coin selection fails, then
// we unlock any outputs we may have locked in the passed closure.
func TestCraftSweepAllTxCoinSelectFail(t *testing.T) {
	t.Parallel()

	utxoSource := newMockUtxoSource(testUtxos)
	coinSelectLocker := &mockCoinSelectionLocker{
		fail: true,
	}
	utxoLeaser := newMockOutputLeaser()

	_, err := CraftSweepAllTx(
		0, 0, 10, nil, nil, coinSelectLocker, utxoSource, utxoLeaser,
		nil, 0, nil,
	)

	// Since we instructed the coin select locker to fail above, we should
	// get an error.
	if err == nil {
		t.Fatalf("sweep tx should have failed: %v", err)
	}

	// At this point, we'll now verify that all outputs were initially
	// locked, and then also unlocked due to the failure.
	assertUtxosLeasedAndReleased(t, utxoLeaser, testUtxos)
}

// TestCraftSweepAllTxUnknownWitnessType tests that if one of the inputs we
// encounter is of an unknown witness type, then we fail and unlock any prior
// locked outputs.
func TestCraftSweepAllTxUnknownWitnessType(t *testing.T) {
	t.Parallel()

	utxoSource := newMockUtxoSource(testUtxos)
	coinSelectLocker := &mockCoinSelectionLocker{}
	utxoLeaser := newMockOutputLeaser()

	_, err := CraftSweepAllTx(
		0, 0, 10, nil, nil, coinSelectLocker, utxoSource, utxoLeaser,
		nil, 0, nil,
	)

	// Since passed in a p2wsh output, which is unknown, we should fail to
	// map the output to a witness type.
	if err == nil {
		t.Fatalf("sweep tx should have failed: %v", err)
	}

	// At this point, we'll now verify that all outputs were initially
	// locked, and then also unlocked since we weren't able to find a
	// witness type for the last output.
	assertUtxosLeasedAndReleased(t, utxoLeaser, testUtxos)
}

// TestCraftSweepAllTx tests that we'll properly lock all available outputs
// within the wallet, and craft a single sweep transaction that pays to the
// target output.
func TestCraftSweepAllTx(t *testing.T) {
	t.Parallel()

	// First, we'll make a mock signer along with a fee estimator, We'll
	// use zero fees to we can assert a precise output value.
	signer := &mock.DummySigner{}

	// For our UTXO source, we'll pass in all the UTXOs that we know of,
	// other than the final one which is of an unknown witness type.
	targetUTXOs := testUtxos[:2]
	utxoSource := newMockUtxoSource(targetUTXOs)
	coinSelectLocker := &mockCoinSelectionLocker{}
	utxoLeaser := newMockOutputLeaser()

	sweepPkg, err := CraftSweepAllTx(
		0, 0, 10, nil, deliveryAddr, coinSelectLocker, utxoSource,
		utxoLeaser, signer, 0, nil,
	)
	require.NoError(t, err, "unable to make sweep tx")

	// At this point, all of the UTXOs that we made above should be locked
	// and none of them unlocked.
	assertUtxosLeased(t, utxoLeaser, testUtxos[:2])
	assertNoUtxosReleased(t, utxoLeaser, testUtxos[:2])

	// Now that we have our sweep transaction, we should find that we have
	// a UTXO for each input, and also that our final output value is the
	// sum of all our inputs.
	sweepTx := sweepPkg.SweepTx
	if len(sweepTx.TxIn) != len(targetUTXOs) {
		t.Fatalf("expected %v utxo, got %v", len(targetUTXOs),
			len(sweepTx.TxIn))
	}

	// We should have a single output that pays to our sweep script
	// generated above.
	expectedSweepValue := int64(3000)
	if len(sweepTx.TxOut) != 1 {
		t.Fatalf("should have %v outputs, instead have %v", 1,
			len(sweepTx.TxOut))
	}
	output := sweepTx.TxOut[0]
	switch {
	case output.Value != expectedSweepValue:
		t.Fatalf("expected %v sweep value, instead got %v",
			expectedSweepValue, output.Value)

	case !bytes.Equal(sweepScript, output.PkScript):
		t.Fatalf("expected %x sweep script, instead got %x", sweepScript,
			output.PkScript)
	}

	// If we cancel the sweep attempt, then we should find that all the
	// UTXOs within the sweep transaction are now unlocked.
	sweepPkg.CancelSweepAttempt()
	assertUtxosReleased(t, utxoLeaser, testUtxos[:2])
}

// TestCraftSweepAllTxWithSelectedUTXO tests that we'll properly lock the
// selected outputs within the wallet, and craft a single sweep transaction
// that pays to the target output.
func TestCraftSweepAllTxWithSelectedUTXO(t *testing.T) {
	t.Parallel()

	// First, we'll make a mock signer along with a fee estimator, We'll
	// use zero fees to we can assert a precise output value.
	signer := &mock.DummySigner{}

	// Grab the first UTXO from the test UTXOs.
	utxo1 := testUtxos[0]
	utxoSource := newMockUtxoSource([]*lnwallet.Utxo{utxo1})
	coinSelectLocker := &mockCoinSelectionLocker{}
	utxoLeaser := newMockOutputLeaser()

	// Create an unknown utxo.
	outpointUknown := wire.OutPoint{Index: 4}

	// Sweep using the uknnown utxo and expect an error.
	sweepPkg, err := CraftSweepAllTx(
		0, 0, 10, nil, deliveryAddr, coinSelectLocker, utxoSource,
		utxoLeaser, signer, 0, fn.NewSet(outpointUknown),
	)
	require.ErrorIs(t, err, ErrUnknownUTXO)
	require.Nil(t, sweepPkg)

	// Sweep again using the known utxo and expect no error.
	sweepPkg, err = CraftSweepAllTx(
		0, 0, 10, nil, deliveryAddr, coinSelectLocker, utxoSource,
		utxoLeaser, signer, 0, fn.NewSet(utxo1.OutPoint),
	)
	require.NoError(t, err)

	// At this point utxo1 should be locked.
	assertUtxosLeased(t, utxoLeaser, []*lnwallet.Utxo{utxo1})
	assertNoUtxosReleased(t, utxoLeaser, []*lnwallet.Utxo{utxo1})

	// Validate the sweeping tx has the expected shape.
	sweepTx := sweepPkg.SweepTx
	require.Len(t, sweepTx.TxIn, 1)
	require.Len(t, sweepTx.TxOut, 1)

	// We should have a single output that pays to our sweep script
	// generated above.
	expectedSweepValue := utxo1.Value
	require.Equal(t, sweepScript, sweepTx.TxOut[0].PkScript)
	require.EqualValues(t, expectedSweepValue, sweepTx.TxOut[0].Value)

	// If we cancel the sweep attempt, then we should find utxo1 to be
	// unlocked.
	sweepPkg.CancelSweepAttempt()
	assertUtxosReleased(t, utxoLeaser, []*lnwallet.Utxo{utxo1})
}
