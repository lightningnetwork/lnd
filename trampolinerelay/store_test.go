package trampolinerelay

import (
	"bytes"
	"testing"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/routing/trampoline"
	"github.com/stretchr/testify/require"
)

// TestKVStoreRoundTrip verifies that relay identity components and terminal
// results survive the production database boundary.
func TestKVStoreRoundTrip(t *testing.T) {
	t.Parallel()

	db, err := channeldb.MakeTestDB(t)
	require.NoError(t, err)
	store, err := NewKVStore(db)
	require.NoError(t, err)

	id := trampoline.RelayID{}
	copy(id.PaymentHash[:], bytes.Repeat([]byte{0x42}, 32))
	copy(id.PaymentAddr[:], bytes.Repeat([]byte{0x24}, 32))
	preimage := [32]byte{}
	copy(preimage[:], bytes.Repeat([]byte{0x55}, 32))

	group := StoredGroup{
		OnionBytes:   []byte{1, 2, 3},
		PayloadBytes: []byte{4, 5, 6},
		NextOnion:    []byte{7, 8, 9},
		Role:         trampoline.RoleClearIntermediate,
		Snapshot: trampoline.RelaySnapshot{
			Version:           1,
			ID:                id,
			State:             trampoline.RelaySucceeded,
			ExpectedTotalMsat: 1_100,
			DownstreamMsat:    1_000,
			CollectedMsat:     1_100,
			RequiredCLTV:      110,
			StartHeight:       100,
			ObservedHeight:    100,
			PeakHeight:        100,
			HoldDeadline:      140,
			Attempts:          1,
			Parts: []trampoline.RelayPart{{
				ID: trampoline.PartID{
					ChannelID: 9,
					HTLCID:    1,
				},
				AmountMsat: 1_100,
				CLTV:       120,
				Resolution: trampoline.PartFulfilled,
			}},
		},
		Preimage: &preimage,
	}
	require.NoError(t, store.Put(id, group))

	loaded, err := store.Load(id)
	require.NoError(t, err)
	require.NotNil(t, loaded)
	require.Equal(t, group.OnionBytes, loaded.OnionBytes)
	require.Equal(t, group.PayloadBytes, loaded.PayloadBytes)
	require.Equal(t, group.NextOnion, loaded.NextOnion)
	require.Equal(t, group.Role, loaded.Role)
	require.Equal(t, group.Snapshot, loaded.Snapshot)
	require.Equal(t, group.Preimage, loaded.Preimage)
	records, err := store.List()
	require.NoError(t, err)
	require.Equal(t, []StoredGroupRecord{{ID: id, Group: *loaded}}, records)

	conflictID := id
	copy(
		conflictID.PaymentAddr[:], bytes.Repeat([]byte{0x25}, 32),
	)
	require.ErrorIs(
		t, store.Put(conflictID, group), ErrPaymentHashConflict,
	)
	loaded, err = store.Load(id)
	require.NoError(t, err)
	require.NotNil(t, loaded)

	require.NoError(t, store.Delete(id))
	require.NoError(t, store.Put(conflictID, group))
	require.NoError(t, store.Delete(conflictID))
	loaded, err = store.Load(id)
	require.NoError(t, err)
	require.Nil(t, loaded)
}

// TestMemoryStoreRejectsPaymentHashAlias verifies that the focused test store
// enforces the same single-group-per-payment-hash invariant as the production
// database.
func TestMemoryStoreRejectsPaymentHashAlias(t *testing.T) {
	t.Parallel()

	store := NewMemoryStore()
	first := trampoline.RelayID{}
	copy(first.PaymentHash[:], bytes.Repeat([]byte{0x42}, 32))
	copy(first.PaymentAddr[:], bytes.Repeat([]byte{0x24}, 32))
	second := first
	copy(second.PaymentAddr[:], bytes.Repeat([]byte{0x25}, 32))

	require.NoError(t, store.Put(first, StoredGroup{}))
	records, err := store.List()
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.Equal(t, first, records[0].ID)
	require.ErrorIs(
		t, store.Put(second, StoredGroup{}), ErrPaymentHashConflict,
	)
	require.NoError(t, store.Delete(first))
	require.NoError(t, store.Put(second, StoredGroup{}))
}

// TestStoredGroupRejectsMalformedData verifies that durable relay state is
// size-bounded, versioned, single-valued, and strict about unknown fields.
func TestStoredGroupRejectsMalformedData(t *testing.T) {
	t.Parallel()

	_, err := NewKVStore(nil)
	require.Error(t, err)

	_, err = encodeStoredGroup(StoredGroup{
		PayloadBytes: bytes.Repeat([]byte{1}, maxStoredGroupSize),
	})
	require.Error(t, err)

	tests := []struct {
		name    string
		encoded []byte
	}{
		{name: "empty"},
		{
			name:    "oversized",
			encoded: bytes.Repeat([]byte{1}, maxStoredGroupSize+1),
		},
		{
			name:    "invalid json",
			encoded: []byte("{"),
		},
		{
			name:    "unknown field",
			encoded: []byte(`{"version":1,"surprise":true}`),
		},
		{
			name:    "unknown version",
			encoded: []byte(`{"version":2}`),
		},
		{
			name: "invalid failure reason",
			encoded: []byte(
				`{"version":1,"failure_reason":"AQ=="}`,
			),
		},
		{
			name:    "trailing value",
			encoded: []byte(`{"version":1} {}`),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, err := decodeStoredGroup(test.encoded)
			require.Error(t, err)
		})
	}

	store := NewMemoryStore()
	id := trampoline.RelayID{PaymentHash: [32]byte{1}}
	store.groups[id] = []byte("{")
	_, err = store.Load(id)
	require.Error(t, err)
	_, err = store.List()
	require.Error(t, err)
}
