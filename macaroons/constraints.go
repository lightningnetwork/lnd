package macaroons

import (
	"fmt"
	"net"
	"time"

	"gopkg.in/macaroon-bakery.v1/bakery/checkers"
	macaroon "gopkg.in/macaroon.v1"
)

// Constraint type adds a layer of indirection over macaroon caveats and
// checkers.
type Constraint func(*macaroon.Macaroon) error

// AddConstraints returns new derived macaroon by applying every passed
// constraint and tightening its restrictions.
func AddConstraints(mac *macaroon.Macaroon, cs ...Constraint) (*macaroon.Macaroon, error) {
	newMac := mac.Clone()
	for _, constraint := range cs {
		if err := constraint(newMac); err != nil {
			return nil, err
		}
	}
	return newMac, nil
}

// Each *Constraint function is a functional option, which takes a pointer
// to the macaroon and adds another restriction to it. For each *Constraint,
// the corresponding *Checker is provided.

// AllowConstraint restricts allowed operations set to the ones
// passed to it.
func AllowConstraint(ops ...string) func(*macaroon.Macaroon) error {
	return func(mac *macaroon.Macaroon) error {
		caveat := checkers.AllowCaveat(ops...)
		return mac.AddFirstPartyCaveat(caveat.Condition)
	}
}

// AllowChecker wraps default checkers.OperationChecker.
func AllowChecker(method string) checkers.Checker {
	return checkers.OperationChecker(method)
}

// TimeoutConstraint restricts the lifetime of the macaroon
// to the amount of seconds given.
func TimeoutConstraint(seconds int64) func(*macaroon.Macaroon) error {
	return func(mac *macaroon.Macaroon) error {
		macaroonTimeout := time.Duration(seconds)
		requestTimeout := time.Now().Add(time.Second * macaroonTimeout)
		caveat := checkers.TimeBeforeCaveat(requestTimeout)
		return mac.AddFirstPartyCaveat(caveat.Condition)
	}
}

// TimeoutChecker wraps default checkers.TimeBefore checker.
func TimeoutChecker() checkers.Checker {
	return checkers.TimeBefore
}

// IPLockConstraint locks macaroon to a specific IP address.
// If address is an empty string, this constraint does nothing to
// accommodate default value's desired behavior.
func IPLockConstraint(ipAddr string) func(*macaroon.Macaroon) error {
	return func(mac *macaroon.Macaroon) error {
		if ipAddr != "" {
			macaroonIPAddr := net.ParseIP(ipAddr)
			if macaroonIPAddr == nil {
				return fmt.Errorf("incorrect macaroon IP-lock address")
			}
			caveat := checkers.ClientIPAddrCaveat(macaroonIPAddr)
			return mac.AddFirstPartyCaveat(caveat.Condition)
		}
		return nil
	}
}

// IPLockChecker accepts client IP from the validation context and compares it
// with IP locked in the macaroon.
func IPLockChecker(clientIP string) checkers.Checker {
	return checkers.CheckerFunc{
		Condition_: checkers.CondClientIPAddr,
		Check_: func(_, cav string) error {
			if !net.ParseIP(cav).Equal(net.ParseIP(clientIP)) {
				msg := "macaroon locked to different IP address"
				return fmt.Errorf(msg)
			}
			return nil
		},
	}
}
