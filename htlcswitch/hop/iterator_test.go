package hop

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/davecgh/go-spew/spew"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// TestSphinxHopIteratorForwardingInstructions tests that we're able to
// properly decode an onion payload, no matter the payload type, into the
// original set of forwarding instructions.
func TestSphinxHopIteratorForwardingInstructions(t *testing.T) {
	t.Parallel()

	// First, we'll make the hop data that the sender would create to send
	// an HTLC through our imaginary route.
	hopData := sphinx.HopData{
		ForwardAmount: 100000,
		OutgoingCltv:  4343,
	}
	copy(hopData.NextAddress[:], bytes.Repeat([]byte("a"), 8))

	// Next, we'll make the hop forwarding information that we should
	// extract each type, no matter the payload type.
	nextAddrInt := binary.BigEndian.Uint64(hopData.NextAddress[:])
	expectedFwdInfo := ForwardingInfo{
		NextHop:         lnwire.NewShortChanIDFromInt(nextAddrInt),
		AmountToForward: lnwire.MilliSatoshi(hopData.ForwardAmount),
		OutgoingCTLV:    hopData.OutgoingCltv,
	}

	// For our TLV payload, we'll serialize the hop into into a TLV stream
	// as we would normally in the routing network.
	var b bytes.Buffer
	tlvRecords := []tlv.Record{
		record.NewAmtToFwdRecord(&hopData.ForwardAmount),
		record.NewLockTimeRecord(&hopData.OutgoingCltv),
		record.NewNextHopIDRecord(&nextAddrInt),
	}
	tlvStream, err := tlv.NewStream(tlvRecords...)
	require.NoError(t, err, "unable to create stream")
	if err := tlvStream.Encode(&b); err != nil {
		t.Fatalf("unable to encode stream: %v", err)
	}

	var testCases = []struct {
		sphinxPacket    *sphinx.ProcessedPacket
		expectedFwdInfo ForwardingInfo
	}{
		// A regular legacy payload that signals more hops.
		{
			sphinxPacket: &sphinx.ProcessedPacket{
				Payload: sphinx.HopPayload{
					Type: sphinx.PayloadLegacy,
				},
				Action:                 sphinx.MoreHops,
				ForwardingInstructions: &hopData,
			},
			expectedFwdInfo: expectedFwdInfo,
		},
		// A TLV payload, which includes the sphinx action as
		// cid may be zero for blinded routes (thus we require the
		// action to signal whether we are at the final hop).
		{
			sphinxPacket: &sphinx.ProcessedPacket{
				Payload: sphinx.HopPayload{
					Type:    sphinx.PayloadTLV,
					Payload: b.Bytes(),
				},
				Action: sphinx.MoreHops,
			},
			expectedFwdInfo: expectedFwdInfo,
		},
	}

	// Finally, we'll test that we get the same set of
	// ForwardingInstructions for each payload type.
	iterator := sphinxHopIterator{}
	for i, testCase := range testCases {
		iterator.processedPacket = testCase.sphinxPacket

		pld, _, pldErr := iterator.HopPayload()
		if pldErr != nil {
			t.Fatalf("#%v: unable to extract forwarding "+
				"instructions: %v", i, pldErr)
		}

		fwdInfo := pld.ForwardingInfo()
		if fwdInfo != testCase.expectedFwdInfo {
			t.Fatalf("#%v: wrong fwding info: expected %v, got %v",
				i, spew.Sdump(testCase.expectedFwdInfo),
				spew.Sdump(fwdInfo))
		}
	}
}

// TestForwardingAmountCalc tests calculation of forwarding amounts from the
// hop's forwarding parameters.
func TestForwardingAmountCalc(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		incomingAmount lnwire.MilliSatoshi
		baseFee        lnwire.MilliSatoshi
		proportional   uint32
		forwardAmount  lnwire.MilliSatoshi
		expectErr      bool
	}{
		{
			name:           "overflow",
			incomingAmount: 10,
			baseFee:        100,
			expectErr:      true,
		},
		{
			name:           "trivial proportional",
			incomingAmount: 100_000,
			baseFee:        1000,
			proportional:   10,
			forwardAmount:  99000,
		},
		{
			name:           "both fees charged",
			incomingAmount: 10_002_020,
			baseFee:        1000,
			proportional:   1,
			forwardAmount:  10_001_010,
		},
	}

	for _, testCase := range tests {
		testCase := testCase

		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			actual, err := calculateForwardingAmount(
				testCase.incomingAmount, testCase.baseFee,
				testCase.proportional,
			)

			require.Equal(t, testCase.expectErr, err != nil)
			require.Equal(t, testCase.forwardAmount.ToSatoshis(),
				actual.ToSatoshis())
		})
	}
}

// mockProcessor is a mocked blinding point processor that just returns the
// data that it is called with when "decrypting".
type mockProcessor struct {
	decryptErr error
}

// DecryptBlindedHopData mocks blob decryption, returning the same data that
// it was called with and an optionally configured error.
func (m *mockProcessor) DecryptBlindedHopData(_ *btcec.PublicKey,
	data []byte) ([]byte, error) {

	return data, m.decryptErr
}

// NextEphemeral mocks getting our next ephemeral key.
func (m *mockProcessor) NextEphemeral(*btcec.PublicKey) (*btcec.PublicKey,
	error) {

	return nil, nil
}

// TestParseAndValidateRecipientData tests deriving forwarding info using a
// blinding kit. This test does not cover assertions on the calculations of
// forwarding information, because this is covered in a test dedicated to those
// calculations.
func TestParseAndValidateRecipientData(t *testing.T) {
	t.Parallel()

	// Encode valid blinding data that we'll fake decrypting for our test.
	maxCltv := 1000
	blindedData := record.NewNonFinalBlindedRouteData(
		lnwire.NewShortChanIDFromInt(1500), nil,
		record.PaymentRelayInfo{
			CltvExpiryDelta: 10,
			BaseFee:         100,
			FeeRate:         0,
		},
		&record.PaymentConstraints{
			MaxCltvExpiry:   1000,
			HtlcMinimumMsat: lnwire.MilliSatoshi(1),
		},
		nil,
	)

	validData, err := record.EncodeBlindedRouteData(blindedData)
	require.NoError(t, err)

	// Mocked error.
	errDecryptFailed := errors.New("could not decrypt")

	nodeKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	tests := []struct {
		name              string
		data              []byte
		incomingCLTV      uint32
		updateAddBlinding *btcec.PublicKey
		payloadBlinding   *btcec.PublicKey
		processor         *mockProcessor
		expectedErr       error
	}{
		{
			name:        "no blinding point",
			data:        validData,
			processor:   &mockProcessor{},
			expectedErr: ErrNoBlindingPoint,
		},
		{
			name:              "decryption failed",
			data:              validData,
			updateAddBlinding: &btcec.PublicKey{},
			incomingCLTV:      500,
			processor: &mockProcessor{
				decryptErr: errDecryptFailed,
			},
			expectedErr: errDecryptFailed,
		},
		{
			name:              "decode fails",
			data:              []byte{1, 2, 3},
			updateAddBlinding: &btcec.PublicKey{},
			incomingCLTV:      500,
			processor:         &mockProcessor{},
			expectedErr:       ErrDecodeFailed,
		},
		{
			name:              "validation fails",
			data:              validData,
			updateAddBlinding: &btcec.PublicKey{},
			incomingCLTV:      uint32(maxCltv) + 10,
			processor:         &mockProcessor{},
			expectedErr: ErrInvalidPayload{
				Type:      record.LockTimeOnionType,
				Violation: InsufficientViolation,
			},
		},
		{
			name:              "valid using update add",
			updateAddBlinding: &btcec.PublicKey{},
			data:              validData,
			processor:         &mockProcessor{},
			expectedErr:       nil,
		},
		{
			name:            "valid using payload",
			payloadBlinding: &btcec.PublicKey{},
			data:            validData,
			processor:       &mockProcessor{},
			expectedErr:     nil,
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			// We don't actually use blinding keys due to our
			// mocking so they can be nil.
			kit := BlindingKit{
				Processor:      testCase.processor,
				IncomingAmount: 10000,
				IncomingCltv:   testCase.incomingCLTV,
			}

			if testCase.updateAddBlinding != nil {
				kit.UpdateAddBlinding = tlv.SomeRecordT(
					//nolint:ll
					tlv.NewPrimitiveRecord[lnwire.BlindingPointTlvType](testCase.updateAddBlinding),
				)
			}
			iterator := &sphinxHopIterator{
				blindingKit: kit,
				router: sphinx.NewRouter(
					&sphinx.PrivKeyECDH{PrivKey: nodeKey},
					sphinx.NewMemoryReplayLog(),
				),
			}

			_, _, err = parseAndValidateRecipientData(
				iterator, &Payload{
					encryptedData: testCase.data,
					blindingPoint: testCase.payloadBlinding,
				}, false, RouteRoleCleartext,
			)
			require.ErrorIs(t, err, testCase.expectedErr)
		})
	}
}
