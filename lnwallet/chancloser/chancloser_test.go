package chancloser

import (
	"crypto/rand"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// randDeliveryAddress generates a random delivery address for testing.
func randDeliveryAddress(t *testing.T) lnwire.DeliveryAddress {
	// Generate an address of maximum length.
	da := lnwire.DeliveryAddress(make([]byte, 34))

	_, err := rand.Read(da)
	require.NoError(t, err, "cannot generate random address")

	return da
}

// TestMaybeMatchScript tests that the maybeMatchScript errors appropriately
// when an upfront shutdown script is set and the script provided does not
// match, and does not error in any other case.
func TestMaybeMatchScript(t *testing.T) {
	t.Parallel()

	addr1 := randDeliveryAddress(t)
	addr2 := randDeliveryAddress(t)

	tests := []struct {
		name           string
		shutdownScript lnwire.DeliveryAddress
		upfrontScript  lnwire.DeliveryAddress
		expectedErr    error
	}{
		{
			name:           "no upfront shutdown set, script ok",
			shutdownScript: addr1,
			upfrontScript:  []byte{},
			expectedErr:    nil,
		},
		{
			name:           "upfront shutdown set, script ok",
			shutdownScript: addr1,
			upfrontScript:  addr1,
			expectedErr:    nil,
		},
		{
			name:           "upfront shutdown set, script not ok",
			shutdownScript: addr1,
			upfrontScript:  addr2,
			expectedErr:    ErrUpfrontShutdownScriptMismatch,
		},
		{
			name:           "nil shutdown and empty upfront",
			shutdownScript: nil,
			upfrontScript:  []byte{},
			expectedErr:    nil,
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			err := maybeMatchScript(
				func() error { return nil }, test.upfrontScript,
				test.shutdownScript,
			)

			if err != test.expectedErr {
				t.Fatalf("Error: %v, expected error: %v", err, test.expectedErr)
			}
		})
	}
}

type mockChannel struct {
	absoluteFee btcutil.Amount
	chanPoint   wire.OutPoint
	initiator   bool
	scid        lnwire.ShortChannelID
}

func (m *mockChannel) CalcFee(chainfee.SatPerKWeight) btcutil.Amount {
	return m.absoluteFee
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

// TestMaxFeeClamp tests that if a max fee is specified, then it's used instead
// of the default max fee multiplier.
func TestMaxFeeClamp(t *testing.T) {
	t.Parallel()

	const absoluteFee = btcutil.Amount(1000)

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
			maxFee:   absoluteFee * defaultMaxFeeMultiplier,
		}, {
			// Max fee specified, this should be used in place.
			name: "max fee clamp",

			idealFee:    chainfee.SatPerKWeight(253),
			inputMaxFee: chainfee.SatPerKWeight(2530),

			// Our mock just returns the canned absolute fee here.
			maxFee: absoluteFee,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			channel := mockChannel{
				absoluteFee: absoluteFee,
			}

			chanCloser := NewChanCloser(
				ChanCloseCfg{
					Channel: &channel,
					MaxFee:  test.inputMaxFee,
				}, nil, test.idealFee, 0, nil, false,
			)

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
		t.Run(fmt.Sprintf("initiator=%v", isInitiator), func(t *testing.T) {
			t.Parallel()

			// First, we'll make our mock channel, and use that to
			// instantiate our channel closer.
			closeCfg := ChanCloseCfg{
				Channel: &mockChannel{
					absoluteFee: absoluteFee,
					initiator:   isInitiator,
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
