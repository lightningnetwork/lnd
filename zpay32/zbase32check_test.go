package zpay32

import (
	"bytes"
	"testing"

	"github.com/davecgh/go-spew/spew"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcutil"
)

var (
	testPrivKey = []byte{
		0x81, 0xb6, 0x37, 0xd8, 0xfc, 0xd2, 0xc6, 0xda,
		0x63, 0x59, 0xe6, 0x96, 0x31, 0x13, 0xa1, 0x17,
		0xd, 0xe7, 0x95, 0xe4, 0xb7, 0x25, 0xb8, 0x4d,
		0x1e, 0xb, 0x4c, 0xfd, 0x9e, 0xc5, 0x8c, 0xe9,
	}

	_, testPubKey = btcec.PrivKeyFromBytes(btcec.S256(), testPrivKey)

	testPayHash = [32]byte{
		0xb7, 0x94, 0x38, 0x5f, 0x2d, 0x1e, 0xf7, 0xab,
		0x4d, 0x92, 0x73, 0xd1, 0x90, 0x63, 0x81, 0xb4,
		0x4f, 0x2f, 0x6f, 0x25, 0x88, 0xa3, 0xef, 0xb9,
		0x6a, 0x49, 0x18, 0x83, 0x31, 0x98, 0x47, 0x53,
	}
)

func TestEncodeDecode(t *testing.T) {
	t.Parallel()

	testPubKey.Curve = nil
	tests := []struct {
		version  int
		payReq   PaymentRequest
		encoding string
	}{
		{
			payReq: PaymentRequest{
				Destination: testPubKey,
				PaymentHash: testPayHash,
				Amount:      btcutil.Amount(50000),
			},
			encoding: "yj8p9uh793syszrd4giu66gtsqprp47cc6cqbdo3" +
				"qaxi7sr63y6bbphw8bx148zzipg3rh6t1btadpnxf7z1mnfd76" +
				"hsw1eaoca3ot4uyyyyyyyyydbib998je6o",
			version: 1,
		},
	}

	for i, test := range tests {
		// First ensure encoding the test payment request string in the
		// specified encoding.
		encodedReq := Encode(&test.payReq)
		if encodedReq != test.encoding {
			t.Fatalf("encoding mismatch for %v: expected %v got %v",
				spew.Sdump(test.payReq), test.encoding, encodedReq)
		}

		// Next ensure the correctness of the transformation in the
		// other direction.
		decodedReq, err := Decode(test.encoding)
		if err != nil {
			t.Fatalf("unable to decode invoice #%v: %v", i, err)
		}

		if !test.payReq.Destination.IsEqual(decodedReq.Destination) {
			t.Fatalf("test #%v:, destination mismatch for decoded request: "+
				"expected %v got %v", i, test.payReq.Destination,
				decodedReq.Destination)
		}
		if !bytes.Equal(test.payReq.PaymentHash[:], decodedReq.PaymentHash[:]) {
			t.Fatalf("test #%v: payment hash mismatch for decoded request: "+
				"expected %x got %x", i, test.payReq.PaymentHash,
				decodedReq.PaymentHash)
		}
		if test.payReq.Amount != decodedReq.Amount {
			t.Fatalf("test #%v: amount mismatch for decoded request: "+
				"expected %x got %x", i, test.payReq.Amount,
				decodedReq.Amount)
		}
	}
}

func TestChecksumMismatch(t *testing.T) {
	t.Parallel()

	// We start with a pre-encoded invoice, which has a valid checksum.
	payReqString := []byte("ycyr8brdjic6oak3bemztc5nupo56y3itq4z5q4qxwb35orf7fmj5phw8bx148zzipg3rh6t1btadpnxf7z1mnfd76hsw1eaoca3ot4uyyyyyyyyydbibt79jo1o")

	// To modify the resulting checksum, we shift a few of the bytes within the
	// string itself.
	payReqString[1] = 98
	payReqString[5] = 102

	if _, err := Decode(string(payReqString)); err != ErrCheckSumMismatch {
		t.Fatalf("decode should fail with checksum mismatch, instead: %v", err)
	}
}

func TestDecodeTooShort(t *testing.T) {
	t.Parallel()

	// We start with a pre-encoded too-short string.
	payReqString := "ycyr8brdji"

	if _, err := Decode(payReqString); err != ErrDataTooShort {
		t.Fatalf("decode should fail with data too short, instead: %v", err)
	}
}
