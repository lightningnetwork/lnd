package zpay32

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/stretchr/testify/require"
)

// bolt11VectorFile holds the invoice test vectors published alongside BOLT 11
// in the spec repository (bolt11/invoice-test.json).
const bolt11VectorFile = "test-vectors/invoice-test.json"

// bolt11Vector is a single entry of the BOLT 11 invoice test vectors.
type bolt11Vector struct {
	Description string `json:"description"`
	Valid       bool   `json:"valid"`
	Invoice     string `json:"invoice"`
}

// paymentLayerVectors lists the invalid vectors that Decode accepts on
// purpose, as lnd enforces the rule they exercise when the invoice is paid
// rather than when it's decoded. Each value is the reason.
var paymentLayerVectors = map[string]string{
	// The `s` field is optional at the decoding layer, since lnd creates
	// invoices with blinded paths and no payment address, and older
	// stored invoices may predate payment addresses. The router rejects
	// a payment request with neither a payment address nor blinded paths.
	"Missing required `s` field": "payment address checked by the router",
}

// TestBOLT11Vectors runs every invoice test vector from the spec through
// Decode. Unknown required feature bits are rejected through the
// WithErrorOnUnknownFeatureBit option, as by default they're left for
// pathfinding to reject so that the invoice can still be inspected.
func TestBOLT11Vectors(t *testing.T) {
	t.Parallel()

	jsonData, err := os.ReadFile(bolt11VectorFile)
	require.NoError(t, err)

	var vectors []bolt11Vector
	require.NoError(t, json.Unmarshal(jsonData, &vectors))
	require.NotEmpty(t, vectors)

	for _, v := range vectors {
		t.Run(v.Description, func(t *testing.T) {
			t.Parallel()

			net := &chaincfg.MainNetParams
			hrp := strings.ToLower(v.Invoice)
			if strings.HasPrefix(hrp, "lntb") {
				net = &chaincfg.TestNet3Params
			}

			invoice, err := Decode(
				v.Invoice, net, WithErrorOnUnknownFeatureBit(),
			)

			if _, ok := paymentLayerVectors[v.Description]; ok {
				require.False(t, v.Valid)
				require.NoError(t, err)
				require.True(t, invoice.PaymentAddr.IsNone())

				return
			}

			if v.Valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}
