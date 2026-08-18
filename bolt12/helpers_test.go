package bolt12

import (
	"bytes"
	"encoding/json"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
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
func loadOffersVectors(t *testing.T) []offersTestVector {
	t.Helper()

	vectors, err := loadOffersVectorsOnce()
	require.NoError(t, err)

	return vectors
}
