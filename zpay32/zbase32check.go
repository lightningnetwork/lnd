package zpay32

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"

	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcutil"
	"github.com/tv42/zbase32"
)

// invoiceSize is the size of an encoded invoice without the added check-sum.
// The size of broken down as follows: 33-bytes (destination pub key), 32-bytes
// (payment hash), 8-bytes for the payment amount in satoshis.
const invoiceSize = 33 + 32 + 8

// ErrCheckSumMismatch is returned byt he Decode function fi when
// decoding an encoded invoice, the checksum doesn't match indicating
// an error somewhere in the bitstream.
var ErrCheckSumMismatch = errors.New("the checksum is incorrect")

// PaymentRequest is a bare-bones invoice for a payment within the Lightning
// Network.  With the details of the invoice, the sender has all the data
// necessary to send a payment to the recipient.
type PaymentRequest struct {
	// Destination is the public key of the node to be paid.
	Destination *btcec.PublicKey

	// PaymentHash is the has to use within the HTLC extended throughout
	// the payment path to the destination.
	PaymentHash [32]byte

	// Amount is the amount to be sent to the destination expressed in
	// satoshis.
	Amount btcutil.Amount
}

// castagnoli is an initialized crc32 checksum generated which Castagnoli's
// polynomial.
var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// checkSum calculates a 4-byte crc32 checksum of the passed data. The returned
// uint32 is serialized as a big-endian integer.
func checkSum(data []byte) []byte {
	crc := crc32.New(castagnoli)
	crc.Write(data)
	return crc.Sum(nil)
}

// Encode encodes the passed payment request using zbase32 with an added 4-byte
// crc32 checksum. The resulting encoding is 77-bytes long and consists of 124
// ASCII characters.
// TODO(roasbeef): add version byte?
func Encode(payReq *PaymentRequest) string {
	var (
		invoiceBytes [invoiceSize]byte
		n            int
	)

	// First copy each of the elements of the payment request into the
	// buffer. Creating a stream that resembles: dest || r_hash || amt
	n += copy(invoiceBytes[:], payReq.Destination.SerializeCompressed())
	n += copy(invoiceBytes[n:], payReq.PaymentHash[:])
	binary.BigEndian.PutUint64(invoiceBytes[n:], uint64(payReq.Amount))

	// Next, we append the checksum to the end of the buffer which covers
	// the serialized payment request.
	b := append(invoiceBytes[:], checkSum(invoiceBytes[:])...)

	// Finally encode the raw bytes as a zbase32 encoded string.
	return zbase32.EncodeToString(b)
}

// Decode attempts to decode the zbase32 encoded payment request. If the
// trailing checksum doesn't match, then an error is returned.
func Decode(payData string) (*PaymentRequest, error) {
	// First we decode the zbase32 encoded string into a series of raw
	// bytes.
	payReqBytes, err := zbase32.DecodeString(payData)
	if err != nil {
		return nil, err
	}

	// With the bytes decoded, we first verify the checksum to ensure the
	// payment request wasn't altered in its decoded form.
	invoiceBytes := payReqBytes[:invoiceSize]
	generatedSum := checkSum(invoiceBytes)

	// If the checksums don't match, then we return an error to the
	// possibly detected error.
	encodedSum := payReqBytes[invoiceSize:]
	if !bytes.Equal(encodedSum, generatedSum) {
		return nil, ErrCheckSumMismatch
	}

	// Otherwise, we've verified the integrity of the encoded payment
	// request and can safely decode the payReq, passing it back up to the
	// caller.
	invoiceReader := bytes.NewReader(invoiceBytes)
	return decodePaymentRequest(invoiceReader)
}

func decodePaymentRequest(r io.Reader) (*PaymentRequest, error) {
	var err error

	i := &PaymentRequest{}

	var pubKey [33]byte
	if _, err := io.ReadFull(r, pubKey[:]); err != nil {
		return nil, err
	}

	i.Destination, err = btcec.ParsePubKey(pubKey[:], btcec.S256())
	if err != nil {
		return nil, err
	}

	if _, err := io.ReadFull(r, i.PaymentHash[:]); err != nil {
		return nil, err
	}

	var amt [8]byte
	if _, err := io.ReadFull(r, amt[:]); err != nil {
		return nil, err
	}

	i.Amount = btcutil.Amount(binary.BigEndian.Uint64(amt[:]))

	return i, nil
}
