package lnwire

import (
	"bytes"
	"reflect"
	"testing"
)

func TestPaymentInitiationEncodeDecode(t *testing.T) {
	var invN [32]byte
	invN[1] = 1
	// First create a new PaymentInitiation message.
	initReq := &PaymentInitiation{
		Amount: CreditsAmount(1001),
		InvoiceNumber: invN,
		Memo: []byte("Some memo"),
	}

	// Next encode the PaymentInitiation message into an empty bytes buffer.
	var b bytes.Buffer
	if err := initReq.Encode(&b, 0); err != nil {
		t.Fatalf("unable to encode PaymentInitiation: %v", err)
	}

	// Deserialize the encoded PaymentInitiation message into a new empty struct.
	initReq2 := &PaymentInitiation{}
	if err := initReq2.Decode(&b, 0); err != nil {
		t.Fatalf("unable to decode PaymentInitiation: %v", err)
	}

	// Assert equality of the two instances.
	if !reflect.DeepEqual(initReq, initReq2) {
		t.Fatalf("encode/decode error messages don't match %#v vs %#v",
			initReq, initReq2)
	}
}
