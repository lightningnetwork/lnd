package lnwire

import (
	"bytes"
	"reflect"
	"testing"
)

var (
	testOnionHash     = []byte{}
	testAmount        = MilliSatoshi(1)
	testCtlvExpiry    = uint32(2)
	testFlags         = uint16(2)
	sig, _            = NewSigFromSignature(testSig)
	testChannelUpdate = ChannelUpdate{
		Signature:      sig,
		ShortChannelID: NewShortChanIDFromInt(1),
		Timestamp:      1,
		Flags:          1,
	}
)

var onionFailures = []FailureMessage{
	&FailInvalidRealm{},
	&FailTemporaryNodeFailure{},
	&FailPermanentNodeFailure{},
	&FailRequiredNodeFeatureMissing{},
	&FailPermanentChannelFailure{},
	&FailRequiredChannelFeatureMissing{},
	&FailUnknownNextPeer{},
	&FailUnknownPaymentHash{},
	&FailIncorrectPaymentAmount{},
	&FailFinalExpiryTooSoon{},

	NewInvalidOnionVersion(testOnionHash),
	NewInvalidOnionHmac(testOnionHash),
	NewInvalidOnionKey(testOnionHash),
	NewTemporaryChannelFailure(&testChannelUpdate),
	NewTemporaryChannelFailure(nil),
	NewAmountBelowMinimum(testAmount, testChannelUpdate),
	NewFeeInsufficient(testAmount, testChannelUpdate),
	NewIncorrectCltvExpiry(testCtlvExpiry, testChannelUpdate),
	NewExpiryTooSoon(testChannelUpdate),
	NewChannelDisabled(testFlags, testChannelUpdate),
	NewFinalIncorrectCltvExpiry(testCtlvExpiry),
	NewFinalIncorrectHtlcAmount(testAmount),
}

// TestEncodeDecodeCode tests the ability of onion errors to be properly encoded
// and decoded.
func TestEncodeDecodeCode(t *testing.T) {
	for _, failure1 := range onionFailures {
		var b bytes.Buffer

		if err := EncodeFailure(&b, failure1, 0); err != nil {
			t.Fatalf("unable to encode failure code(%v): %v",
				failure1.Code(), err)
		}

		failure2, err := DecodeFailure(&b, 0)
		if err != nil {
			t.Fatalf("unable to decode failure code(%v): %v",
				failure1.Code(), err)
		}

		if !reflect.DeepEqual(failure1, failure2) {
			t.Fatalf("failure message are different, failure "+
				"code(%v)", failure1.Code())
		}
	}
}
