package macaroons_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io/ioutil"
	"os"
	"path"
	"testing"

	"github.com/coreos/bbolt"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/macaroons"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"gopkg.in/macaroon-bakery.v2/bakery"
	"gopkg.in/macaroon-bakery.v2/bakery/checkers"
)

var (
	testOperation = bakery.Op{
		Entity: "testEntity",
		Action: "read",
	}
	testPermissionMap = map[string][]bakery.Op{
		"/some/fake/path": {testOperation},
	}
	defaultPw = []byte("hello")
)

// setupTestRootKeyStorage creates a dummy root key storage by
// creating a temporary macaroons.db and initializing it with the
// default password of 'hello'. Only the path to the temporary
// DB file is returned, because the service will open the file
// and read the store on its own.
func setupTestRootKeyStorage(t *testing.T) string {
	tempDir, err := ioutil.TempDir("", "macaroonstore-")
	if err != nil {
		t.Fatalf("Error creating temp dir: %v", err)
	}
	db, err := bbolt.Open(
		path.Join(tempDir, "macaroons.db"), 0600, bbolt.DefaultOptions,
	)
	if err != nil {
		t.Fatalf("Error opening store DB: %v", err)
	}
	store, err := macaroons.NewRootKeyStorage(db)
	if err != nil {
		db.Close()
		t.Fatalf("Error creating root key store: %v", err)
	}
	defer store.Close()
	err = store.CreateUnlock(&defaultPw)
	if err != nil {
		t.Fatalf("error creating unlock: %v", err)
	}
	return tempDir
}

// TestNewService tests the creation of the macaroon service.
func TestNewService(t *testing.T) {
	// First, initialize a dummy DB file with a store that the service
	// can read from. Make sure the file is removed in the end.
	tempDir := setupTestRootKeyStorage(t)
	defer os.RemoveAll(tempDir)

	// Second, create the new service instance, unlock it and pass in a
	// checker that we expect it to add to the bakery.
	service, err := macaroons.NewService(tempDir, macaroons.IPLockChecker)
	defer service.Close()
	if err != nil {
		t.Fatalf("Error creating new service: %v", err)
	}
	err = service.CreateUnlock(&defaultPw)
	if err != nil {
		t.Fatalf("Error unlocking root key storage: %v", err)
	}

	// Third, check if the created service can bake macaroons.
	macaroon, err := service.Oven.NewMacaroon(
		nil, bakery.LatestVersion, nil, testOperation,
	)
	if err != nil {
		t.Fatalf("Error creating macaroon from service: %v", err)
	}
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
	// First, initialize the service and unlock it.
	tempDir := setupTestRootKeyStorage(t)
	defer os.RemoveAll(tempDir)
	service, err := macaroons.NewService(tempDir, macaroons.IPLockChecker)
	defer service.Close()
	if err != nil {
		t.Fatalf("Error creating new service: %v", err)
	}
	err = service.CreateUnlock(&defaultPw)
	if err != nil {
		t.Fatalf("Error unlocking root key storage: %v", err)
	}

	// Then, create a new macaroon that we can serialize.
	macaroon, err := service.Oven.NewMacaroon(
		nil, bakery.LatestVersion, nil, testOperation,
	)
	if err != nil {
		t.Fatalf("Error creating macaroon from service: %v", err)
	}
	macaroonBinary, err := macaroon.M().MarshalBinary()
	if err != nil {
		t.Fatalf("Error serializing macaroon: %v", err)
	}

	// Because the macaroons are always passed in a context, we need to
	// mock one that has just the serialized macaroon as a value.
	md := metadata.New(map[string]string{
		"macaroon": hex.EncodeToString(macaroonBinary),
	})
	mockContext := metadata.NewIncomingContext(context.Background(), md)

	// Finally, validate the macaroon against the required permissions.
	err = service.ValidateMacaroon(
		mockContext, lnrpc.GetInfoRequest{}, "",
		[]bakery.Op{testOperation},
	)
	if err != nil {
		t.Fatalf("Error validating the macaroon: %v", err)
	}
}

// TestUnaryServerInterceptor tests the synchronous gRPC interceptor on the
// server side.
func TestUnaryServerInterceptor(t *testing.T) {
	// First, initialize the service and unlock it.
	tempDir := setupTestRootKeyStorage(t)
	defer os.RemoveAll(tempDir)
	service, err := macaroons.NewService(
		tempDir, macaroons.IPLockChecker, macaroons.RequestHashChecker,
	)
	defer service.Close()
	if err != nil {
		t.Fatalf("Error creating new service: %v", err)
	}
	err = service.CreateUnlock(&defaultPw)
	if err != nil {
		t.Fatalf("Error unlocking root key storage: %v", err)
	}

	// Then, create a new macaroon that we can serialize.
	macaroon, err := service.Oven.NewMacaroon(
		nil, bakery.LatestVersion, nil, testOperation,
	)
	if err != nil {
		t.Fatalf("Error creating macaroon from service: %v", err)
	}

	// Now we prepare the hash of the request. For this test, we use the
	// method /some/fake/path and an empty struct that JSON serializes to
	// the string '{}'. So the SHA256 hash will be of the string
	// '/some/fake/path{}'.
	sha256Bytes := sha256.Sum256([]byte("/some/fake/path{}"))
	sha256Str := hex.EncodeToString(sha256Bytes[:])
	mockContext := context.WithValue(
		context.Background(), macaroons.CondRequestHash, sha256Str,
	)

	// Add the request hash first-party caveat to the macaroon and serialize
	// it.
	constrainedMac, err := macaroons.AddConstraints(
		macaroon.M(), macaroons.RequestHashConstraint(sha256Str),
	)
	if err != nil {
		t.Fatalf("Error adding constraint on macaroon: %v", err)
	}
	macaroonBinary, err := constrainedMac.MarshalBinary()
	if err != nil {
		t.Fatalf("Error serializing macaroon: %v", err)
	}

	// Because the macaroons are always passed in a context, we need to
	// mock one that has just the serialized macaroon as a value.
	md := metadata.New(map[string]string{
		"macaroon": hex.EncodeToString(macaroonBinary),
	})
	mockContext = metadata.NewIncomingContext(mockContext, md)

	// When the interceptor finishes successfully, a handler function is
	// called so the chain can continue. We provide a fake one and assume
	// everything is ok if it is called.
	interceptStatus := false
	fakeHandler := func(ctx context.Context,
		req interface{}) (interface{}, error) {
		interceptStatus = true
		return true, nil
	}

	// Next, get the interceptor function and pass permissions, then invoke
	// it with mocked server environment.
	interceptFunc := service.UnaryServerInterceptor(testPermissionMap)
	fakeInfo := &grpc.UnaryServerInfo{
		Server:     nil,
		FullMethod: "/some/fake/path",
	}
	result, err := interceptFunc(
		mockContext, lnrpc.GetInfoRequest{}, fakeInfo, fakeHandler,
	)

	// Then, make sure everything worked as expected.
	if err != nil || result != true || !interceptStatus {
		t.Fatalf("Error intercepting the request: %v", err)
	}

	// Now check that a path not specified in the macaroon is rejected.
	interceptStatus = false
	fakeInfo = &grpc.UnaryServerInfo{
		Server:     nil,
		FullMethod: "/another/path",
	}
	result, err = interceptFunc(
		mockContext, lnrpc.GetInfoRequest{}, fakeInfo, fakeHandler,
	)
	if err == nil || result != nil || interceptStatus {
		t.Fatalf("Unexpected result, error should be set but is nil.")
	}

	// Finally, check that another request object is rejected because it
	// produces a different hash.
	interceptStatus = false
	fakeInfo = &grpc.UnaryServerInfo{
		Server:     nil,
		FullMethod: "/some/fake/path",
	}
	wrongRequestObj := &lnrpc.DebugLevelRequest{Show: true}
	result, err = interceptFunc(
		mockContext, wrongRequestObj, fakeInfo, fakeHandler,
	)
	if err == nil || result != nil || interceptStatus {
		t.Fatalf("Unexpected result, error should be set but is nil.")
	}
}
