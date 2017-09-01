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
type Constraint func(*macaroon.Macaroon)

// AddConstraints returns new derived macaroon by applying every passed
// constraint and tightening its restrictions.
func AddConstraints(mac *macaroon.Macaroon, cs ...Constraint) *macaroon.Macaroon {
	newMac := mac.Clone()
	for _, constraint := range cs {
		constraint(newMac)
	}
	return newMac
}

// Each *Constraint function is a functional option, which takes a pointer
// to the macaroon and adds another restriction to it. For each *Constraint,
// the corresponding *Checker is provided.

// PermissionsConstraint restricts allowed operations set to the ones
// passed to it.
func PermissionsConstraint(ops ...string) func(*macaroon.Macaroon) {
	return func(mac *macaroon.Macaroon) {
		caveat := checkers.AllowCaveat(ops...)
		mac.AddFirstPartyCaveat(caveat.Condition)
	}
}

// PermissionsChecker wraps default checkers.OperationChecker.
func PermissionsChecker(method string) checkers.Checker {
	return checkers.OperationChecker(method)
}

// TimeoutConstraint restricts the lifetime of the macaroon
// to the amount of seconds given.
func TimeoutConstraint(seconds int64) func(*macaroon.Macaroon) {
	return func(mac *macaroon.Macaroon) {
		macaroonTimeout := time.Duration(seconds)
		requestTimeout := time.Now().Add(time.Second * macaroonTimeout)
		caveat := checkers.TimeBeforeCaveat(requestTimeout)
		mac.AddFirstPartyCaveat(caveat.Condition)
	}
}

// TimeoutChecker wraps default checkers.TimeBefore checker.
func TimeoutChecker() checkers.Checker {
	return checkers.TimeBefore
}

// IPLockConstraint locks macaroon to a specific IP address.
// If address is an empty string, this constraint does nothing to
// accommodate default value's desired behavior.
func IPLockConstraint(ipAddr string) func(*macaroon.Macaroon) {
	return func(mac *macaroon.Macaroon) {
		if ipAddr != "" {
			macaroonIPAddr := net.ParseIP(ipAddr)
			caveat := checkers.ClientIPAddrCaveat(macaroonIPAddr)
			mac.AddFirstPartyCaveat(caveat.Condition)
		}
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
