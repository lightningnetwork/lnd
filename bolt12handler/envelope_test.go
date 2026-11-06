package bolt12handler

import (
	"bytes"
	"testing"

	"github.com/lightningnetwork/lnd/bolt12handler/envelope"
	"github.com/stretchr/testify/require"
)

// TestEnvelopeBinding verifies that a change of any envelope byte changes the
// binding.
func TestEnvelopeBinding(t *testing.T) {
	t.Parallel()

	encoded, err := envelope.Encode(&envelope.Data{
		CreatedAt: 1, Amount: 2, Quantity: 3,
	})
	require.NoError(t, err)

	binding := EnvelopeBinding(encoded)
	for i := range encoded {
		changed := bytes.Clone(encoded)
		changed[i] ^= 1
		require.NotEqual(t, binding, EnvelopeBinding(changed))
	}
}
