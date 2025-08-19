package macaroons_test

import (
	"encoding/hex"
	"testing"

	"github.com/lightningnetwork/lnd/macaroons"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/metadata"
	"gopkg.in/macaroon-bakery.v2/bakery"
)

// TestBakeFromRootKey tests that a macaroon can be baked from a root key
// directly without needing to create a store or service first.
func TestBakeFromRootKey(t *testing.T) {
	// Create a test store and unlock it.
	_, store := newTestStore(t)

	pw := []byte("weks")
	err := store.CreateUnlock(&pw)
	require.NoError(t, err)

	// Force the store to create a new random root key.
	key, id, err := store.RootKey(defaultRootKeyIDContext)
	require.NoError(t, err)
	require.Len(t, key, 32)

	tmpKey, err := store.Get(defaultRootKeyIDContext, id)
	require.NoError(t, err)
	require.Equal(t, key, tmpKey)

	// Create a service that uses the root key store.
	service, err := macaroons.NewService(store, "lnd", false)
	require.NoError(t, err, "Error creating new service")
	defer func() {
		require.NoError(t, service.Close())
	}()

	// Call the BakeFromRootKey function that derives a macaroon directly
	// from the root key.
	perms := []bakery.Op{{Entity: "foo", Action: "bar"}}
	mac, err := macaroons.BakeFromRootKey(key, perms)
	require.NoError(t, err)

	macaroonBytes, err := mac.MarshalBinary()
	require.NoError(t, err)

	md := metadata.New(map[string]string{
		"macaroon": hex.EncodeToString(macaroonBytes),
	})
	macCtx := metadata.NewIncomingContext(t.Context(), md)

	// The macaroon should be valid for the service, since the root key was
	// the same.
	err = service.ValidateMacaroon(macCtx, nil, "baz")
	require.NoError(t, err)
}
