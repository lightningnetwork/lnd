package lnwire

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/roasbeef/btcutil"
)

func TestFundingRequestEncodeDecode(t *testing.T) {
	copy(revocationHash[:], revocationHashBytes)

	// funding request
	fr := &FundingRequest{
		ReservationID:          uint64(12345678),
		ChannelType:            uint8(0),
		RequesterFundingAmount: btcutil.Amount(100000000),
		RequesterReserveAmount: btcutil.Amount(131072),
		MinFeePerKb:            btcutil.Amount(20000),
		MinTotalFundingAmount:  btcutil.Amount(150000000),
		LockTime:               uint32(4320), // 30 block-days
		FeePayer:               uint8(0),
		PaymentAmount:          btcutil.Amount(1234567),
		MinDepth:               uint32(6),
		RevocationHash:         revocationHash,
		Pubkey:                 pubKey,
		DeliveryPkScript:       deliveryPkScript,
		ChangePkScript:         changePkScript,
		Inputs:                 inputs,
	}

	// Next encode the FR message into an empty bytes buffer.
	var b bytes.Buffer
	if err := fr.Encode(&b, 0); err != nil {
		t.Fatalf("unable to encode FundingRequest: %v", err)
	}

	// Deserialize the encoded FR message into a new empty struct.
	fr2 := &FundingRequest{}
	if err := fr2.Decode(&b, 0); err != nil {
		t.Fatalf("unable to decode FundingRequest: %v", err)
	}

	// Assert equality of the two instances.
	if !reflect.DeepEqual(fr, fr2) {
		t.Fatalf("encode/decode error messages don't match %#v vs %#v",
			fr, fr2)
	}
}
