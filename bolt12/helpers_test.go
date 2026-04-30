package bolt12

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// bobKey returns the deterministic spec test key for Bob, whose 32-byte scalar
// is 0x42 repeated. Used across signature and round-trip tests so the same key
// is not reconstructed in every callsite.
func bobKey() (*btcec.PrivateKey, *btcec.PublicKey) {
	priv, pub := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{0x42}, 32))

	return priv, pub
}

// aliceKey returns the deterministic spec test key for Alice, whose 32-byte
// scalar is 0x41 repeated.
func aliceKey() (*btcec.PrivateKey, *btcec.PublicKey) {
	priv, pub := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{0x41}, 32))

	return priv, pub
}

// formatStringTestVector represents a single test case from the BOLT 12
// format-string-test.json file.
type formatStringTestVector struct {
	Comment string `json:"comment"`
	Valid   bool   `json:"valid"`
	String  string `json:"string"`
}

// loadFormatStringVectorsOnce parses format-string-test.json once.
var loadFormatStringVectorsOnce = sync.OnceValues(
	func() ([]formatStringTestVector, error) {
		data, err := os.ReadFile(
			"test-vectors/format-string-test.json",
		)
		if err != nil {
			return nil, err
		}

		var vectors []formatStringTestVector
		if err := json.Unmarshal(data, &vectors); err != nil {
			return nil, err
		}

		return vectors, nil
	},
)

// loadFormatStringVectors returns the parsed format-string-test.json vectors.
func loadFormatStringVectors(t *testing.T) []formatStringTestVector {
	t.Helper()

	vectors, err := loadFormatStringVectorsOnce()
	require.NoError(t, err)

	return vectors
}

// farFutureNow returns a time well past every spec fixture's expiry, so
// structural validation runs without expiry interference.
func farFutureNow() time.Time {
	return time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
}

// offersTestVector represents a single test case from offers-test.json.
type offersTestVector struct {
	Description string            `json:"description"`
	Valid       bool              `json:"valid"`
	Bolt12      string            `json:"bolt12"`
	Fields      []offersTestField `json:"fields"`
}

// offersTestField represents an expected TLV field in the test vector.
type offersTestField struct {
	Type   uint64 `json:"type"`
	Length uint64 `json:"length"`
	Hex    string `json:"hex"`
}

// loadOffersVectorsOnce parses test-vectors/offers-test.json once and memoizes
// the result for all callers.
var loadOffersVectorsOnce = sync.OnceValues(
	func() ([]offersTestVector, error) {
		data, err := os.ReadFile("test-vectors/offers-test.json")
		if err != nil {
			return nil, err
		}

		var vectors []offersTestVector
		if err := json.Unmarshal(data, &vectors); err != nil {
			return nil, err
		}

		return vectors, nil
	},
)

// loadOffersVectors returns the parsed offers-test.json vectors, failing the
// test if the file is unreadable or malformed.
func loadOffersVectors(t testing.TB) []offersTestVector {
	t.Helper()

	vectors, err := loadOffersVectorsOnce()
	require.NoError(t, err)

	return vectors
}

// findTestVector returns the offers-test.json vector matching desc, failing
// the test if no match is found.
func findTestVector(t *testing.T, desc string) offersTestVector {
	t.Helper()

	for _, v := range loadOffersVectors(t) {
		if v.Description == desc {
			return v
		}
	}

	t.Fatalf("test vector not found: %s", desc)

	return offersTestVector{}
}

// streamToRecords parses an arbitrary TLV byte stream into tlv.Record values
// whose Encode method reproduces the original wire bytes, without going
// through a typed message decoder.
func streamToRecords(t *testing.T, data []byte) []tlv.Record {
	t.Helper()

	stream, err := tlv.NewStream()
	require.NoError(t, err)

	typeMap, err := stream.DecodeWithParsedTypesP2P(bytes.NewReader(data))
	require.NoError(t, err)

	return lnwire.TlvMapToRecords(typeMap)
}

// recordFromWireBytes builds a single tlv.Record whose encoding is the
// supplied full TLV byte slice. The slice must be a complete
// type+length+value sequence. Inputs are trusted spec fixtures, so the
// length prefix is allocated without a bound.
func recordFromWireBytes(t *testing.T, full []byte) tlv.Record {
	t.Helper()

	var buf [8]byte
	r := bytes.NewReader(full)

	typ, err := tlv.ReadVarInt(r, &buf)
	require.NoError(t, err)

	length, err := tlv.ReadVarInt(r, &buf)
	require.NoError(t, err)

	value := make([]byte, length)
	_, err = io.ReadFull(r, value)
	require.NoError(t, err)

	return tlv.MakePrimitiveRecord(tlv.Type(typ), &value)
}

// sigTestVector represents a test case from signature-test.json.
type sigTestVector struct {
	Comment string `json:"comment"`
	TLV     string `json:"tlv"`
	Bolt12  string `json:"bolt12"`

	//nolint:tagliatelle // BOLT 12 spec vector key.
	FirstTLV string            `json:"first-tlv"`
	Leaves   []json.RawMessage `json:"leaves"`
	Branches []json.RawMessage `json:"branches"`
	Merkle   string            `json:"merkle"`

	SignatureTag string `json:"signature_tag"`
	Signature    string `json:"signature"`
}

// readSignatureDataOnce reads signature-test.json once so the file is
// parsed only once per test process.
var readSignatureDataOnce = sync.OnceValues(func() ([]byte, error) {
	return os.ReadFile("test-vectors/signature-test.json")
})

// loadSignatureVectorsOnce parses signature-test.json into typed
// sigTestVectors. The raw-JSON loader is separate because the JSON
// contains a key ("H(signature_tag,merkle)") that cannot be expressed
// via Go struct tags.
var loadSignatureVectorsOnce = sync.OnceValues(
	func() ([]sigTestVector, error) {
		data, err := readSignatureDataOnce()
		if err != nil {
			return nil, err
		}

		var vectors []sigTestVector
		if err := json.Unmarshal(data, &vectors); err != nil {
			return nil, err
		}

		return vectors, nil
	},
)

// loadSignatureVectors returns the parsed sigTestVector slice, failing the
// test if signature-test.json is unreadable or malformed.
func loadSignatureVectors(t testing.TB) []sigTestVector {
	t.Helper()

	vectors, err := loadSignatureVectorsOnce()
	require.NoError(t, err)

	return vectors
}

// loadSignatureRawOnce parses signature-test.json as a slice of raw
// json.RawMessage so callers can index into keys whose names cannot be
// expressed via struct tags.
var loadSignatureRawOnce = sync.OnceValues(
	func() ([]json.RawMessage, error) {
		data, err := readSignatureDataOnce()
		if err != nil {
			return nil, err
		}

		var raw []json.RawMessage
		if err := json.Unmarshal(data, &raw); err != nil {
			return nil, err
		}

		return raw, nil
	},
)

// loadSignatureRawVectors returns the raw json.RawMessage view of
// signature-test.json, failing the test if the file is unreadable or
// malformed.
func loadSignatureRawVectors(t *testing.T) []json.RawMessage {
	t.Helper()

	raw, err := loadSignatureRawOnce()
	require.NoError(t, err)

	return raw
}
