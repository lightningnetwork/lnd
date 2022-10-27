package chancloser

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// TestMaybeMatchScript tests that the maybeMatchScript errors appropriately
// when an upfront shutdown script is set and the script provided does not
// match, and does not error in any other case.
func TestMaybeMatchScript(t *testing.T) {
	t.Parallel()

	pubHash := bytes.Repeat([]byte{0x0}, 20)
	scriptHash := bytes.Repeat([]byte{0x0}, 32)

	p2wkh, err := txscript.NewScriptBuilder().AddOp(txscript.OP_0).
		AddData(pubHash).Script()
	require.NoError(t, err)

	p2wsh, err := txscript.NewScriptBuilder().AddOp(txscript.OP_0).
		AddData(scriptHash).Script()
	require.NoError(t, err)

	p2tr, err := txscript.NewScriptBuilder().AddOp(txscript.OP_1).
		AddData(scriptHash).Script()
	require.NoError(t, err)

	p2OtherV1, err := txscript.NewScriptBuilder().AddOp(txscript.OP_1).
		AddData(pubHash).Script()
	require.NoError(t, err)

	invalidFork, err := txscript.NewScriptBuilder().AddOp(txscript.OP_NOP).
		AddData(scriptHash).Script()
	require.NoError(t, err)

	type testCase struct {
		name           string
		shutdownScript lnwire.DeliveryAddress
		upfrontScript  lnwire.DeliveryAddress
		expectedErr    error
	}
	tests := []testCase{
		{
			name:           "no upfront shutdown set, script ok",
			shutdownScript: p2wkh,
			upfrontScript:  []byte{},
			expectedErr:    nil,
		},
		{
			name:           "upfront shutdown set, script ok",
			shutdownScript: p2wkh,
			upfrontScript:  p2wkh,
			expectedErr:    nil,
		},
		{
			name:           "upfront shutdown set, script not ok",
			shutdownScript: p2wkh,
			upfrontScript:  p2wsh,
			expectedErr:    htlcswitch.ErrUpfrontShutdownScriptMismatch,
		},
		{
			name:           "nil shutdown and empty upfront",
			shutdownScript: nil,
			upfrontScript:  []byte{},
			expectedErr:    nil,
		},
		{
			name:           "p2tr is ok",
			shutdownScript: p2tr,
		},
		{
			name:           "segwit v1 is ok",
			shutdownScript: p2OtherV1,
		},
		{
			name:           "invalid script not allowed",
			shutdownScript: invalidFork,
			expectedErr:    ErrInvalidShutdownScript,
		},
	}

	// All future segwit softforks should also be ok.
	futureForks := []byte{
		txscript.OP_1, txscript.OP_2, txscript.OP_3, txscript.OP_4,
		txscript.OP_5, txscript.OP_6, txscript.OP_7, txscript.OP_8,
		txscript.OP_9, txscript.OP_10, txscript.OP_11, txscript.OP_12,
		txscript.OP_13, txscript.OP_14, txscript.OP_15, txscript.OP_16,
	}
	for _, witnessVersion := range futureForks {
		p2FutureFork, err := txscript.NewScriptBuilder().AddOp(witnessVersion).
			AddData(scriptHash).Script()
		require.NoError(t, err)

		opString, err := txscript.DisasmString([]byte{witnessVersion})
		require.NoError(t, err)

		tests = append(tests, testCase{
			name:           fmt.Sprintf("witness_version=%v", opString),
			shutdownScript: p2FutureFork,
		})
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			err := validateShutdownScript(
				func() error { return nil }, test.upfrontScript,
				test.shutdownScript, &chaincfg.SimNetParams,
			)

			if err != test.expectedErr {
				t.Fatalf("Error: %v, expected error: %v", err, test.expectedErr)
			}
		})
	}
}

type mockChannel struct {
	chanPoint wire.OutPoint
	initiator bool
	scid      lnwire.ShortChannelID
}

func (m *mockChannel) ChannelPoint() *wire.OutPoint {
	return &m.chanPoint
}

func (m *mockChannel) MarkCoopBroadcasted(*wire.MsgTx, bool) error {
	return nil
}

func (m *mockChannel) IsInitiator() bool {
	return m.initiator
}

func (m *mockChannel) ShortChanID() lnwire.ShortChannelID {
	return m.scid
}

func (m *mockChannel) AbsoluteThawHeight() (uint32, error) {
	return 0, nil
}

func (m *mockChannel) RemoteUpfrontShutdownScript() lnwire.DeliveryAddress {
	return lnwire.DeliveryAddress{}
}

func (m *mockChannel) CreateCloseProposal(fee btcutil.Amount,
	localScript, remoteScript []byte,
) (input.Signature, *chainhash.Hash, btcutil.Amount, error) {

	s := &lnwire.Sig{
		// r value
		0x4e, 0x45, 0xe1, 0x69, 0x32, 0xb8, 0xaf, 0x51,
		0x49, 0x61, 0xa1, 0xd3, 0xa1, 0xa2, 0x5f, 0xdf,
		0x3f, 0x4f, 0x77, 0x32, 0xe9, 0xd6, 0x24, 0xc6,
		0xc6, 0x15, 0x48, 0xab, 0x5f, 0xb8, 0xcd, 0x41,
		// s value
		0x18, 0x15, 0x22, 0xec, 0x8e, 0xca, 0x07, 0xde,
		0x48, 0x60, 0xa4, 0xac, 0xdd, 0x12, 0x90, 0x9d,
		0x83, 0x1c, 0xc5, 0x6c, 0xbb, 0xac, 0x46, 0x22,
		0x08, 0x22, 0x21, 0xa8, 0x76, 0x8d, 0x1d, 0x09,
	}
	ecdsaSig, err := s.ToSignature()
	if err != nil {
		return nil, nil, 0, err
	}

	return ecdsaSig, nil, 0, nil
}

func (m *mockChannel) CompleteCooperativeClose(localSig,
	remoteSig input.Signature, localScript, remoteScript []byte,
	proposedFee btcutil.Amount) (*wire.MsgTx, btcutil.Amount, error) {

	return nil, 0, nil
}

func (m *mockChannel) LocalBalanceDust() bool {
	return false
}

func (m *mockChannel) RemoteBalanceDust() bool {
	return false
}

type mockCoopFeeEstimator struct {
	targetFee btcutil.Amount
}

func (m *mockCoopFeeEstimator) EstimateFee(chanType channeldb.ChannelType,
	localTxOut, remoteTxOut *wire.TxOut,
	idealFeeRate chainfee.SatPerKWeight) btcutil.Amount {

	return m.targetFee
}

type mockFeeRangeEstimator struct{}

func (m *mockFeeRangeEstimator) EstimateFee(chanType channeldb.ChannelType,
	localTxOut, remoteTxOut *wire.TxOut,
	idealFeeRate chainfee.SatPerKWeight) btcutil.Amount {

	// Use a weight of 2000 even though coop closing will never produce
	// such a large transaction. This is needed so that the ChanCloser can
	// create a realistic fee-range instead of a 1sat fee-range if the
	// mockCoopFeeEstimator were to be used.
	return idealFeeRate.FeeForWeight(2000)
}

type feeRangeHarness struct {
	t            *testing.T
	funderCloser *ChanCloser
	fundeeCloser *ChanCloser
}

// newFeeRangeHarness returns a new testing harness for testing fee-range logic.
// It returns two ChanCloser pointers that are set to the closeFeeNegotiation
// state and are already in the clean state.
func newFeeRangeHarness(t *testing.T, funderRate,
	fundeeRate, maxFee chainfee.SatPerKWeight) (*feeRangeHarness,
	*lnwire.ClosingSigned) {

	broadcastStub := func(*wire.MsgTx, string) error { return nil }

	funderCfg := ChanCloseCfg{
		Channel: &mockChannel{
			initiator: true,
		},
		FeeEstimator: &mockFeeRangeEstimator{},
		BroadcastTx:  broadcastStub,
		MaxFee:       maxFee,
	}
	funderCloser := NewChanCloser(
		funderCfg, nil, funderRate, 0, nil, true, false,
	)

	fundeeCfg := ChanCloseCfg{
		Channel:      &mockChannel{},
		FeeEstimator: &mockFeeRangeEstimator{},
		BroadcastTx:  broadcastStub,
	}
	fundeeCloser := NewChanCloser(
		fundeeCfg, nil, fundeeRate, 0, nil, false, false,
	)

	// Set both sides' state to closeFeeNegotiation and call ChannelClean.
	funderCloser.state = closeFeeNegotiation
	fundeeCloser.state = closeFeeNegotiation

	msg, done, err := funderCloser.ChannelClean()
	require.NoError(t, err)
	require.False(t, done)
	require.Equal(t, 1, len(msg))

	funderClosing, ok := msg[0].(*lnwire.ClosingSigned)
	require.True(t, ok)

	msg, done, err = fundeeCloser.ChannelClean()
	require.NoError(t, err)
	require.False(t, done)
	require.Nil(t, msg)

	harness := &feeRangeHarness{
		t:            t,
		funderCloser: funderCloser,
		fundeeCloser: fundeeCloser,
	}

	return harness, funderClosing
}

// processCloseMsg allows the harness to process a close message and assert
// various things like whether negotiation is complete or whether an error
// should be returned from the ProcessCloseMsg call.
func (f *feeRangeHarness) processCloseMsg(closeMsg lnwire.Message,
	fundee, expectMsg, done bool, expectedErr error) lnwire.Message {

	closer := f.funderCloser
	if fundee {
		closer = f.fundeeCloser
	}

	msg, finished, err := closer.ProcessCloseMsg(closeMsg, true)
	require.Equal(f.t, finished, done)

	if expectedErr != nil {
		require.ErrorIs(f.t, err, expectedErr)
	} else {
		require.NoError(f.t, err)
	}

	if expectMsg {
		require.NotNil(f.t, msg)
		require.Equal(f.t, 1, len(msg))
		return msg[0]
	}

	require.Nil(f.t, msg)
	return nil
}

// TestMaxFeeClamp tests that if a max fee is specified, then it's used instead
// of the default max fee multiplier.
func TestMaxFeeClamp(t *testing.T) {
	t.Parallel()

	const (
		absoluteFeeOneSatByte = 126
		absoluteFeeTenSatByte = 1265
	)

	tests := []struct {
		name string

		idealFee    chainfee.SatPerKWeight
		inputMaxFee chainfee.SatPerKWeight

		maxFee btcutil.Amount
	}{
		{
			// No max fee specified, we should see 3x the ideal fee.
			name: "no max fee",

			idealFee: chainfee.SatPerKWeight(253),
			maxFee:   absoluteFeeOneSatByte * defaultMaxFeeMultiplier,
		},
		{
			// Max fee specified, this should be used in place.
			name: "max fee clamp",

			idealFee:    chainfee.SatPerKWeight(253),
			inputMaxFee: chainfee.SatPerKWeight(2530),

			// We should get the resulting absolute fee based on a
			// factor of 10 sat/byte (our new max fee).
			maxFee: absoluteFeeTenSatByte,
		},
	}
	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			channel := mockChannel{}

			chanCloser := NewChanCloser(
				ChanCloseCfg{
					Channel:      &channel,
					MaxFee:       test.inputMaxFee,
					FeeEstimator: &SimpleCoopFeeEstimator{},
				}, nil, test.idealFee, 0, nil, false, false,
			)

			// We'll call initFeeBaseline early here since we need
			// the populate these internal variables.
			chanCloser.initFeeBaseline()

			require.Equal(t, test.maxFee, chanCloser.maxFee)
		})
	}
}

// TestMaxFeeBailOut tests that once the negotiated fee rate rises above our
// maximum fee, we'll return an error and refuse to process a co-op close
// message.
func TestMaxFeeBailOut(t *testing.T) {
	t.Parallel()

	const (
		absoluteFee = btcutil.Amount(1000)
		idealFee    = chainfee.SatPerKWeight(253)
	)

	for _, isInitiator := range []bool{true, false} {
		isInitiator := isInitiator

		t.Run(fmt.Sprintf("initiator=%v", isInitiator), func(t *testing.T) {
			t.Parallel()

			// First, we'll make our mock channel, and use that to
			// instantiate our channel closer.
			closeCfg := ChanCloseCfg{
				Channel: &mockChannel{
					initiator: isInitiator,
				},
				FeeEstimator: &mockCoopFeeEstimator{
					targetFee: absoluteFee,
				},
				MaxFee: idealFee * 2,
			}
			chanCloser := NewChanCloser(
				closeCfg, nil, idealFee, 0, nil, false, false,
			)

			// We'll now force the channel state into the
			// closeFeeNegotiation state so we can skip straight to
			// the juicy part. We'll also set our last fee sent so
			// we'll attempt to actually "negotiate" here.
			chanCloser.state = closeFeeNegotiation
			chanCloser.lastFeeProposal = absoluteFee

			// Put the ChanCloser into a clean state by calling
			// ChannelClean.
			_, _, err := chanCloser.ChannelClean()
			require.NoError(t, err)

			// Next, we'll make a ClosingSigned message that
			// proposes a fee that's above the specified max fee.
			//
			// NOTE: We use the absoluteFee here since our mock
			// always returns this fee for the CalcFee method which
			// is used to translate a fee rate
			// into an absolute fee amount in sats.
			closeMsg := &lnwire.ClosingSigned{
				FeeSatoshis: absoluteFee * 2,
			}

			_, _, err = chanCloser.ProcessCloseMsg(closeMsg, true)

			switch isInitiator {
			// If we're the initiator, then we expect an error at
			// this point.
			case true:
				require.ErrorIs(t, err, ErrProposalExeceedsMaxFee)

			// Otherwise, we expect things to fail for some other
			// reason (invalid sig, etc).
			case false:
				require.NotErrorIs(t, err, ErrProposalExeceedsMaxFee)
			}
		})
	}
}

// TestFeeRangeFundeeAgreesIdeal tests fee_range negotiation can end early if
// the fundee immediately agrees with the funder's fee_satoshis.
func TestFeeRangeFundeeAgreesIdeal(t *testing.T) {
	t.Parallel()

	idealFeeRate := chainfee.SatPerKWeight(500)
	harness, funderClosing := newFeeRangeHarness(
		t, idealFeeRate, idealFeeRate, chainfee.SatPerKWeight(0),
	)

	// The fundee will process the close message.
	msg := harness.processCloseMsg(funderClosing, true, true, true, nil)

	fundeeClosing, ok := msg.(*lnwire.ClosingSigned)
	require.True(t, ok)

	// Assert that we are done with negotiation.
	require.Equal(
		t, fundeeClosing.FeeSatoshis, funderClosing.FeeSatoshis,
	)

	msg = harness.processCloseMsg(fundeeClosing, false, true, true, nil)

	funderClosing, ok = msg.(*lnwire.ClosingSigned)
	require.True(t, ok)
	require.Equal(
		t, fundeeClosing.FeeSatoshis, funderClosing.FeeSatoshis,
	)
}

// TestFeeRangeFundeeAgreesOverlap tests that negotiation completes in two
// messages if the overlap's MaxFeeSats for the fundee is equal to the fee
// proposed by the funder.
func TestFeeRangeFundeeAgreesOverlap(t *testing.T) {
	t.Parallel()

	// We use a fundee feerate of 250 so that it is out of the overlap and
	// the fundee chooses the maximum overlap value. This happens to be the
	// funder's MaxFee so negotiation completes early.
	funderFeeRate := chainfee.SatPerKWeight(500)
	fundeeFeeRate := chainfee.SatPerKWeight(250)

	harness, funderClosing := newFeeRangeHarness(
		t, funderFeeRate, fundeeFeeRate, funderFeeRate,
	)

	// The fundee will process the close message.
	msg := harness.processCloseMsg(funderClosing, true, true, true, nil)

	fundeeClosing, ok := msg.(*lnwire.ClosingSigned)
	require.True(t, ok)

	// Assert that we are done with negotiation and that fundeeClosing uses
	// a feerate of 500 at a weight of 2000.
	expectedFee := funderFeeRate.FeeForWeight(2000)
	require.Equal(
		t, fundeeClosing.FeeSatoshis, funderClosing.FeeSatoshis,
	)
	require.Equal(t, expectedFee, fundeeClosing.FeeSatoshis)
	require.Equal(t, expectedFee, funderClosing.FeeRange.MaxFeeSats)

	msg = harness.processCloseMsg(fundeeClosing, false, true, true, nil)

	funderClosing, ok = msg.(*lnwire.ClosingSigned)
	require.True(t, ok)
	require.Equal(t, expectedFee, funderClosing.FeeSatoshis)
}

// TestFeeRangeRegular tests fee_range negotiation completes in the happy path.
func TestFeeRangeRegular(t *testing.T) {
	t.Parallel()

	funderFeeRate := chainfee.SatPerKWeight(500)
	fundeeFeeRate := chainfee.SatPerKWeight(800)
	harness, funderClosing := newFeeRangeHarness(
		t, funderFeeRate, fundeeFeeRate, chainfee.SatPerKWeight(0),
	)

	// The fundee will process the close message.
	msg := harness.processCloseMsg(funderClosing, true, true, false, nil)

	fundeeClosing, ok := msg.(*lnwire.ClosingSigned)
	require.True(t, ok)

	// Assert that the fundee replies with the fundeeFeeRate for 2000WU.
	expectedFee := fundeeFeeRate.FeeForWeight(2000)
	require.Equal(t, expectedFee, fundeeClosing.FeeSatoshis)

	// The funder should consider negotiation done at this point.
	msg = harness.processCloseMsg(fundeeClosing, false, true, true, nil)

	funderClosing, ok = msg.(*lnwire.ClosingSigned)
	require.True(t, ok)
	require.Equal(t, expectedFee, funderClosing.FeeSatoshis)

	// Now the fundee should also consider negotiation done.
	msg = harness.processCloseMsg(funderClosing, true, true, true, nil)

	fundeeClosing, ok = msg.(*lnwire.ClosingSigned)
	require.True(t, ok)
	require.Equal(t, expectedFee, fundeeClosing.FeeSatoshis)
}

// TestFeeRangeCloseTypeChangedLegacy verifies that negotiation fails if the
// type of negotiation is changed in the middle of negotiation from legacy to
// range-based. We only test the fundee logic.
func TestFeeRangeCloseTypeChangedLegacy(t *testing.T) {
	t.Parallel()

	funderFeeRate := chainfee.SatPerKWeight(500)
	fundeeFeeRate := chainfee.SatPerKWeight(800)

	harness, funderClosing := newFeeRangeHarness(
		t, funderFeeRate, fundeeFeeRate, chainfee.SatPerKWeight(0),
	)

	// Give the fundee a ClosingSigned message without a FeeRange.
	funderClosing.FeeRange = nil
	msg := harness.processCloseMsg(funderClosing, true, true, false, nil)

	// Give the funder the message so that we can obtain a message to give
	// the fundee.
	fundeeClosing, ok := msg.(*lnwire.ClosingSigned)
	require.True(t, ok)

	// The funder is technically done, but this does not matter since we are
	// not testing the funder.
	msg = harness.processCloseMsg(fundeeClosing, false, true, true, nil)

	// We'll change the FeeSatoshis of funderClosing so that we can test the
	// close type logic.
	funderClosing, ok = msg.(*lnwire.ClosingSigned)
	require.True(t, ok)
	require.NotNil(t, funderClosing.FeeRange)
	funderClosing.FeeSatoshis = btcutil.Amount(501)

	// The fundee should fail with ErrCloseTypeChanged.
	_ = harness.processCloseMsg(
		funderClosing, true, false, false, ErrCloseTypeChanged,
	)
}

// TestFeeRangeCloseTypeChangedRange verifies that negotiation fails if the type
// of negotiation is changed from range-based to legacy. We only test the fundee
// logic since it is not possible to test the funder as the funder would already
// be finished negotiating.
func TestFeeRangeCloseTypeChangedRange(t *testing.T) {
	t.Parallel()

	funderFeeRate := chainfee.SatPerKWeight(500)
	fundeeFeeRate := chainfee.SatPerKWeight(800)

	harness, funderClosing := newFeeRangeHarness(
		t, funderFeeRate, fundeeFeeRate, chainfee.SatPerKWeight(0),
	)

	// Give the fundee the ClosingSigned message to start range-based
	// negotiation.
	msg := harness.processCloseMsg(funderClosing, true, true, false, nil)

	fundeeClosing, ok := msg.(*lnwire.ClosingSigned)
	require.True(t, ok)

	// Give the message to the funder so we can get another message to give
	// to the fundee.
	msg = harness.processCloseMsg(fundeeClosing, false, true, true, nil)

	// We'll change FeeSatoshis so we can trigger the close-type-changed
	// logic. We'll also un-set FeeRange.
	funderClosing, ok = msg.(*lnwire.ClosingSigned)
	require.True(t, ok)
	funderClosing.FeeSatoshis = btcutil.Amount(501)
	funderClosing.FeeRange = nil

	// The fundee should fail with ErrCloseTypeChanged
	_ = harness.processCloseMsg(
		funderClosing, true, false, false, ErrCloseTypeChanged,
	)
}

// TestFeeRangeNoOverlapFunder tests that the funder will fail if they receive a
// ClosingSigned with no fee-range overlap.
func TestFeeRangeNoOverlapFunder(t *testing.T) {
	t.Parallel()

	funderFeeRate := chainfee.SatPerKWeight(500)
	fundeeFeeRate := chainfee.SatPerKWeight(800)

	harness, funderClosing := newFeeRangeHarness(
		t, funderFeeRate, fundeeFeeRate, chainfee.SatPerKWeight(0),
	)

	// Hand off the message to the fundee.
	msg := harness.processCloseMsg(funderClosing, true, true, false, nil)

	// We'll change the FeeRange to trigger the no overlap error for the
	// funder.
	fundeeClosing, ok := msg.(*lnwire.ClosingSigned)
	require.True(t, ok)
	fundeeClosing.FeeRange = &lnwire.FeeRange{}

	_ = harness.processCloseMsg(
		fundeeClosing, false, false, false, ErrNoRangeOverlap,
	)
}

// TestFeeRangeNoOverlapFundee tests that the fundee will send a warning if it
// receives a FeeRange that has no overlap with its own yet-to-be-sent FeeRange.
func TestFeeRangeNoOverlapFundee(t *testing.T) {
	t.Parallel()

	funderFeeRate := chainfee.SatPerKWeight(500)
	fundeeFeeRate := chainfee.SatPerKWeight(800)

	harness, funderClosing := newFeeRangeHarness(
		t, funderFeeRate, fundeeFeeRate, chainfee.SatPerKWeight(0),
	)

	// Modify funderClosing to have a FeeRange with no overlap.
	funderClosing.FeeRange = &lnwire.FeeRange{}

	// Hand off the message to the fundee and assert that we get back a
	// warning.
	msg := harness.processCloseMsg(funderClosing, true, true, false, nil)

	_, ok := msg.(*lnwire.Warning)
	require.True(t, ok)
}

// TestFeeNotInOverlap tests that ErrFeeNotInOverlap is returned when the fee
// that the fundee proposes is not in the range overlap.
func TestFeeNotInOverlap(t *testing.T) {
	t.Parallel()

	funderFeeRate := chainfee.SatPerKWeight(500)
	fundeeFeeRate := chainfee.SatPerKWeight(800)

	harness, funderClosing := newFeeRangeHarness(
		t, funderFeeRate, fundeeFeeRate, chainfee.SatPerKWeight(0),
	)

	// Hand off the message to the fundee.
	msg := harness.processCloseMsg(funderClosing, true, true, false, nil)

	// We will change FeeSatoshis to zero so that it is out of the overlap.
	fundeeClosing, ok := msg.(*lnwire.ClosingSigned)
	require.True(t, ok)
	fundeeClosing.FeeSatoshis = btcutil.Amount(0)

	// The funder should error with ErrFeeNotInOverlap.
	_ = harness.processCloseMsg(
		fundeeClosing, false, false, false, ErrFeeNotInOverlap,
	)
}

// TestErrFeeRangeViolation tests that ErrFeeRangeViolation is returned if the
// funder does not follow the spec by agreeing with the fundee's FeeSatoshis.
func TestErrFeeRangeViolation(t *testing.T) {
	t.Parallel()

	funderFeeRate := chainfee.SatPerKWeight(500)
	fundeeFeeRate := chainfee.SatPerKWeight(800)

	harness, funderClosing := newFeeRangeHarness(
		t, funderFeeRate, fundeeFeeRate, chainfee.SatPerKWeight(0),
	)

	// Hand off the message to the fundee.
	msg := harness.processCloseMsg(funderClosing, true, true, false, nil)

	fundeeClosing, ok := msg.(*lnwire.ClosingSigned)
	require.True(t, ok)

	msg = harness.processCloseMsg(fundeeClosing, false, true, true, nil)

	// We'll modify FeeSatoshis to not match what the fundee sent. This will
	// trigger ErrFeeRangeViolation.
	funderClosing, ok = msg.(*lnwire.ClosingSigned)
	require.True(t, ok)
	funderClosing.FeeSatoshis = btcutil.Amount(501)

	_ = harness.processCloseMsg(
		funderClosing, true, false, false, ErrFeeRangeViolation,
	)
}
