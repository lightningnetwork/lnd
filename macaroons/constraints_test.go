package macaroons_test

import (
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/macaroons"
	macaroon "gopkg.in/macaroon.v2"
)

var (
	testRootKey                 = []byte("dummyRootKey")
	testID                      = []byte("dummyId")
	testLocation                = "lnd"
	testVersion                 = macaroon.LatestVersion
	expectedTimeCaveatSubstring = "time-before " + string(time.Now().Year())
)

func createDummyMacaroon(t *testing.T) *macaroon.Macaroon {
	dummyMacaroon, err := macaroon.New(testRootKey, testID,
		testLocation, testVersion)
	if err != nil {
		t.Fatalf("Error creating initial macaroon: %v", err)
	}
	return dummyMacaroon
}

func errContains(err error, str string) bool {
	return strings.Contains(err.Error(), str)
}

// TestAddConstraints tests that constraints can be added to an existing
// macaroon and therefore tighten its restrictions.
func TestAddConstraints(t *testing.T) {
	// We need a dummy macaroon to start with. Create one without
	// a bakery, because we mock everything anyway.
	initialMac := createDummyMacaroon(t)

	// Now add a constraint and make sure we have a cloned macaroon
	// with the constraint applied instead of a mutated initial one.
	newMac, err := macaroons.AddConstraints(initialMac,
		macaroons.TimeoutConstraint(1))
	if err != nil {
		t.Fatalf("Error adding constraint: %v", err)
	}
	if &newMac == &initialMac {
		t.Fatalf("Initial macaroon has been changed, something " +
			"went wrong!")
	}

	// Finally, test that the constraint has been added.
	if len(initialMac.Caveats()) == len(newMac.Caveats()) {
		t.Fatalf("No caveat has been added to the macaroon when " +
			"constraint was applied")
	}
}

// TestTimeoutConstraint tests that a caveat for the lifetime of
// a macaroon is created.
func TestTimeoutConstraint(t *testing.T) {
	// Get a configured version of the constraint function.
	constraintFunc := macaroons.TimeoutConstraint(3)

	// Now we need a dummy macaroon that we can apply the constraint
	// function to.
	testMacaroon := createDummyMacaroon(t)
	err := constraintFunc(testMacaroon)
	if err != nil {
		t.Fatalf("Error applying timeout constraint: %v", err)
	}

	// Finally, check that the created caveat has an acceptable value.
	if strings.HasPrefix(string(testMacaroon.Caveats()[0].Id),
		expectedTimeCaveatSubstring) {
		t.Fatalf(
			"Added caveat '%s' does not meet the expectations!",
			testMacaroon.Caveats()[0].Id,
		)
	}
}

// TestIPLockConstraint tests that a caveat for an IP address of
// a macaroon is created.
func TestIPLockConstraint(t *testing.T) {
	// Get a configured version of the constraint function.
	constraintFunc := macaroons.IPLockConstraint("127.0.0.1")

	// Now we need a dummy macaroon that we can apply the constraint
	// function to.
	testMacaroon := createDummyMacaroon(t)
	err := constraintFunc(testMacaroon)
	if err != nil {
		t.Fatalf("Error applying IP constraint: %v", err)
	}

	// Finally, check that the created caveat has an acceptable value.
	if string(testMacaroon.Caveats()[0].Id) != "ipaddr 127.0.0.1" {
		t.Fatalf(
			"Added caveat '%s' does not meet the expectations!",
			testMacaroon.Caveats()[0].Id,
		)
	}
}

// TestIPLockBadIP tests that an IP constraint cannot be added if the
// provided string is not a valid IP address.
func TestIPLockBadIP(t *testing.T) {
	constraintFunc := macaroons.IPLockConstraint("127.0.0/800")
	testMacaroon := createDummyMacaroon(t)
	err := constraintFunc(testMacaroon)
	if err == nil {
		t.Fatalf("IPLockConstraint with bad IP should fail.")
	}
}

// TestAccountLockConstraint tests that a macaroon can be constrained to an
// account.
func TestAccountLockConstraint(t *testing.T) {
	// Create a dummy macaroon with the constraint.
	con := macaroons.AccountLockConstraint(makeAccountID(t, "00aabb"))
	testMacaroon := createDummyMacaroon(t)
	err := con(testMacaroon)
	if err != nil {
		t.Fatalf("Error applying account constraint: %v", err)
	}

	// Then check that the created caveat has an acceptable value.
	if string(testMacaroon.Caveats()[0].Id) != "account 00aabb0000000000" {
		t.Fatalf(
			"Added caveat '%s' does not meet the expectations!",
			testMacaroon.Caveats()[0].Id,
		)
	}
}

// TestAccountLockChecker tests that a checker function is returned that checks
// the validity of an account macaroon.
func TestAccountLockChecker(t *testing.T) {
	// First, initialize and unlock the service, then create two accounts.
	service, cleanup := setupService(t)
	defer cleanup()
	goodAccount, err := service.NewAccount(9735, testExpDateFuture)
	if err != nil {
		t.Fatalf("Error creating account: %v", err)
	}
	expiredAccount, err := service.NewAccount(9735, testExpDatePast)
	if err != nil {
		t.Fatalf("Error creating account: %v", err)
	}

	// Now get the checker function that uses the service.
	checkerName, checkerFunc := macaroons.AccountLockChecker(service)
	if checkerName != macaroons.CondAccount {
		t.Fatalf(
			"Unexpected name. Expected %s, got %s.",
			macaroons.CondAccount, checkerName,
		)
	}

	// Test parsing of the account ID.
	err = checkerFunc(nil, "", "not-hex")
	if err == nil || !errContains(err, "invalid account id: ") {
		t.Fatalf("Parsing didn't fail.")
	}
	err = checkerFunc(nil, "", "aabbccdd")
	if err == nil || !errContains(err, "invalid account id length") {
		t.Fatalf("Parsing didn't fail.")
	}

	// Check account expiry.
	err = checkerFunc(nil, "", hex.EncodeToString(expiredAccount.ID[:]))
	if err != macaroons.ErrAccExpired {
		t.Fatalf("Expected account to be expired but got: %v", err)
	}

	// Finally test the good account.
	err = checkerFunc(nil, "", hex.EncodeToString(goodAccount.ID[:]))
	if err != nil {
		t.Fatalf("Error checking good account: %v", err)
	}
}
