package lnwire

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/roasbeef/btcutil"
)

func TestFundingResponseEncodeDecode(t *testing.T) {
	copy(revocationHash[:], revocationHashBytes)

	// funding response
	fr := &FundingResponse{
		ChannelType:            uint8(1),
		ReservationID:          uint64(12345678),
		ResponderFundingAmount: btcutil.Amount(100000000),
		ResponderReserveAmount: btcutil.Amount(131072),
		MinFeePerKb:            btcutil.Amount(20000),
		MinDepth:               uint32(6),
		LockTime:               uint32(4320), // 30 block-days
		FeePayer:               uint8(1),
		RevocationHash:         revHash,
		Pubkey:                 pubKey,
		CommitSig:              commitSig,
		DeliveryPkScript:       deliveryPkScript,
		ChangePkScript:         changePkScript,
		Inputs:                 inputs,
	}

	// Next encode the FR message into an empty bytes buffer.
	var b bytes.Buffer
	if err := fr.Encode(&b, 0); err != nil {
		t.Fatalf("unable to encode HTLCAddRequest: %v", err)
	}

	// Deserialize the encoded FR message into a new empty struct.
	fr2 := &FundingResponse{}
	if err := fr2.Decode(&b, 0); err != nil {
		t.Fatalf("unable to decode FundingResponse: %v", err)
	}

	// Assert equality of the two instances.
	if !reflect.DeepEqual(fr, fr2) {
		t.Fatalf("encode/decode error messages don't match %#v vs %#v",
			fr, fr2)
	}
}
