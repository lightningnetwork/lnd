package macaroons

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"time"

	"google.golang.org/grpc/peer"

	"gopkg.in/macaroon-bakery.v2/bakery/checkers"
	macaroon "gopkg.in/macaroon.v2"

	"golang.org/x/net/context"
)

// contextKey is the type for our context keys. It needs to be a separate type
// to avoid collisions with other packages.
type contextKey int

// Constants for all the custom caveat conditions.
// Only first-party caveat conditions are currently supported.
const (
	CondIPAddress              = "ipaddr"
	CondRequestHash            = "request-hash"
	KeyRequestHash  contextKey = iota
)

// Constraint type adds a layer of indirection over macaroon caveats.
type Constraint func(*macaroon.Macaroon) error

// Checker type adds a layer of indirection over macaroon checkers. A Checker
// returns the name of the checker and the checker function; these are used to
// register the function with the bakery service's compound checker.
type Checker func() (string, checkers.Func)

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
func IPLockChecker() (string, checkers.Func) {
	return CondIPAddress, func(ctx context.Context, cond,
		arg string) error {
		// Get peer info and extract IP address from it for macaroon
		// check.
		pr, ok := peer.FromContext(ctx)
		if !ok {
			return fmt.Errorf(
				"unable to get peer info from context",
			)
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

// RequestHashConstraint locks macaroon to a specific request by hashing
// the request option.
// If the request hash is an empty string, this constraint does nothing to
// accommodate default value's desired behavior.
// If the request hash does not appear to be a SHA256 hash, an error is
// returned.
func RequestHashConstraint(reqHash string) func(*macaroon.Macaroon) error {
	return func(mac *macaroon.Macaroon) error {
		if reqHash != "" {
			// Make sure it's at least plausible to be a SHA256 hash
			// by hex decoding it and checking the length.
			hashHex, err := hex.DecodeString(reqHash)
			if err != nil {
				return fmt.Errorf(
					"unable to decode request hash: %v",
					err,
				)
			}
			if len(hashHex) != sha256.Size {
				return fmt.Errorf(
					"incorrect request hash length. "+
						"expected %d got %d",
					sha256.Size, len(hashHex),
				)
			}
			caveat := checkers.Condition(CondRequestHash, reqHash)
			return mac.AddFirstPartyCaveat([]byte(caveat))
		}
		return nil
	}
}

// RequestHashChecker accepts a request from the context that should have the
// hash of the request set as a value. That hash is then compared to the hash
// locked in the macaroon. It is of the `Checker` type.
func RequestHashChecker() (string, checkers.Func) {
	return CondRequestHash, func(ctx context.Context, cond,
		arg string) error {
		contextValue := ctx.Value(KeyRequestHash)
		if arg != contextValue {
			return fmt.Errorf(
				"macaroon locked to different request hash",
			)
		}
		return nil
	}
}
