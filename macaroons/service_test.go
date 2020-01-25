package macaroons_test

import (
	"context"
	"encoding/hex"
	"io/ioutil"
	"os"
	"testing"

	"github.com/lightningnetwork/lnd/macaroons"
	"google.golang.org/grpc/metadata"
	"gopkg.in/macaroon-bakery.v2/bakery"
	"gopkg.in/macaroon-bakery.v2/bakery/checkers"
)

var (
	testOperation = bakery.Op{
		Entity: "testEntity",
		Action: "read",
	}
	defaultPw = []byte("hello")
)

// setupService creates a dummy root key storage by
// creating a temporary macaroons.db and initializing it with the
// default password of 'hello'. Then the store is unlocked and a new macaroons
// service is created and returned.
func setupService(t *testing.T) (*macaroons.Service, func()) {
	tempDir, err := ioutil.TempDir("", "macaroonstore-")
	if err != nil {
		t.Fatalf("Error creating temp dir: %v", err)
	}
	service, err := macaroons.NewService(tempDir, macaroons.IPLockChecker)
	if err != nil {
		os.RemoveAll(tempDir)
		t.Fatalf("Error creating new service: %v", err)
	}

	cleanup := func() {
		service.Close()
		os.RemoveAll(tempDir)
	}

	if err := service.CreateUnlock(&defaultPw); err != nil {
		cleanup()
		t.Fatalf("Error unlocking root key store: %v", err)
	}

	return service, cleanup
}

// TestNewService tests the creation of the macaroon service.
func TestNewService(t *testing.T) {
	// First, initialize and unlock the service.
	service, cleanup := setupService(t)
	defer cleanup()

	// Third, check if the created service can bake macaroons.
	macaroon, err := service.Oven.NewMacaroon(
		context.Background(), bakery.LatestVersion, nil, testOperation,
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
	// First, initialize and unlock the service.
	service, cleanup := setupService(t)
	defer cleanup()

	// Then, create a new macaroon that we can serialize.
	macaroon, err := service.Oven.NewMacaroon(
		context.Background(), bakery.LatestVersion, nil, testOperation,
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
	err = service.ValidateMacaroon(mockContext, []bakery.Op{testOperation})
	if err != nil {
		t.Fatalf("Error validating the macaroon: %v", err)
	}
}

// TestValidateMacaroonAccountBalance tests the validation of an account bound
// macaroon that is in an incoming context.
func TestValidateMacaroonAccountBalance(t *testing.T) {
	// First, initialize and unlock the service.
	service, cleanup := setupService(t)
	defer cleanup()

	// Next, create a dummy account with a balance.
	account, err := service.NewAccount(9735, testExpDateFuture)
	if err != nil {
		t.Fatalf("Error creating account: %v", err)
	}

	// Then, create a macaroon that is not locked to an account.
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
	md := metadata.New(map[string]string{
		"macaroon": hex.EncodeToString(macaroonBinary),
	})
	mockContext := metadata.NewIncomingContext(context.Background(), md)
	err = service.ValidateAccountBalance(mockContext, 123)
	if err != nil {
		t.Fatalf("Error validating account balance: %v", err)
	}

	// Now create a macaroon that is locked to the account we created.
	lockedMac, err := macaroons.AddConstraints(
		macaroon.M(), macaroons.AccountLockConstraint(account.ID),
	)
	if err != nil {
		t.Fatalf("Error locking macaroon to account: %v", err)
	}
	macaroonBinary, err = lockedMac.MarshalBinary()
	if err != nil {
		t.Fatalf("Error serializing macaroon: %v", err)
	}
	md = metadata.New(map[string]string{
		"macaroon": hex.EncodeToString(macaroonBinary),
	})
	mockContext = metadata.NewIncomingContext(context.Background(), md)
	err = service.ValidateAccountBalance(mockContext, 123)
	if err != nil {
		t.Fatalf("Error validating account balance: %v", err)
	}
}

// TestChargeMacaroonAccountBalance tests the validation of an account bound
// macaroon that is in an incoming context.
func TestChargeMacaroonAccountBalance(t *testing.T) {
	// First, initialize and unlock the service.
	service, cleanup := setupService(t)
	defer cleanup()

	// Next, create a dummy account with a balance.
	account, err := service.NewAccount(9735, testExpDateFuture)
	if err != nil {
		t.Fatalf("Error creating account: %v", err)
	}

	// Then, create a macaroon that is not locked to an account.
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
	md := metadata.New(map[string]string{
		"macaroon": hex.EncodeToString(macaroonBinary),
	})
	mockContext := metadata.NewIncomingContext(context.Background(), md)
	err = service.ChargeAccountBalance(mockContext, 123)
	if err != nil {
		t.Fatalf("Error validating account balance: %v", err)
	}

	// Now create a macaroon that is locked to the account we created.
	lockedMac, err := macaroons.AddConstraints(
		macaroon.M(), macaroons.AccountLockConstraint(account.ID),
	)
	if err != nil {
		t.Fatalf("Error locking macaroon to account: %v", err)
	}
	macaroonBinary, err = lockedMac.MarshalBinary()
	if err != nil {
		t.Fatalf("Error serializing macaroon: %v", err)
	}
	md = metadata.New(map[string]string{
		"macaroon": hex.EncodeToString(macaroonBinary),
	})
	mockContext = metadata.NewIncomingContext(context.Background(), md)
	err = service.ChargeAccountBalance(mockContext, 123)
	if err != nil {
		t.Fatalf("Error validating account balance: %v", err)
	}

	// Finally check that the account now has a lower balance.
	updatedAccount, err := service.GetAccount(account.ID)
	if err != nil {
		t.Fatalf("Error getting account: %v", err)
	}
	if updatedAccount.CurrentBalance != (9735 - 123) {
		t.Fatalf(
			"Wrong account balance. Expected %d, got %d.",
			(9735 - 123), updatedAccount.CurrentBalance,
		)
	}
}
