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
			expectedErr:    ErrUpfrontShutdownScriptMismatch,
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

	return nil, nil, 0, nil
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
				}, nil, test.idealFee, 0, nil, false,
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
				closeCfg, nil, idealFee, 0, nil, false,
			)

			// We'll now force the channel state into the
			// closeFeeNegotiation state so we can skip straight to
			// the juicy part. We'll also set our last fee sent so
			// we'll attempt to actually "negotiate" here.
			chanCloser.state = closeFeeNegotiation
			chanCloser.lastFeeProposal = absoluteFee

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

			_, _, err := chanCloser.ProcessCloseMsg(closeMsg)

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

// TestParseUpfrontShutdownAddress tests the we are able to parse the upfront
// shutdown address properly.
func TestParseUpfrontShutdownAddress(t *testing.T) {
	t.Parallel()

	var (
		testnetAddress = "tb1qdfkmwwgdaa5dnezrlhtftvmj5qn2kwgp7n0z6r"
		regtestAddress = "bcrt1q09crvvuj95x5nk64wsxf5n6ky0kr8358vpx4d8"
	)

	tests := []struct {
		name        string
		address     string
		params      chaincfg.Params
		expectedErr string
	}{
		{
			name:        "invalid closing address",
			address:     "non-valid-address",
			params:      chaincfg.RegressionNetParams,
			expectedErr: "invalid address",
		},
		{
			name:        "closing address from another net",
			address:     testnetAddress,
			params:      chaincfg.RegressionNetParams,
			expectedErr: "not a regtest address",
		},
		{
			name:    "valid p2wkh closing address",
			address: regtestAddress,
			params:  chaincfg.RegressionNetParams,
		},
	}

	for _, tc := range tests {
		tc := tc

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := ParseUpfrontShutdownAddress(
				tc.address, &tc.params,
			)

			if tc.expectedErr != "" {
				require.ErrorContains(t, err, tc.expectedErr)
				return
			}

			require.NoError(t, err)
		})
	}
}
