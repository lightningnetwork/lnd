package record_test

import (
	"bytes"
	"testing"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/tlv"
)

type recordEncDecTest struct {
	name      string
	encRecord func() tlv.RecordProducer
	decRecord func() tlv.RecordProducer
	assert    func(*testing.T, interface{})
}

var (
	testTotal      = lnwire.MilliSatoshi(45)
	testAddr       = [32]byte{0x01, 0x02}
	testShare      = [32]byte{0x03, 0x04}
	testSetID      = [32]byte{0x05, 0x06}
	testChildIndex = uint32(17)

	testBaseFee    uint32 = 100
	testFeeRate    uint32 = 100
	testCltvExpiry uint16 = 100

	testMinimumHtlc   uint64 = 1
	testMaxCltvExpiry uint32 = uint32(testCltvExpiry)
)

var recordEncDecTests = []recordEncDecTest{
	{
		name: "mpp",
		encRecord: func() tlv.RecordProducer {
			return record.NewMPP(testTotal, testAddr)
		},
		decRecord: func() tlv.RecordProducer {
			return new(record.MPP)
		},
		assert: func(t *testing.T, r interface{}) {
			mpp := r.(*record.MPP)
			if mpp.TotalMsat() != testTotal {
				t.Fatal("incorrect total msat")
			}
			if mpp.PaymentAddr() != testAddr {
				t.Fatal("incorrect payment addr")
			}
		},
	},
	{
		name: "amp",
		encRecord: func() tlv.RecordProducer {
			return record.NewAMP(
				testShare, testSetID, testChildIndex,
			)
		},
		decRecord: func() tlv.RecordProducer {
			return new(record.AMP)
		},
		assert: func(t *testing.T, r interface{}) {
			amp := r.(*record.AMP)
			if amp.RootShare() != testShare {
				t.Fatal("incorrect root share")
			}
			if amp.SetID() != testSetID {
				t.Fatal("incorrect set id")
			}
			if amp.ChildIndex() != testChildIndex {
				t.Fatal("incorrect child index")
			}
		},
	},
	{
		name: "route blinding payment relay",
		encRecord: func() tlv.RecordProducer {
			return &record.PaymentRelay{
				BaseFee:         testBaseFee,
				FeeRate:         testFeeRate,
				CltvExpiryDelta: testCltvExpiry,
			}
		},
		decRecord: func() tlv.RecordProducer {
			return new(record.PaymentRelay)
		},
		assert: func(t *testing.T, r interface{}) {
			paymentRelay := r.(*record.PaymentRelay)
			if paymentRelay.BaseFee != testBaseFee {
				t.Fatal("incorrect base fee")
			}
			if paymentRelay.FeeRate != testFeeRate {
				t.Fatal("incorrect fee rate")
			}
			if paymentRelay.CltvExpiryDelta != testCltvExpiry {
				t.Fatal("incorrect cltv expiry")
			}
		},
	},
	{
		name: "route blinding payment constraints",
		encRecord: func() tlv.RecordProducer {
			return &record.PaymentConstraints{
				MaxCltvExpiryDelta: testMaxCltvExpiry,
				HtlcMinimumMsat:    testMinimumHtlc,
				// AllowedFeatures:    []byte{},
			}
		},
		decRecord: func() tlv.RecordProducer {
			return new(record.PaymentConstraints)
		},
		assert: func(t *testing.T, r interface{}) {
			payConstraints := r.(*record.PaymentConstraints)
			if payConstraints.MaxCltvExpiryDelta != testMaxCltvExpiry {
				t.Fatal("incorrect blinded route expiry")
			}
			if payConstraints.HtlcMinimumMsat != testMinimumHtlc {
				t.Fatal("incorrect minimum htlc")
			}
			// if bytes.Equal(payConstraints.AllowedFeatures, []byte{}) {
			// 	t.Fatal("incorrect allowed features: ", payConstraints.AllowedFeatures)
			// }
		},
	},
}

// TestRecordEncodeDecode is a generic test framework for custom TLV records. It
// asserts that records can encode and decode themselves, and that the value of
// the original record matches the decoded record.
func TestRecordEncodeDecode(t *testing.T) {
	for _, test := range recordEncDecTests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			r := test.encRecord()
			r2 := test.decRecord()
			encStream := tlv.MustNewStream(r.Record())
			decStream := tlv.MustNewStream(r2.Record())

			test.assert(t, r)

			var b bytes.Buffer
			err := encStream.Encode(&b)
			if err != nil {
				t.Fatalf("unable to encode record: %v", err)
			}

			err = decStream.Decode(bytes.NewReader(b.Bytes()))
			if err != nil {
				t.Fatalf("unable to decode record: %v", err)
			}

			test.assert(t, r2)
		})
	}
}
