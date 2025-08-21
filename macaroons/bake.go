package macaroons

import (
	"bytes"
	"context"
	"fmt"

	"gopkg.in/macaroon-bakery.v2/bakery"
	"gopkg.in/macaroon.v2"
)

// inMemoryRootKeyStore is a simple implementation of bakery.RootKeyStore that
// stores a single root key in memory.
type inMemoryRootKeyStore struct {
	rootKey []byte
}

// A compile-time check to ensure that inMemoryRootKeyStore implements
// bakery.RootKeyStore.
var _ bakery.RootKeyStore = (*inMemoryRootKeyStore)(nil)

// Get returns the root key for the given id. If the item is not there, it
// returns ErrNotFound.
func (s *inMemoryRootKeyStore) Get(_ context.Context, id []byte) ([]byte,
	error) {

	if !bytes.Equal(id, DefaultRootKeyID) {
		return nil, bakery.ErrNotFound
	}

	return s.rootKey, nil
}

// RootKey returns the root key to be used for making a new macaroon, and an id
// that can be used to look it up later with the Get method.
func (s *inMemoryRootKeyStore) RootKey(context.Context) ([]byte, []byte,
	error) {

	return s.rootKey, DefaultRootKeyID, nil
}

// BakeFromRootKey creates a new macaroon that is derived from the given root
// key and permissions.
func BakeFromRootKey(rootKey []byte,
	permissions []bakery.Op) (*macaroon.Macaroon, error) {

	if len(rootKey) != RootKeyLen {
		return nil, fmt.Errorf("root key must be %d bytes, is %d",
			RootKeyLen, len(rootKey))
	}

	rootKeyStore := &inMemoryRootKeyStore{
		rootKey: rootKey,
	}

	service, err := NewService(rootKeyStore, "lnd", false)
	if err != nil {
		return nil, fmt.Errorf("unable to create service: %w", err)
	}

	ctx := context.Background()
	mac, err := service.NewMacaroon(ctx, DefaultRootKeyID, permissions...)
	if err != nil {
		return nil, fmt.Errorf("unable to create macaroon: %w", err)
	}

	return mac.M(), nil
}
