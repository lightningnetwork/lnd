package lnwire

import (
	"fmt"
	"math/big"
	"testing"

	"github.com/btcsuite/btcd/btcec"
)

func TestSignatureSerializeDeserialize(t *testing.T) {
	t.Parallel()

	// Local-scoped closure to serialize and deserialize a Signature and
	// check for errors as well as check if the results are correct.
	signatureSerializeDeserialize := func(e btcec.Signature) error {
		sig, err := NewSigFromSignature(&e)
		if err != nil {
			return err
		}

		e2, err := sig.ToSignature()
		if err != nil {
			return err
		}

		if e.R.Cmp(e2.R) != 0 {
			return fmt.Errorf("Pre/post-serialize Rs don't match"+
				": %s, %s", e.R, e2.R)
		}
		if e.S.Cmp(e2.S) != 0 {
			return fmt.Errorf("Pre/post-serialize Ss don't match"+
				": %s, %s", e.S, e2.S)
		}
		return nil
	}

	sig := btcec.Signature{}

	// Check R = N-1, S = 128.
	sig.R = big.NewInt(1) // Allocate a big.Int before we call .Sub.
	sig.R.Sub(btcec.S256().N, sig.R)
	sig.S = big.NewInt(128)
	err := signatureSerializeDeserialize(sig)
	if err != nil {
		t.Fatalf("R = N-1, S = 128: %s", err.Error())
	}

	// Check R = N-1, S = 127.
	sig.S = big.NewInt(127)
	err = signatureSerializeDeserialize(sig)
	if err != nil {
		t.Fatalf("R = N-1, S = 127: %s", err.Error())
	}

	// Check R = N-1, S = N>>1.
	sig.S.Set(btcec.S256().N)
	sig.S.Rsh(sig.S, 1)
	err = signatureSerializeDeserialize(sig)
	if err != nil {
		t.Fatalf("R = N-1, S = N>>1: %s", err.Error())
	}

	// Check R = N-1, S = N.
	sig.S.Set(btcec.S256().N)
	err = signatureSerializeDeserialize(sig)
	if err.Error() != "signature S isn't 1 or more" {
		t.Fatalf("R = N-1, S = N should become R = N-1, S = 0: %s",
			err.Error())
	}

	// Check R = N-1, S = N-1.
	sig.S.Sub(sig.S, big.NewInt(1))
	err = signatureSerializeDeserialize(sig)
	if err.Error() != "Pre/post-serialize Ss don't match: 115792089237316"+
		"195423570985008687907852837564279074904382605163141518161494"+
		"336, 1" {
		t.Fatalf("R = N-1, S = N-1 should become R = N-1, S = 1: %s",
			err.Error())
	}

	// Check R = 2N, S = 128
	sig.R.Mul(btcec.S256().N, big.NewInt(2))
	sig.S.Set(big.NewInt(127))
	err = signatureSerializeDeserialize(sig)
	if err.Error() != "R is over 32 bytes long without padding" {
		t.Fatalf("R = 2N, S = 128, R should be over 32 bytes: %s",
			err.Error())
	}
}

var (
	// signatures from bitcoin blockchain tx
	// 0437cd7f8525ceed2324359c2d0ba26006d92d85.
	normalSig = []byte{
		0x30, 0x44, 0x02, 0x20,
		// r value
		0x4e, 0x45, 0xe1, 0x69, 0x32, 0xb8, 0xaf, 0x51,
		0x49, 0x61, 0xa1, 0xd3, 0xa1, 0xa2, 0x5f, 0xdf,
		0x3f, 0x4f, 0x77, 0x32, 0xe9, 0xd6, 0x24, 0xc6,
		0xc6, 0x15, 0x48, 0xab, 0x5f, 0xb8, 0xcd, 0x41,

		0x02, 0x20,
		// s value
		0x18, 0x15, 0x22, 0xec, 0x8e, 0xca, 0x07, 0xde,
		0x48, 0x60, 0xa4, 0xac, 0xdd, 0x12, 0x90, 0x9d,
		0x83, 0x1c, 0xc5, 0x6c, 0xbb, 0xac, 0x46, 0x22,
		0x08, 0x22, 0x21, 0xa8, 0x76, 0x8d, 0x1d, 0x09,
	}

	// minimal length with 1 byte r and 1 byte s.
	minSig = []byte{
		0x30, 0x06, 0x02, 0x01, 0x00, 0x02, 0x01, 0x00,
	}

	// sig length is below 6.
	smallLenSig = []byte{
		0x30, 0x05, 0x02, 0x01, 0x00, 0x02, 0x01, 0x00,
	}

	// sig length is above 6.
	largeLenSig = []byte{
		0x30, 0x07, 0x02, 0x01, 0x00, 0x02, 0x01, 0x00,
	}

	// r length is 2.
	largeRSig = []byte{
		0x30, 0x06, 0x02, 0x02, 0x00, 0x02, 0x01, 0x00,
	}

	// r length is 0.
	smallRSig = []byte{
		0x30, 0x06, 0x02, 0x00, 0x00, 0x02, 0x01, 0x00,
	}

	// s length is 2.
	largeSSig = []byte{
		0x30, 0x06, 0x02, 0x01, 0x00, 0x02, 0x02, 0x00,
	}

	// s length is 0.
	smallSSig = []byte{
		0x30, 0x06, 0x02, 0x01, 0x00, 0x02, 0x00, 0x00,
	}

	// r length is 33.
	missPaddingRSig = []byte{
		0x30, 0x25, 0x02, 0x21,
		// r value with a wrong padding.
		0xff,
		0x4e, 0x45, 0xe1, 0x69, 0x32, 0xb8, 0xaf, 0x51,
		0x49, 0x61, 0xa1, 0xd3, 0xa1, 0xa2, 0x5f, 0xdf,
		0x3f, 0x4f, 0x77, 0x32, 0xe9, 0xd6, 0x24, 0xc6,
		0xc6, 0x15, 0x48, 0xab, 0x5f, 0xb8, 0xcd, 0x41,
		// s value is 0.
		0x02, 0x01, 0x00,
	}

	// s length is 33.
	missPaddingSSig = []byte{
		// r value is 0.
		0x30, 0x25, 0x02, 0x01, 0x00,
		0x02, 0x21,
		// s value with a wrong padding.
		0xff,
		0x18, 0x15, 0x22, 0xec, 0x8e, 0xca, 0x07, 0xde,
		0x48, 0x60, 0xa4, 0xac, 0xdd, 0x12, 0x90, 0x9d,
		0x83, 0x1c, 0xc5, 0x6c, 0xbb, 0xac, 0x46, 0x22,
		0x08, 0x22, 0x21, 0xa8, 0x76, 0x8d, 0x1d, 0x09,
	}
)

func TestNewSigFromRawSignature(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name        string
		rawSig      []byte
		expectedErr error
		expectedSig Sig
	}{
		{
			name:        "valid signature",
			rawSig:      normalSig,
			expectedErr: nil,
			expectedSig: Sig{
				// r value
				0x4e, 0x45, 0xe1, 0x69, 0x32, 0xb8, 0xaf, 0x51,
				0x49, 0x61, 0xa1, 0xd3, 0xa1, 0xa2, 0x5f, 0xdf,
				0x3f, 0x4f, 0x77, 0x32, 0xe9, 0xd6, 0x24, 0xc6,
				0xc6, 0x15, 0x48, 0xab, 0x5f, 0xb8, 0xcd, 0x41,
				// s value
				0x18, 0x15, 0x22, 0xec, 0x8e, 0xca, 0x07, 0xde,
				0x48, 0x60, 0xa4, 0xac, 0xdd, 0x12, 0x90, 0x9d,
				0x83, 0x1c, 0xc5, 0x6c, 0xbb, 0xac, 0x46, 0x22,
				0x08, 0x22, 0x21, 0xa8, 0x76, 0x8d, 0x1d, 0x09,
			},
		},
		{
			name:        "minimal length signature",
			rawSig:      minSig,
			expectedErr: nil,
			// NOTE: r and s are both 0x00 here.
			expectedSig: Sig{},
		},
		{
			name:        "signature length too short",
			rawSig:      []byte{0x30},
			expectedErr: errSigTooShort,
			expectedSig: Sig{},
		},
		{
			name:        "sig length too large",
			rawSig:      largeLenSig,
			expectedErr: errBadLength,
			expectedSig: Sig{},
		},
		{
			name:        "sig length too small",
			rawSig:      smallLenSig,
			expectedErr: errBadLength,
			expectedSig: Sig{},
		},
		{
			name:        "r length too large",
			rawSig:      largeRSig,
			expectedErr: errBadRLength,
			expectedSig: Sig{},
		},
		{
			name:        "r length too small",
			rawSig:      smallRSig,
			expectedErr: errBadRLength,
			expectedSig: Sig{},
		},
		{
			name:        "s length too large",
			rawSig:      largeSSig,
			expectedErr: errBadSLength,
			expectedSig: Sig{},
		},
		{
			name:        "s length too small",
			rawSig:      smallSSig,
			expectedErr: errBadSLength,
			expectedSig: Sig{},
		},
		{
			name:        "missing padding in r",
			rawSig:      missPaddingRSig,
			expectedErr: errRTooLong,
			expectedSig: Sig{},
		},
		{
			name:        "missing padding in s",
			rawSig:      missPaddingSSig,
			expectedErr: errSTooLong,
			expectedSig: Sig{},
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			result, err := NewSigFromRawSignature(tc.rawSig)
			require.Equal(t, tc.expectedErr, err)
			require.Equal(t, tc.expectedSig, result)
		})
	}
}
