package autopilot_test

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/autopilot"
)

// randKey returns a random public key.
func randKey() (*btcec.PublicKey, error) {
	priv, err := btcec.NewPrivateKey()
	if err != nil {
		return nil, err
	}

	return priv.PubKey(), nil
}

// TestSetNodeScores tests that the scores returned by the
// ExternalScoreAttachment correctly reflects the scores we set last.
func TestSetNodeScores(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	const name = "externalscore"

	h := autopilot.NewExternalScoreAttachment()

	// Create a list of random node IDs.
	const numKeys = 20
	var pubkeys []autopilot.NodeID
	for i := 0; i < numKeys; i++ {
		k, err := randKey()
		if err != nil {
			t.Fatal(err)
		}

		nID := autopilot.NewNodeID(k)
		pubkeys = append(pubkeys, nID)
	}

	// Set the score of half of the nodes.
	scores := make(map[autopilot.NodeID]float64)
	for i := 0; i < numKeys/2; i++ {
		nID := pubkeys[i]
		scores[nID] = 0.05 * float64(i)
	}

	applied, err := h.SetNodeScores(name, scores)
	if err != nil {
		t.Fatal(err)
	}

	if !applied {
		t.Fatalf("scores were not applied")
	}

	// Query all scores, half should be set, half should be zero.
	q := make(map[autopilot.NodeID]struct{})
	for _, nID := range pubkeys {
		q[nID] = struct{}{}
	}
	resp, err := h.NodeScores(
		ctx, nil, nil, btcutil.Amount(btcutil.SatoshiPerBitcoin), q,
	)
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < numKeys; i++ {
		var expected float64
		if i < numKeys/2 {
			expected = 0.05 * float64(i)
		}
		nID := pubkeys[i]

		var score float64
		if s, ok := resp[nID]; ok {
			score = s.Score
		}

		if score != expected {
			t.Fatalf("expected score %v, got %v",
				expected, score)
		}

	}

	// Try to apply scores with bogus name, should not be applied.
	applied, err = h.SetNodeScores("dummy", scores)
	if err != nil {
		t.Fatal(err)
	}

	if applied {
		t.Fatalf("scores were applied")
	}

}
