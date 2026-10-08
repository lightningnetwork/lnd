package migration1

import (
	"bytes"
	"testing"

	"github.com/lightningnetwork/lnd/graph/db/migration1/models"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// TestEdgePolicyOpaqueDataRoundTrip asserts that the migration reader accepts
// the largest opaque policy blob that the legacy writer can produce.
func TestEdgePolicyOpaqueDataRoundTrip(t *testing.T) {
	var (
		extra   bytes.Buffer
		scratch [8]byte
	)
	require.NoError(t, tlv.WriteVarInt(&extra, 1, &scratch))
	require.NoError(t, tlv.WriteVarInt(&extra, 9996, &scratch))
	_, err := extra.Write(make([]byte, 9996))
	require.NoError(t, err)
	require.Equal(t, MaxAllowedExtraOpaqueBytes, extra.Len())

	policy := &models.ChannelEdgePolicy{
		MessageFlags:    lnwire.ChanUpdateRequiredMaxHtlc,
		ExtraOpaqueData: extra.Bytes(),
	}

	var encoded bytes.Buffer
	require.NoError(t, serializeChanEdgePolicy(
		&encoded, policy, policy.ToNode[:],
	))

	decoded, err := deserializeChanEdgePolicy(&encoded)
	require.NoError(t, err)
	require.Equal(t, policy.MessageFlags, decoded.MessageFlags)
	require.Equal(t, policy.MaxHTLC, decoded.MaxHTLC)
	require.Equal(t, policy.ExtraOpaqueData, decoded.ExtraOpaqueData)
}
