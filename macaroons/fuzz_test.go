package macaroons

import (
	"testing"

	"gopkg.in/macaroon-bakery.v2/bakery"
	"gopkg.in/macaroon.v2"
)

func FuzzUnmarshalMacaroon(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		mac := &macaroon.Macaroon{}
		_ = mac.UnmarshalBinary(data)
	})
}

func FuzzAuthChecker(f *testing.F) {
	rootKeyStore := bakery.NewMemRootKeyStore()
	ctx := f.Context()

	f.Fuzz(func(t *testing.T, location, entity, action, method string,
		rootKey, id []byte) {

		macService, err := NewService(
			rootKeyStore, location, true, IPLockChecker,
		)
		if err != nil {
			return
		}

		requiredPermissions := []bakery.Op{{
			Entity: entity,
			Action: action,
		}}

		mac, err := macaroon.New(rootKey, id, location, macaroon.V2)
		if err != nil {
			return
		}

		macBytes, err := mac.MarshalBinary()
		if err != nil {
			return
		}

		_ = macService.CheckMacAuth(
			ctx, macBytes, requiredPermissions, method,
		)
	})
}
