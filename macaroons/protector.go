package macaroons

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"gopkg.in/macaroon-bakery.v2/bakery/checkers"
	macaroon "gopkg.in/macaroon.v2"
)

const (
	// CondProtector is the first party caveat condition name that is used
	// for protector caveats. A protector caveat restricts what request
	// fields may be set on the RPC methods covered by the named protector
	// profile. Every protector caveat entry is encoded as the string
	//
	//	"protector <profile-name>"
	//
	// in the serialized macaroon. The profile name refers to a profile
	// that is compiled into lnd. The set of methods a profile covers and
	// the fields it denies are owned by the release of lnd that validates
	// the macaroon, which means the restrictions of a given profile name
	// may be tightened (but never loosened) by a future release without
	// re-baking any macaroons.
	CondProtector = "protector"
)

// ErrInvalidProtectorProfile is returned if a protector profile name is
// malformed.
var ErrInvalidProtectorProfile = errors.New("invalid protector profile name")

// ProtectorProfileChecker is an interface that contains a single method for
// checking whether a protector profile with the given name is known to this
// version of lnd. A macaroon that references an unknown profile name must be
// rejected as a whole, otherwise a macaroon baked for a future profile could
// validate without any of the intended restrictions being enforced.
type ProtectorProfileChecker interface {
	// KnownProtectorProfile returns nil if a protector profile with the
	// given name is known and can be enforced. If the profile is unknown,
	// an error must be returned.
	KnownProtectorProfile(profile string) error
}

// ValidateProtectorProfileName checks that the given protector profile name is
// well formed: non-empty and free of whitespace or non-printable characters.
func ValidateProtectorProfileName(profile string) error {
	if profile == "" {
		return fmt.Errorf("%w: name cannot be empty",
			ErrInvalidProtectorProfile)
	}

	invalidRune := func(r rune) bool {
		return unicode.IsSpace(r) || !unicode.IsPrint(r)
	}
	if strings.IndexFunc(profile, invalidRune) >= 0 {
		return fmt.Errorf("%w: unexpected white space or "+
			"non-printable character in name %q",
			ErrInvalidProtectorProfile, profile)
	}

	return nil
}

// ProtectorConstraint returns a function that adds a protector caveat with the
// given profile name to a macaroon.
func ProtectorConstraint(profile string) Constraint {
	return func(mac *macaroon.Macaroon) error {
		if err := ValidateProtectorProfileName(profile); err != nil {
			return err
		}

		caveat := checkers.Condition(CondProtector, profile)

		return mac.AddFirstPartyCaveat([]byte(caveat))
	}
}

// ProtectorChecker returns a Checker that the macaroon bakery uses to verify
// protector caveats. The bakery level check only asserts that the referenced
// profile is well formed and known to this lnd instance; a macaroon
// referencing an unknown profile is rejected as a whole. The actual field
// level enforcement of the profile runs in the RPC interceptor chain where the
// request message is available, and runs independently of which macaroon
// validator (internal or external) accepted the macaroon.
func ProtectorChecker(checker ProtectorProfileChecker) Checker {
	check := func(_ context.Context, _, arg string) error {
		if err := ValidateProtectorProfileName(arg); err != nil {
			return err
		}

		return checker.KnownProtectorProfile(arg)
	}

	return func() (string, checkers.Func) {
		return CondProtector, check
	}
}

// GetProtectorProfiles returns the profile names of all protector caveats
// found in the given macaroon. A malformed protector caveat results in an
// error, so a caller failing closed on the error cannot be tricked into
// skipping enforcement by a caveat that almost parses. Caveats are decoded
// with the same parser the bakery uses for checking (checkers.ParseCaveat),
// so bake time encoding and enforcement time decoding cannot drift apart.
func GetProtectorProfiles(mac *macaroon.Macaroon) ([]string, error) {
	if mac == nil {
		return nil, nil
	}

	var profiles []string
	for _, caveat := range mac.Caveats() {
		caveatStr := string(caveat.Id)
		cond, arg, err := checkers.ParseCaveat(caveatStr)
		if err != nil {
			// ParseCaveat only fails for an empty caveat or one
			// starting with a space, neither of which can begin
			// with the protector keyword, so such a caveat cannot
			// have been meant as a protector caveat. The internal
			// bakery rejects macaroons carrying them anyway.
			continue
		}

		switch {
		case cond == CondProtector:
			err := ValidateProtectorProfileName(arg)
			if err != nil {
				return nil, err
			}

			profiles = append(profiles, arg)

		// A condition like "protector\tname" is not the protector
		// condition (the bakery splits conditions on a single space),
		// but it was almost certainly meant to be one. Fail closed
		// instead of silently ignoring the intended restriction.
		case protectorLikeCondition(cond):
			return nil, fmt.Errorf("%w: malformed protector "+
				"caveat %q", ErrInvalidProtectorProfile,
				caveatStr)
		}
	}

	return profiles, nil
}

// protectorLikeCondition returns true if the given caveat condition is not
// the protector condition itself but almost certainly was meant to be one:
// the protector keyword in any letter case, followed by nothing or by a rune
// that could not continue a longer, legitimately different condition name
// (e.g. "Protector name", "protector\tname", "protector:name",
// "protector=name"). Conditions that merely share the prefix but continue
// with a name rune ("protectorX", "protector-v2") are different conditions
// and are ignored.
func protectorLikeCondition(cond string) bool {
	if len(cond) < len(CondProtector) {
		return false
	}

	prefix := cond[:len(CondProtector)]
	if !strings.EqualFold(prefix, CondProtector) {
		return false
	}

	rest := cond[len(CondProtector):]
	if rest == "" {
		// The bare keyword itself, in the canonical case, is handled
		// by the caller via profile name validation; any other
		// casing of the bare keyword is protector like.
		return prefix != CondProtector
	}

	firstRune := []rune(rest)[0]
	nameRune := unicode.IsLetter(firstRune) || unicode.IsDigit(firstRune) ||
		firstRune == '-' || firstRune == '_'

	// A rune that could continue a longer condition name means this is a
	// different condition, unless the prefix used non-canonical casing.
	if nameRune {
		return prefix != CondProtector
	}

	return true
}
