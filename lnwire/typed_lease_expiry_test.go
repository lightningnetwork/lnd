package lnwire

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestChannelTypeEncodeDecode tests that we're able to properly encode and
// decode channel types within TLV streams.
func TestLeaseExpiryEncodeDecode(t *testing.T) {
	t.Parallel()

	leaseExpiry := LeaseExpiry(1337)

	var extraData ExtraOpaqueData
	require.NoError(t, extraData.PackRecords(&leaseExpiry))

	var leaseExpiry2 LeaseExpiry
	tlvs, err := extraData.ExtractRecords(&leaseExpiry2)
	require.NoError(t, err)

	require.Contains(t, tlvs, LeaseExpiryRecordType)
	require.Equal(t, leaseExpiry, leaseExpiry2)
}
