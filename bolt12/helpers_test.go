package bolt12

import (
	"bytes"
	"encoding/json"
	"os"
	"sync"
	"testing"

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
