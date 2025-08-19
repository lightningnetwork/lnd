package macaroons

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"google.golang.org/grpc/peer"
	"gopkg.in/macaroon-bakery.v2/bakery/checkers"
	macaroon "gopkg.in/macaroon.v2"
)

const (
	// CondLndCustom is the first party caveat condition name that is used
	// for all custom caveats in lnd. Every custom caveat entry will be
	// encoded as the string
	// "lnd-custom <custom-caveat-name> <custom-caveat-condition>"
	// in the serialized macaroon. We choose a single space as the delimiter
	// between the because that is also used by the macaroon bakery library.
	CondLndCustom = "lnd-custom"

	// CondIPRange is the caveat condition name that is used for tying an IP
	// range to a macaroon.
	CondIPRange = "iprange"
)

// CustomCaveatAcceptor is an interface that contains a single method for
// checking whether a macaroon with the given custom caveat name should be
// accepted or not.
type CustomCaveatAcceptor interface {
	// CustomCaveatSupported returns nil if a macaroon with the given custom
	// caveat name can be validated by any component in lnd (for example an
	// RPC middleware). If no component is registered to handle the given
	// custom caveat then an error must be returned. This method only checks
	// the availability of a validating component, not the validity of the
	// macaroon itself.
	CustomCaveatSupported(customCaveatName string) error
}

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

	// The macaroon library's Clone() method has a subtle bug that doesn't
	// correctly clone all caveats. We need to use our own, safe clone
	// function instead.
	newMac, err := SafeCopyMacaroon(mac)
	if err != nil {
		return nil, err
	}

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

// IPLockConstraint locks a macaroon to a specific IP address. If ipAddr is an
// empty string, this constraint does nothing to accommodate  default value's
// desired behavior.
func IPLockConstraint(ipAddr string) func(*macaroon.Macaroon) error {
	return func(mac *macaroon.Macaroon) error {
		if ipAddr != "" {
			macaroonIPAddr := net.ParseIP(ipAddr)
			if macaroonIPAddr == nil {
				return fmt.Errorf("incorrect macaroon IP-" +
					"lock address")
			}
			caveat := checkers.Condition("ipaddr",
				macaroonIPAddr.String())

			return mac.AddFirstPartyCaveat([]byte(caveat))
		}

		return nil
	}
}

// IPRangeLockConstraint locks a macaroon to a specific IP address range. If
// ipRange is an empty string, this constraint does nothing to accommodate
// default value's desired behavior.
func IPRangeLockConstraint(ipRange string) func(*macaroon.Macaroon) error {
	return func(mac *macaroon.Macaroon) error {
		if ipRange != "" {
			_, parsedNet, err := net.ParseCIDR(ipRange)
			if err != nil {
				return fmt.Errorf("incorrect macaroon IP "+
					"range: %w", err)
			}
			caveat := checkers.Condition(
				CondIPRange, parsedNet.String(),
			)

			return mac.AddFirstPartyCaveat([]byte(caveat))
		}

		return nil
	}
}

// IPLockChecker accepts client IP from the validation context and compares it
// with IP locked in the macaroon. It is of the `Checker` type.
func IPLockChecker() (string, checkers.Func) {
	return "ipaddr", func(ctx context.Context, cond, arg string) error {
		// Get peer info and extract IP address from it for macaroon
		// check.
		pr, ok := peer.FromContext(ctx)
		if !ok {
			return fmt.Errorf("unable to get peer info from " +
				"context")
		}
		peerAddr, _, err := net.SplitHostPort(pr.Addr.String())
		if err != nil {
			return fmt.Errorf("unable to parse peer address")
		}

		if !net.ParseIP(arg).Equal(net.ParseIP(peerAddr)) {
			return fmt.Errorf("macaroon locked to different IP " +
				"address")
		}
		return nil
	}
}

// IPRangeLockChecker accepts client IP range from the validation context and
// compares it with the IP range locked in the macaroon. It is of the `Checker`
// type.
func IPRangeLockChecker() (string, checkers.Func) {
	return CondIPRange, func(ctx context.Context, cond, arg string) error {
		// Get peer info and extract IP range from it for macaroon
		// check.
		pr, ok := peer.FromContext(ctx)
		if !ok {
			return errors.New("unable to get peer info from " +
				"context")
		}
		peerAddr, _, err := net.SplitHostPort(pr.Addr.String())
		if err != nil {
			return fmt.Errorf("unable to parse peer address: %w",
				err)
		}

		_, ipNet, err := net.ParseCIDR(arg)
		if err != nil {
			return fmt.Errorf("unable to parse macaroon IP "+
				"range: %w", err)
		}

		if !ipNet.Contains(net.ParseIP(peerAddr)) {
			return errors.New("macaroon locked to different " +
				"IP range")
		}

		return nil
	}
}

// CustomConstraint returns a function that adds a custom caveat condition to
// a macaroon.
func CustomConstraint(name, condition string) func(*macaroon.Macaroon) error {
	return func(mac *macaroon.Macaroon) error {
		// We rely on a name being set for the interception, so don't
		// allow creating a caveat without a name in the first place.
		if name == "" {
			return fmt.Errorf("name cannot be empty")
		}

		// The inner (custom) condition is optional.
		outerCondition := fmt.Sprintf("%s %s", name, condition)
		if condition == "" {
			outerCondition = name
		}

		caveat := checkers.Condition(CondLndCustom, outerCondition)
		return mac.AddFirstPartyCaveat([]byte(caveat))
	}
}

// CustomChecker returns a Checker function that is used by the macaroon bakery
// library to check whether a custom caveat is supported by lnd in general or
// not. Support in this context means: An additional gRPC interceptor was set up
// that validates the content (=condition) of the custom caveat. If such an
// interceptor is in place then the acceptor should return a nil error. If no
// interceptor exists for the custom caveat in the macaroon of a request context
// then a non-nil error should be returned and the macaroon is rejected as a
// whole.
func CustomChecker(acceptor CustomCaveatAcceptor) Checker {
	// We return the general name of all lnd custom macaroons and a function
	// that splits the outer condition to extract the name of the custom
	// condition and the condition itself. In the bakery library that's used
	// here, a caveat always has the following form:
	//
	// <condition-name> <condition-value>
	//
	// Because a checker function needs to be bound to the condition name we
	// have to choose a static name for the first part ("lnd-custom", see
	// CondLndCustom. Otherwise we'd need to register a new Checker function
	// for each custom caveat that's registered. To allow for a generic
	// custom caveat handling, we just add another layer and expand the
	// initial <condition-value> into
	//
	// "<custom-condition-name> <custom-condition-value>"
	//
	// The full caveat string entry of a macaroon that uses this generic
	// mechanism would therefore look like this:
	//
	// "lnd-custom <custom-condition-name> <custom-condition-value>"
	checker := func(_ context.Context, _, outerCondition string) error {
		if outerCondition != strings.TrimSpace(outerCondition) {
			return fmt.Errorf("unexpected white space found in " +
				"caveat condition")
		}
		if outerCondition == "" {
			return fmt.Errorf("expected custom caveat, got empty " +
				"string")
		}

		// The condition part of the original caveat is now name and
		// condition of the custom caveat (we add a layer of conditions
		// to allow one custom checker to work for all custom lnd
		// conditions that implement arbitrary business logic).
		parts := strings.Split(outerCondition, " ")
		customCaveatName := parts[0]

		return acceptor.CustomCaveatSupported(customCaveatName)
	}

	return func() (string, checkers.Func) {
		return CondLndCustom, checker
	}
}

// HasCustomCaveat tests if the given macaroon has a custom caveat with the
// given custom caveat name.
func HasCustomCaveat(mac *macaroon.Macaroon, customCaveatName string) bool {
	if mac == nil {
		return false
	}

	caveatPrefix := []byte(fmt.Sprintf(
		"%s %s", CondLndCustom, customCaveatName,
	))
	for _, caveat := range mac.Caveats() {
		if bytes.HasPrefix(caveat.Id, caveatPrefix) {
			return true
		}
	}

	return false
}

// GetCustomCaveatCondition returns the custom caveat condition for the given
// custom caveat name from the given macaroon.
func GetCustomCaveatCondition(mac *macaroon.Macaroon,
	customCaveatName string) string {

	if mac == nil {
		return ""
	}

	caveatPrefix := []byte(fmt.Sprintf(
		"%s %s ", CondLndCustom, customCaveatName,
	))
	for _, caveat := range mac.Caveats() {
		// The caveat id has a format of
		// "lnd-custom [custom-caveat-name] [custom-caveat-condition]"
		// and we only want the condition part. If we match the prefix
		// part we return the condition that comes after the prefix.
		if bytes.HasPrefix(caveat.Id, caveatPrefix) {
			caveatSplit := strings.SplitN(
				string(caveat.Id),
				string(caveatPrefix),
				2,
			)
			if len(caveatSplit) == 2 {
				return caveatSplit[1]
			}
		}
	}

	// We didn't find a condition for the given custom caveat name.
	return ""
}
