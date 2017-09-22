package macaroons

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"gopkg.in/macaroon-bakery.v1/bakery"
	"gopkg.in/macaroon-bakery.v1/bakery/checkers"
	macaroon "gopkg.in/macaroon.v1"
)

type macError struct {
	message string
}

func (err macError) Error() string {
	return err.message
}

func testConstraint(constraint Constraint, ok checkers.Checker,
	failFn func() checkers.Checker) error {
	macParams := bakery.NewServiceParams{}
	svc, err := bakery.NewService(macParams)
	if err != nil {
		return errors.New("Failed to create a new service")
	}
	mac, err := svc.NewMacaroon("", nil, nil)
	if err != nil {
		return errors.New("Failed to create a new macaroon")
	}

	mac, err = AddConstraints(mac, constraint)
	if err != nil {
		return errors.New("Failed to add macaroon constraint")
	}

	okChecker := checkers.New(ok)
	if err := svc.Check(macaroon.Slice{mac}, okChecker); err != nil {
		msg := "Correct checker failed: %v"
		return macError{fmt.Sprintf(msg, ok)}
	}

	fail := failFn()
	failChecker := checkers.New(fail)
	if err := svc.Check(macaroon.Slice{mac}, failChecker); err == nil {
		msg := "Incorrect checker succeeded: %v"
		return macError{fmt.Sprintf(msg, fail)}
	}
	return nil
}

func TestAllowConstraint(t *testing.T) {
	if err := testConstraint(
		AllowConstraint("op1", "op2", "op4"),
		AllowChecker("op1"),
		func() checkers.Checker {
			return AllowChecker("op3")
		},
	); err != nil {
		t.Fatalf(err.Error())
	}
}

func TestTimeoutConstraint(t *testing.T) {
	if err := testConstraint(
		TimeoutConstraint(1),
		TimeoutChecker(),
		func() checkers.Checker {
			time.Sleep(time.Second)
			return TimeoutChecker()
		},
	); err != nil {
		t.Fatalf(err.Error())
	}
}

func TestIPLockConstraint(t *testing.T) {
	if err := testConstraint(
		IPLockConstraint("127.0.0.1"),
		IPLockChecker("127.0.0.1"),
		func() checkers.Checker {
			return IPLockChecker("0.0.0.0")
		},
	); err != nil {
		t.Fatalf(err.Error())
	}
}

func TestIPLockEmptyIP(t *testing.T) {
	if err := testConstraint(
		IPLockConstraint(""),
		IPLockChecker("127.0.0.1"),
		func() checkers.Checker {
			return IPLockChecker("0.0.0.0")
		},
	); err != nil {
		if _, ok := err.(macError); !ok {
			t.Fatalf("IPLock with an empty IP should always pass")
		}
	}
}

func TestIPLockBadIP(t *testing.T) {
	if err := IPLockConstraint("127.0.0/800"); err == nil {
		t.Fatalf("IPLockConstraint with bad IP should fail")
	}
}
