package macaroons

import (
	"strings"
	"testing"
	"unicode"

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

// FuzzGetProtectorProfiles makes sure the protector caveat parser never
// panics, never returns an invalid profile name without an error and never
// silently skips a well formed protector caveat.
func FuzzGetProtectorProfiles(f *testing.F) {
	f.Add(
		"protector channel-management-v1",
		"time-before 2026-01-01T00:00:00Z",
	)
	f.Add("protector", "protector\tchannel-management-v1")
	f.Add("protectorX foo", "protector two words")
	f.Add("", "lnd-custom foo bar")

	f.Fuzz(func(t *testing.T, caveat1, caveat2 string) {
		mac, err := macaroon.New(
			[]byte("key"), []byte("id"), "lnd", macaroon.V2,
		)
		if err != nil {
			return
		}

		for _, caveat := range []string{caveat1, caveat2} {
			if caveat == "" {
				continue
			}
			err := mac.AddFirstPartyCaveat([]byte(caveat))
			if err != nil {
				return
			}
		}

		profiles, err := GetProtectorProfiles(mac)
		if err != nil {
			return
		}

		// Any profile returned without an error must be well formed.
		for _, profile := range profiles {
			err := ValidateProtectorProfileName(profile)
			if err != nil {
				t.Fatalf("parser returned invalid profile "+
					"%q: %v", profile, err)
			}
		}

		// A caveat that is exactly "protector <valid-name>" must never
		// be silently dropped: it either shows up in the result or the
		// parse errors (handled above).
		for _, caveat := range []string{caveat1, caveat2} {
			after, found := strings.CutPrefix(
				caveat, CondProtector+" ",
			)
			if !found {
				continue
			}
			if ValidateProtectorProfileName(after) != nil {
				continue
			}

			returned := false
			for _, profile := range profiles {
				if profile == after {
					returned = true
				}
			}
			if !returned {
				t.Fatalf("well formed protector caveat %q "+
					"was silently dropped", caveat)
			}
		}
	})
}

// FuzzValidateProtectorProfileName makes sure profile name validation never
// panics and only ever accepts printable, whitespace free, non-empty names.
func FuzzValidateProtectorProfileName(f *testing.F) {
	f.Add("channel-management-v1")
	f.Add("chan mgmt")
	f.Add("chan\tmgmt")
	f.Add("")

	f.Fuzz(func(t *testing.T, name string) {
		if err := ValidateProtectorProfileName(name); err != nil {
			return
		}

		if name == "" {
			t.Fatal("empty name accepted")
		}
		for _, r := range name {
			if unicode.IsSpace(r) || !unicode.IsPrint(r) {
				t.Fatalf("name %q with invalid rune %q "+
					"accepted", name, r)
			}
		}
	})
}
