package macaroons_test

import (
	"context"
	"encoding/hex"
	"fmt"
	"testing"

	"github.com/lightningnetwork/lnd/macaroons"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/metadata"
	"gopkg.in/macaroon-bakery.v2/bakery"
)

// knownProfileStub is a test stub for the ProtectorProfileChecker interface
// that knows a fixed set of profile names.
type knownProfileStub map[string]struct{}

func (k knownProfileStub) KnownProtectorProfile(profile string) error {
	if _, ok := k[profile]; !ok {
		return fmt.Errorf("unknown protector profile %q", profile)
	}

	return nil
}

// TestValidateProtectorProfileName makes sure malformed profile names are
// rejected.
func TestValidateProtectorProfileName(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name    string
		profile string
		valid   bool
	}{
		{"valid name", "channel-management-v1", true},
		{"empty name", "", false},
		{"inner space", "chan mgmt", false},
		{"tab", "chan\tmgmt", false},
		{"newline", "chan\nmgmt", false},
		{"leading space", " chan-mgmt", false},
		{"non-printable", "chan\x00mgmt", false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			err := macaroons.ValidateProtectorProfileName(
				tc.profile,
			)
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.ErrorIs(
					t, err,
					macaroons.ErrInvalidProtectorProfile,
				)
			}
		})
	}
}

// TestProtectorConstraint tests that protector caveats are added correctly
// and can be read back from the macaroon.
func TestProtectorConstraint(t *testing.T) {
	t.Parallel()

	mac := createDummyMacaroon(t)

	// An invalid profile name must be refused at bake time already.
	_, err := macaroons.AddConstraints(
		mac, macaroons.ProtectorConstraint("bad name"),
	)
	require.ErrorIs(t, err, macaroons.ErrInvalidProtectorProfile)

	// Add two protector caveats plus an unrelated one and make sure both
	// profiles are found again.
	newMac, err := macaroons.AddConstraints(
		mac,
		macaroons.ProtectorConstraint("channel-management-v1"),
		macaroons.IPLockConstraint("127.0.0.1"),
		macaroons.ProtectorConstraint("other-profile"),
	)
	require.NoError(t, err)

	profiles, err := macaroons.GetProtectorProfiles(newMac)
	require.NoError(t, err)
	require.Equal(
		t, []string{"channel-management-v1", "other-profile"}, profiles,
	)

	// The caveat must be serialized in the documented format.
	require.Equal(
		t, "protector channel-management-v1",
		string(newMac.Caveats()[0].Id),
	)

	// A macaroon without protector caveats yields no profiles.
	profiles, err = macaroons.GetProtectorProfiles(
		createDummyMacaroon(t),
	)
	require.NoError(t, err)
	require.Empty(t, profiles)

	// A nil macaroon yields no profiles.
	profiles, err = macaroons.GetProtectorProfiles(nil)
	require.NoError(t, err)
	require.Empty(t, profiles)
}

// TestGetProtectorProfilesMalformed makes sure malformed protector caveats
// result in an error instead of being silently skipped, so callers failing
// closed on the error cannot be tricked into skipping enforcement.
func TestGetProtectorProfilesMalformed(t *testing.T) {
	t.Parallel()

	// A bare "protector" caveat without a profile name is malformed.
	mac := createDummyMacaroon(t)
	require.NoError(t, mac.AddFirstPartyCaveat([]byte("protector")))

	_, err := macaroons.GetProtectorProfiles(mac)
	require.ErrorIs(t, err, macaroons.ErrInvalidProtectorProfile)

	// A profile name containing another space is malformed.
	mac = createDummyMacaroon(t)
	require.NoError(
		t, mac.AddFirstPartyCaveat([]byte("protector two words")),
	)

	_, err = macaroons.GetProtectorProfiles(mac)
	require.ErrorIs(t, err, macaroons.ErrInvalidProtectorProfile)

	// A caveat whose condition is "protector" followed by a whitespace
	// variant other than the single space the bakery uses (e.g. a tab) was
	// almost certainly meant to be a protector caveat; it must fail closed
	// instead of being silently skipped.
	mac = createDummyMacaroon(t)
	tabCaveat := []byte("protector\tchannel-management-v1")
	require.NoError(t, mac.AddFirstPartyCaveat(tabCaveat))

	_, err = macaroons.GetProtectorProfiles(mac)
	require.ErrorIs(t, err, macaroons.ErrInvalidProtectorProfile)

	// Other near miss encodings that were almost certainly meant to be
	// protector caveats fail closed as well: wrong letter case or a
	// separator that isn't the single space of the canonical encoding.
	for _, nearMiss := range []string{
		"Protector channel-management-v1",
		"PROTECTOR channel-management-v1",
		"protector:channel-management-v1",
		"protector=channel-management-v1",
	} {
		mac = createDummyMacaroon(t)
		require.NoError(
			t, mac.AddFirstPartyCaveat([]byte(nearMiss)),
		)

		_, err = macaroons.GetProtectorProfiles(mac)
		require.ErrorIs(
			t, err, macaroons.ErrInvalidProtectorProfile,
			"near miss %q must fail closed", nearMiss,
		)
	}

	// A caveat of a different condition that merely shares the prefix
	// string ("protectorX", "protector-v2") is not a protector caveat and
	// is ignored.
	for _, distinct := range []string{
		"protectorX foo",
		"protector-v2 foo",
		"protector_x foo",
	} {
		mac = createDummyMacaroon(t)
		require.NoError(
			t, mac.AddFirstPartyCaveat([]byte(distinct)),
		)

		profiles, err := macaroons.GetProtectorProfiles(mac)
		require.NoError(
			t, err, "distinct condition %q must be ignored",
			distinct,
		)
		require.Empty(t, profiles)
	}
}

// TestProtectorCheckerBakery runs the protector checker through a real
// bakery backed macaroon service and asserts that macaroons referencing
// unknown profiles are rejected as a whole while known profiles validate.
func TestProtectorCheckerBakery(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	db := setupTestRootKeyStorage(t)
	rootKeyStore, err := macaroons.NewRootKeyStorage(db)
	require.NoError(t, err)

	known := knownProfileStub{"channel-management-v1": {}}
	service, err := macaroons.NewService(
		rootKeyStore, "lnd", false,
		macaroons.ProtectorChecker(known),
	)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, service.Close())
	}()

	err = service.CreateUnlock(&defaultPw)
	require.NoError(t, err)

	bakeWithProfile := func(profile string) []byte {
		bakedMac, err := service.NewMacaroon(
			ctx, macaroons.DefaultRootKeyID, testOperation,
		)
		require.NoError(t, err)

		constrainedMac, err := macaroons.AddConstraints(
			bakedMac.M(), macaroons.ProtectorConstraint(profile),
		)
		require.NoError(t, err)

		macBytes, err := constrainedMac.MarshalBinary()
		require.NoError(t, err)

		return macBytes
	}

	macCtx := func(macBytes []byte) context.Context {
		md := metadata.Pairs(
			"macaroon", hex.EncodeToString(macBytes),
		)

		return metadata.NewIncomingContext(ctx, md)
	}

	// A macaroon with a known protector profile must validate.
	knownMac := bakeWithProfile("channel-management-v1")
	err = service.CheckMacAuth(
		macCtx(knownMac), knownMac, []bakery.Op{testOperation},
		"SomeMethod",
	)
	require.NoError(t, err)

	// A macaroon referencing an unknown profile must be rejected as a
	// whole, even though its permission ops would otherwise be
	// sufficient. This is the fail closed property that makes future
	// profile names safe to introduce.
	unknownMac := bakeWithProfile("future-profile-v9")
	err = service.CheckMacAuth(
		macCtx(unknownMac), unknownMac, []bakery.Op{testOperation},
		"SomeMethod",
	)
	require.ErrorContains(t, err, "future-profile-v9")

	// An lnd without the protector checker registered (simulating an
	// older version) must also reject the macaroon outright, because the
	// bakery treats the caveat as unrecognized.
	oldRootKeyStore, err := macaroons.NewRootKeyStorage(db)
	require.NoError(t, err)
	oldService, err := macaroons.NewService(oldRootKeyStore, "lnd", false)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, oldService.Close())
	}()

	err = oldService.CreateUnlock(&defaultPw)
	require.NoError(t, err)

	err = oldService.CheckMacAuth(
		macCtx(knownMac), knownMac, []bakery.Op{testOperation},
		"SomeMethod",
	)
	require.Error(t, err)
}
