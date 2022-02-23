package lnwire

import (
	"fmt"
	"math/big"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
)

func TestSignatureSerializeDeserialize(t *testing.T) {
	t.Parallel()

	// Local-scoped closure to serialize and deserialize a Signature and
	// check for errors as well as check if the results are correct.
	signatureSerializeDeserialize := func(e *ecdsa.Signature) error {
		sig, err := NewSigFromSignature(e)
		if err != nil {
			return err
		}

		e2, err := sig.ToSignature()
		if err != nil {
			return err
		}

		if !e.IsEqual(e2) {
			return fmt.Errorf("pre/post-serialize sigs don't " +
				"match")
		}
		return nil
	}

	// Check R = N-1, S = 128.
	r := big.NewInt(1) // Allocate a big.Int before we call .Sub.
	r.Sub(btcec.S256().N, r)
	rScalar := new(btcec.ModNScalar)
	rScalar.SetByteSlice(r.Bytes())

	sig := ecdsa.NewSignature(rScalar, new(btcec.ModNScalar).SetInt(128))
	err := signatureSerializeDeserialize(sig)
	if err != nil {
		t.Fatalf("R = N-1, S = 128: %s", err.Error())
	}

	// Check R = N-1, S = 127.
	sig = ecdsa.NewSignature(rScalar, new(btcec.ModNScalar).SetInt(127))
	err = signatureSerializeDeserialize(sig)
	if err != nil {
		t.Fatalf("R = N-1, S = 127: %s", err.Error())
	}

	// Check R = N-1, S = N>>1.
	s := new(big.Int).Set(btcec.S256().N)
	s.Rsh(s, 1)
	sScalar := new(btcec.ModNScalar)
	sScalar.SetByteSlice(s.Bytes())
	sig = ecdsa.NewSignature(rScalar, sScalar)
	err = signatureSerializeDeserialize(sig)
	if err != nil {
		t.Fatalf("R = N-1, S = N>>1: %s", err.Error())
	}

	// Check R = N-1, S = N.
	s = new(big.Int).Set(btcec.S256().N)
	overflow := sScalar.SetByteSlice(s.Bytes())
	if !overflow {
		t.Fatalf("Expect ModNScalar to overflow when setting N but " +
			"didn't")
	}
	sig = ecdsa.NewSignature(rScalar, sScalar)
	err = signatureSerializeDeserialize(sig)
	if err.Error() != "invalid signature: S is 0" {
		t.Fatalf("R = N-1, S = N should become R = N-1, S = 0: %s",
			err.Error())
	}

	// Check R = N-1, S = N-1.
	s = new(big.Int).Set(btcec.S256().N)
	s.Sub(s, big.NewInt(1))
	sScalar.SetByteSlice(s.Bytes())
	sig = ecdsa.NewSignature(rScalar, sScalar)
	err = signatureSerializeDeserialize(sig)
	if err.Error() != "pre/post-serialize sigs don't match" {
		t.Fatalf("R = N-1, S = N-1 should become R = N-1, S = 1: %s",
			err.Error())
	}

	// Check R = 2N, S = 128
	// This cannot be tested anymore since the new ecdsa package creates
	// the signature from ModNScalar values which don't allow setting a
	// value larger than N (hence the name mod n).
}
