package macaroons_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/lightningnetwork/lnd/macaroons"
	macaroon "gopkg.in/macaroon.v2"
)

var (
	testRootKey                 = []byte("dummyRootKey")
	testID                      = []byte("dummyId")
	testLocation                = "lnd"
	testVersion                 = macaroon.LatestVersion
	expectedTimeCaveatSubstring = fmt.Sprintf("time-before %d", time.Now().Year())
)

func createDummyMacaroon(t *testing.T) *macaroon.Macaroon {
	dummyMacaroon, err := macaroon.New(
		testRootKey, testID, testLocation, testVersion,
	)
	if err != nil {
		t.Fatalf("Error creating initial macaroon: %v", err)
	}
	return dummyMacaroon
}

// TestAddConstraints tests that constraints can be added to an existing
// macaroon and therefore tighten its restrictions.
func TestAddConstraints(t *testing.T) {
	// We need a dummy macaroon to start with. Create one without
	// a bakery, because we mock everything anyway.
	initialMac := createDummyMacaroon(t)

	// Now add a constraint and make sure we have a cloned macaroon
	// with the constraint applied instead of a mutated initial one.
	newMac, err := macaroons.AddConstraints(
		initialMac, macaroons.TimeoutConstraint(1),
	)
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

	// Finally, check that the created caveat has an
	// acceptable value.
	if !strings.HasPrefix(
		string(testMacaroon.Caveats()[0].Id),
		expectedTimeCaveatSubstring,
	) {
		t.Fatalf("Added caveat '%s' does not meet the expectations!",
			testMacaroon.Caveats()[0].Id)
	}
}

// TestTimeoutConstraint tests that a caveat for the lifetime of
// a macaroon is created.
func TestIpLockConstraint(t *testing.T) {
	// Get a configured version of the constraint function.
	constraintFunc := macaroons.IPLockConstraint("127.0.0.1")

	// Now we need a dummy macaroon that we can apply the constraint
	// function to.
	testMacaroon := createDummyMacaroon(t)
	err := constraintFunc(testMacaroon)
	if err != nil {
		t.Fatalf("Error applying timeout constraint: %v", err)
	}

	// Finally, check that the created caveat has an
	// acceptable value.
	if string(testMacaroon.Caveats()[0].Id) != "ipaddr 127.0.0.1" {
		t.Fatalf("Added caveat '%s' does not meet the expectations!",
			testMacaroon.Caveats()[0].Id)
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

// TestCustomConstraint tests that a custom constraint with a name and value can
// be added to a macaroon.
func TestCustomConstraint(t *testing.T) {
	// Test a custom caveat with a value first.
	constraintFunc := macaroons.CustomConstraint("unit-test", "test-value")
	testMacaroon := createDummyMacaroon(t)
	require.NoError(t, constraintFunc(testMacaroon))

	require.Equal(
		t, []byte("lnd-custom unit-test test-value"),
		testMacaroon.Caveats()[0].Id,
	)
	require.True(t, macaroons.HasCustomCaveat(testMacaroon, "unit-test"))
	require.False(t, macaroons.HasCustomCaveat(testMacaroon, "test-value"))
	require.False(t, macaroons.HasCustomCaveat(testMacaroon, "something"))
	require.False(t, macaroons.HasCustomCaveat(nil, "foo"))

	// Custom caveats don't necessarily need a value, just the name is fine
	// too to create a tagged macaroon.
	constraintFunc = macaroons.CustomConstraint("unit-test", "")
	testMacaroon = createDummyMacaroon(t)
	require.NoError(t, constraintFunc(testMacaroon))

	require.Equal(
		t, []byte("lnd-custom unit-test"), testMacaroon.Caveats()[0].Id,
	)
	require.True(t, macaroons.HasCustomCaveat(testMacaroon, "unit-test"))
	require.False(t, macaroons.HasCustomCaveat(testMacaroon, "test-value"))
	require.False(t, macaroons.HasCustomCaveat(testMacaroon, "something"))
}
