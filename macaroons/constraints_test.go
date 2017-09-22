package macaroons

import (
	"testing"
	"time"

	"gopkg.in/macaroon-bakery.v1/bakery"
	"gopkg.in/macaroon-bakery.v1/bakery/checkers"
	macaroon "gopkg.in/macaroon.v1"
)

func TestAllowConstraint(t *testing.T) {
	macParams := bakery.NewServiceParams{}
	svc, err := bakery.NewService(macParams)
	if err != nil {
		t.Fatalf("Failed to create a new service")
	}
	mac, err := svc.NewMacaroon("", nil, nil)
	if err != nil {
		t.Fatalf("Failed to create a new macaroon")
	}

	constraint := AllowConstraint("op1", "op2", "op4")
	mac, err = AddConstraints(mac, constraint)
	if err != nil {
		t.Fatalf("Failed to add macaroon constraint")
	}

	checker := checkers.New(AllowChecker("op1"))
	if err := svc.Check(macaroon.Slice{mac}, checker); err != nil {
		t.Fatalf("Allowed operation failed macaroon check")
	}

	checker = checkers.New(AllowChecker("op3"))
	if err := svc.Check(macaroon.Slice{mac}, checker); err == nil {
		t.Fatalf("Disallowed operation passed macaroon check")
	}
}

func TestTimeoutConstraint(t *testing.T) {
	macParams := bakery.NewServiceParams{}
	svc, err := bakery.NewService(macParams)
	if err != nil {
		t.Fatalf("Failed to create a new service")
	}
	mac, err := svc.NewMacaroon("", nil, nil)
	if err != nil {
		t.Fatalf("Failed to create a new macaroon")
	}

	constraint := TimeoutConstraint(1)
	mac, err = AddConstraints(mac, constraint)
	if err != nil {
		t.Fatalf("Failed to add macaroon constraint")
	}

	checker := checkers.New(TimeoutChecker())
	if err := svc.Check(macaroon.Slice{mac}, checker); err != nil {
		t.Fatalf("Timeout check failed within timeframe")
	}

	time.Sleep(time.Second)
	if err := svc.Check(macaroon.Slice{mac}, checker); err == nil {
		t.Fatalf("Timeout check passed for an expired timeout")
	}
}

func TestIPLockConstraint(t *testing.T) {
	macParams := bakery.NewServiceParams{}
	svc, err := bakery.NewService(macParams)
	if err != nil {
		t.Fatalf("Failed to create a new service")
	}
	mac, err := svc.NewMacaroon("", nil, nil)
	if err != nil {
		t.Fatalf("Failed to create a new macaroon")
	}

	constraint := IPLockConstraint("127.0.0.1")
	mac, err = AddConstraints(mac, constraint)
	if err != nil {
		t.Fatalf("Failed to add macaroon constraint")
	}

	checker := checkers.New(IPLockChecker("127.0.0.1"))
	if err := svc.Check(macaroon.Slice{mac}, checker); err != nil {
		t.Fatalf("IPLock for the same IP failed the test")
	}

	checker = checkers.New(IPLockChecker("0.0.0.0"))
	if err := svc.Check(macaroon.Slice{mac}, checker); err == nil {
		t.Fatalf("IPLock for a different IP passed the test")
	}
}
