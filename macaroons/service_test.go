package macaroons_test

import (
	"encoding/hex"
	"path"
	"testing"

	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/macaroons"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/metadata"
	"gopkg.in/macaroon-bakery.v2/bakery"
	"gopkg.in/macaroon-bakery.v2/bakery/checkers"
	macaroon "gopkg.in/macaroon.v2"
)

var (
	testOperation = bakery.Op{
		Entity: "testEntity",
		Action: "read",
	}
	testOperationURI = bakery.Op{
		Entity: macaroons.PermissionEntityCustomURI,
		Action: "SomeMethod",
	}
	defaultPw = []byte("hello")
)

// setupTestRootKeyStorage creates a dummy root key storage by
// creating a temporary macaroons.db and initializing it with the
// default password of 'hello'. Only the path to the temporary
// DB file is returned, because the service will open the file
// and read the store on its own.
func setupTestRootKeyStorage(t *testing.T) kvdb.Backend {
	db, err := kvdb.Create(
		kvdb.BoltBackendName, path.Join(t.TempDir(), "macaroons.db"), true,
		kvdb.DefaultDBTimeout, false,
	)
	require.NoError(t, err, "Error opening store DB")
	t.Cleanup(func() {
		require.NoError(t, db.Close())
	})

	store, err := macaroons.NewRootKeyStorage(db)
	require.NoError(t, err, "Error creating root key store")

	err = store.CreateUnlock(&defaultPw)
	require.NoError(t, store.Close())
	require.NoError(t, err, "error creating unlock")

	return db
}

// TestNewService tests the creation of the macaroon service.
func TestNewService(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	// First, initialize a dummy DB file with a store that the service
	// can read from. Make sure the file is removed in the end.
	db := setupTestRootKeyStorage(t)

	rootKeyStore, err := macaroons.NewRootKeyStorage(db)
	require.NoError(t, err)

	// Second, create the new service instance, unlock it and pass in a
	// checker that we expect it to add to the bakery.
	service, err := macaroons.NewService(
		rootKeyStore, "lnd", false, macaroons.IPLockChecker,
	)
	require.NoError(t, err, "Error creating new service")
	defer service.Close()
	err = service.CreateUnlock(&defaultPw)
	require.NoError(t, err, "Error unlocking root key storage")

	// Third, check if the created service can bake macaroons.
	_, err = service.NewMacaroon(ctx, nil, testOperation)
	if err != macaroons.ErrMissingRootKeyID {
		t.Fatalf("Received %v instead of ErrMissingRootKeyID", err)
	}

	macaroon, err := service.NewMacaroon(
		ctx, macaroons.DefaultRootKeyID, testOperation,
	)
	require.NoError(t, err, "Error creating macaroon from service")
	if macaroon.Namespace().String() != "std:" {
		t.Fatalf("The created macaroon has an invalid namespace: %s",
			macaroon.Namespace().String())
	}

	// Finally, check if the service has been initialized correctly and
	// the checker has been added.
	var checkerFound = false
	checker := service.Checker.FirstPartyCaveatChecker.(*checkers.Checker)
	for _, info := range checker.Info() {
		if info.Name == "ipaddr" &&
			info.Prefix == "" &&
			info.Namespace == "std" {
			checkerFound = true
		}
	}
	if !checkerFound {
		t.Fatalf("Checker '%s' not found in service.", "ipaddr")
	}
}

// TestValidateMacaroon tests the validation of a macaroon that is in an
// incoming context.
func TestValidateMacaroon(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	// First, initialize the service and unlock it.
	db := setupTestRootKeyStorage(t)
	rootKeyStore, err := macaroons.NewRootKeyStorage(db)
	require.NoError(t, err)
	service, err := macaroons.NewService(
		rootKeyStore, "lnd", false, macaroons.IPLockChecker,
	)
	require.NoError(t, err, "Error creating new service")
	defer service.Close()

	err = service.CreateUnlock(&defaultPw)
	require.NoError(t, err, "Error unlocking root key storage")

	// Then, create a new macaroon that we can serialize.
	macaroon, err := service.NewMacaroon(
		ctx, macaroons.DefaultRootKeyID, testOperation,
		testOperationURI,
	)
	require.NoError(t, err, "Error creating macaroon from service")
	macaroonBinary, err := macaroon.M().MarshalBinary()
	require.NoError(t, err, "Error serializing macaroon")

	// Because the macaroons are always passed in a context, we need to
	// mock one that has just the serialized macaroon as a value.
	md := metadata.New(map[string]string{
		"macaroon": hex.EncodeToString(macaroonBinary),
	})
	mockContext := metadata.NewIncomingContext(ctx, md)

	// Finally, validate the macaroon against the required permissions.
	err = service.ValidateMacaroon(
		mockContext, []bakery.Op{testOperation}, "FooMethod",
	)
	require.NoError(t, err, "Error validating the macaroon")

	// If the macaroon has the method specific URI permission, the list of
	// required entity/action pairs is irrelevant.
	err = service.ValidateMacaroon(
		mockContext, []bakery.Op{{Entity: "irrelevant"}}, "SomeMethod",
	)
	require.NoError(t, err, "Error validating the macaroon")
}

// TestListMacaroonIDs checks that ListMacaroonIDs returns the expected result.
func TestListMacaroonIDs(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	// First, initialize a dummy DB file with a store that the service
	// can read from. Make sure the file is removed in the end.
	db := setupTestRootKeyStorage(t)

	// Second, create the new service instance, unlock it and pass in a
	// checker that we expect it to add to the bakery.
	rootKeyStore, err := macaroons.NewRootKeyStorage(db)
	require.NoError(t, err)
	service, err := macaroons.NewService(
		rootKeyStore, "lnd", false, macaroons.IPLockChecker,
	)
	require.NoError(t, err, "Error creating new service")
	defer service.Close()

	err = service.CreateUnlock(&defaultPw)
	require.NoError(t, err, "Error unlocking root key storage")

	// Third, make 3 new macaroons with different root key IDs.
	expectedIDs := [][]byte{{1}, {2}, {3}}
	for _, v := range expectedIDs {
		_, err := service.NewMacaroon(ctx, v, testOperation)
		require.NoError(t, err, "Error creating macaroon from service")
	}

	// Finally, check that calling List return the expected values.
	ids, _ := service.ListMacaroonIDs(ctx)
	require.Equal(t, expectedIDs, ids, "root key IDs mismatch")
}

// TestDeleteMacaroonID removes the specific root key ID.
func TestDeleteMacaroonID(t *testing.T) {
	t.Parallel()

	ctxb := t.Context()

	// First, initialize a dummy DB file with a store that the service
	// can read from. Make sure the file is removed in the end.
	db := setupTestRootKeyStorage(t)

	// Second, create the new service instance, unlock it and pass in a
	// checker that we expect it to add to the bakery.
	rootKeyStore, err := macaroons.NewRootKeyStorage(db)
	require.NoError(t, err)
	service, err := macaroons.NewService(
		rootKeyStore, "lnd", false, macaroons.IPLockChecker,
	)
	require.NoError(t, err, "Error creating new service")
	defer service.Close()

	err = service.CreateUnlock(&defaultPw)
	require.NoError(t, err, "Error unlocking root key storage")

	// Third, checks that removing encryptedKeyID returns an error.
	encryptedKeyID := []byte("enckey")
	_, err = service.DeleteMacaroonID(ctxb, encryptedKeyID)
	require.Equal(t, macaroons.ErrDeletionForbidden, err)

	// Fourth, checks that removing DefaultKeyID returns an error.
	_, err = service.DeleteMacaroonID(ctxb, macaroons.DefaultRootKeyID)
	require.Equal(t, macaroons.ErrDeletionForbidden, err)

	// Fifth, checks that removing empty key id returns an error.
	_, err = service.DeleteMacaroonID(ctxb, []byte{})
	require.Equal(t, macaroons.ErrMissingRootKeyID, err)

	// Sixth, checks that removing a non-existed key id returns nil.
	nonExistedID := []byte("test-non-existed")
	deletedID, err := service.DeleteMacaroonID(ctxb, nonExistedID)
	require.NoError(t, err, "deleting macaroon ID got an error")
	require.Nil(t, deletedID, "deleting non-existed ID should return nil")

	// Seventh, make 3 new macaroons with different root key IDs, and delete
	// one.
	expectedIDs := [][]byte{{1}, {2}, {3}}
	for _, v := range expectedIDs {
		_, err := service.NewMacaroon(ctxb, v, testOperation)
		require.NoError(t, err, "Error creating macaroon from service")
	}
	deletedID, err = service.DeleteMacaroonID(ctxb, expectedIDs[0])
	require.NoError(t, err, "deleting macaroon ID got an error")

	// Finally, check that the ID is deleted.
	require.Equal(t, expectedIDs[0], deletedID, "expected ID to be removed")
	ids, _ := service.ListMacaroonIDs(ctxb)
	require.Equal(t, expectedIDs[1:], ids, "root key IDs mismatch")
}

// TestCloneMacaroons tests that macaroons can be cloned correctly and that
// modifications to the copy don't affect the original.
func TestCloneMacaroons(t *testing.T) {
	t.Parallel()

	// Get a configured version of the constraint function.
	constraintFunc := macaroons.TimeoutConstraint(3)

	// Now we need a dummy macaroon that we can apply the constraint
	// function to.
	testMacaroon := createDummyMacaroon(t)
	err := constraintFunc(testMacaroon)
	require.NoError(t, err)

	// Check that the caveat has an empty location.
	require.Equal(
		t, "", testMacaroon.Caveats()[0].Location,
		"expected caveat location to be empty, found: %s",
		testMacaroon.Caveats()[0].Location,
	)

	// Make a copy of the macaroon.
	newMacCred, err := macaroons.NewMacaroonCredential(testMacaroon)
	require.NoError(t, err)

	newMac := newMacCred.Macaroon
	require.Equal(
		t, "", newMac.Caveats()[0].Location,
		"expected new caveat location to be empty, found: %s",
		newMac.Caveats()[0].Location,
	)

	// They should be deep equal as well.
	testMacaroonBytes, err := testMacaroon.MarshalBinary()
	require.NoError(t, err)
	newMacBytes, err := newMac.MarshalBinary()
	require.NoError(t, err)
	require.Equal(t, testMacaroonBytes, newMacBytes)

	// Modify the caveat location on the old macaroon.
	testMacaroon.Caveats()[0].Location = "mars"

	// The old macaroon's caveat location should be changed.
	require.Equal(
		t, "mars", testMacaroon.Caveats()[0].Location,
		"expected caveat location to be empty, found: %s",
		testMacaroon.Caveats()[0].Location,
	)

	// The new macaroon's caveat location should stay untouched.
	require.Equal(
		t, "", newMac.Caveats()[0].Location,
		"expected new caveat location to be empty, found: %s",
		newMac.Caveats()[0].Location,
	)
}

// TestMacaroonVersionDecode tests that we'll reject macaroons with an unknown
// version.
func TestMacaroonVersionDecode(t *testing.T) {
	t.Parallel()

	ctxb := t.Context()

	// First, initialize a dummy DB file with a store that the service
	// can read from. Make sure the file is removed in the end.
	db := setupTestRootKeyStorage(t)

	// Second, create the new service instance, unlock it and pass in a
	// checker that we expect it to add to the bakery.
	rootKeyStore, err := macaroons.NewRootKeyStorage(db)
	require.NoError(t, err)

	service, err := macaroons.NewService(
		rootKeyStore, "lnd", false, macaroons.IPLockChecker,
	)
	require.NoError(t, err, "Error creating new service")

	defer service.Close()

	t.Run("invalid_version", func(t *testing.T) {
		// Now that we have our sample service, we'll make a new custom
		// macaroon with an unknown version.
		testMac, err := macaroon.New(
			testRootKey, testID, testLocation, 1,
		)
		require.NoError(t, err)

		// Next, we'll serialize the macaroon to the binary form,
		// modifying the first byte to signal an unknown version.
		testMacBytes, err := testMac.MarshalBinary()
		require.NoError(t, err)

		// If we attempt to check the mac auth, then we should get a
		// failure that the version is unknown.
		err = service.CheckMacAuth(ctxb, testMacBytes, nil, "")
		require.ErrorIs(t, err, macaroons.ErrUnknownVersion)
	})

	t.Run("invalid_id", func(t *testing.T) {
		// We'll now make a macaroon with a valid version, but modify
		// the ID to be rejected.
		badID := []byte{}
		testMac, err := macaroon.New(
			testRootKey, badID, testLocation, testVersion,
		)
		require.NoError(t, err)

		testMacBytes, err := testMac.MarshalBinary()
		require.NoError(t, err)

		// If we attempt to check the mac auth, then we should get a
		// failure that the ID is bad.
		err = service.CheckMacAuth(ctxb, testMacBytes, nil, "")
		require.ErrorIs(t, err, macaroons.ErrInvalidID)
	})
}
