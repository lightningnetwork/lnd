package macaroons

import (
	"context"
	"encoding/hex"
	"fmt"
	"net"
	"time"

	"google.golang.org/grpc/peer"
	"gopkg.in/macaroon-bakery.v2/bakery/checkers"
	"gopkg.in/macaroon.v2"
)

// Constants for all the custom caveat conditions.
// Only first-party caveat conditions are currently supported.
const (
	CondIPAddress = "ipaddr"
	CondAccount   = "account"
)

// Constraint type adds a layer of indirection over macaroon caveats.
type Constraint func(*macaroon.Macaroon) error

// Checker type adds a layer of indirection over macaroon checkers. A Checker
// returns the name of the checker and the checker function; these are used to
// register the function with the bakery service's compound checker.
type Checker func(*Service) (string, checkers.Func)

// AddConstraints returns new derived macaroon by applying every passed
// constraint and tightening its restrictions.
func AddConstraints(mac *macaroon.Macaroon,
	cs ...Constraint) (*macaroon.Macaroon, error) {

	newMac := mac.Clone()
	for _, constraint := range cs {
		if err := constraint(newMac); err != nil {
			return nil, err
		}
	}
	return newMac, nil
}

// GetCaveatArgOfCondition parses the caveats of a macaroon and returns the
// argument of the caveat that matches the condition name given.
func GetCaveatArgOfCondition(mac *macaroon.Macaroon, cond string) string {
	for _, caveat := range mac.Caveats() {
		caveatCond, arg, err := checkers.ParseCaveat(string(caveat.Id))
		if err != nil {
			// If we can't parse the caveat, it probably doesn't
			// concern us.
			continue
		}
		if caveatCond == cond {
			return arg
		}
	}
	return ""
}

// Each *Constraint function is a functional option, which takes a pointer
// to the macaroon and adds another restriction to it. For each *Constraint,
// the corresponding *Checker is provided if not provided by default.

// TimeoutConstraint restricts the lifetime of the macaroon
// to the amount of seconds given.
func TimeoutConstraint(seconds int64) func(*macaroon.Macaroon) error {
	return func(mac *macaroon.Macaroon) error {
		macaroonTimeout := time.Duration(seconds)
		requestTimeout := time.Now().Add(time.Second * macaroonTimeout)
		caveat := checkers.TimeBeforeCaveat(requestTimeout)
		return mac.AddFirstPartyCaveat([]byte(caveat.Condition))
	}
}

// IPLockConstraint locks macaroon to a specific IP address.
// If address is an empty string, this constraint does nothing to
// accommodate default value's desired behavior.
func IPLockConstraint(ipAddr string) func(*macaroon.Macaroon) error {
	return func(mac *macaroon.Macaroon) error {
		if ipAddr != "" {
			macaroonIPAddr := net.ParseIP(ipAddr)
			if macaroonIPAddr == nil {
				return fmt.Errorf(
					"incorrect macaroon IP-lock address",
				)
			}
			caveat := checkers.Condition(
				CondIPAddress, macaroonIPAddr.String(),
			)
			return mac.AddFirstPartyCaveat([]byte(caveat))
		}
		return nil
	}
}

// IPLockChecker accepts client IP from the validation context and compares it
// with IP locked in the macaroon. It is of the `Checker` type.
func IPLockChecker(service *Service) (string, checkers.Func) {
	return CondIPAddress, func(ctx context.Context, cond,
		arg string) error {

		// Get peer info and extract IP address from it for macaroon
		// check.
		pr, ok := peer.FromContext(ctx)
		if !ok {
			return fmt.Errorf("unable to get peer info from context")
		}
		peerAddr, _, err := net.SplitHostPort(pr.Addr.String())
		if err != nil {
			return fmt.Errorf("unable to parse peer address")
		}

		if !net.ParseIP(arg).Equal(net.ParseIP(peerAddr)) {
			msg := "macaroon locked to different IP address"
			return fmt.Errorf(msg)
		}
		return nil
	}
}

// AccountLockConstraint locks a macaroon to a specific account. So every action
// that sends funds requires this specific account to have enough balance to
// perform the action. Once the action was successful, the sent amount is
// subtracted from the account's balance.
func AccountLockConstraint(accountID AccountID) func(*macaroon.Macaroon) error {
	return func(mac *macaroon.Macaroon) error {
		caveat := checkers.Condition(
			CondAccount, hex.EncodeToString(accountID[:]),
		)
		return mac.AddFirstPartyCaveat([]byte(caveat))
	}
}

// AccountLockChecker checks that the account the macaroon is locked to exists
// and has not yet expired. The actual balance check happens in the rpcserver.
func AccountLockChecker(service *Service) (string, checkers.Func) {
	return CondAccount, func(ctx context.Context, cond, arg string) error {
		id, err := hex.DecodeString(arg)
		if err != nil {
			return fmt.Errorf("invalid account id: %v", err)
		}
		if len(id) != AccountIDLen {
			return fmt.Errorf("invalid account id length")
		}
		var accountID AccountID
		copy(accountID[:], id)
		account, err := service.GetAccount(accountID)
		if err != nil {
			return err
		}

		if account.HasExpired() {
			return ErrAccExpired
		}
		return nil
	}
}
