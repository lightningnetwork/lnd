package lnwire

import (
	"bytes"
	"reflect"
	"testing"
)

func TestPaymentInitiationConfirmationEncodeDecode(t *testing.T) {
	var invN, redemH [32]byte
	invN[1] = 1
	redemH[1] = 2

	// First create a new PaymentInitiationConfirmation message.
	initConfReq := &PaymentInitiationConfirmation{
		InvoiceNumber: invN,
		RedemptionHash: redemH,
	}

	// Next encode the PaymentInitiationConfirmation message into an empty bytes buffer.
	var b bytes.Buffer
	if err := initConfReq.Encode(&b, 0); err != nil {
		t.Fatalf("unable to encode PaymentInitiationConfirmation: %v", err)
	}

	// Deserialize the encoded PaymentInitiationConfirmation message into a new empty struct.
	initConfReq2 := &PaymentInitiationConfirmation{}
	if err := initConfReq2.Decode(&b, 0); err != nil {
		t.Fatalf("unable to decode PaymentInitiationConfirmation: %v", err)
	}

	// Assert equality of the two instances.
	if !reflect.DeepEqual(initConfReq, initConfReq2) {
		t.Fatalf("encode/decode error messages don't match %#v vs %#v",
			initConfReq, initConfReq2)
	}
}
