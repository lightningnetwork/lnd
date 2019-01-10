package blob

import (
	"fmt"
	"strings"
)

// Flag represents a specify option that can be present in a Type.
type Flag uint16

const (
	// FlagReward signals that the justice transaction should contain an
	// additional output for itself. Signatures sent by the client should
	// include the reward script negotiated during session creation. Without
	// the flag, there is only one output sweeping clients funds back to
	// them solely.
	FlagReward Flag = 1 << iota

	// FlagCommitOutputs signals that the blob contains the information
	// required to sweep commitment outputs.
	FlagCommitOutputs
)

// Type returns a Type consisting solely of this flag enabled.
func (f Flag) Type() Type {
	return Type(f)
}

// String returns the name of the flag.
func (f Flag) String() string {
	switch f {
	case FlagReward:
		return "FlagReward"
	case FlagCommitOutputs:
		return "FlagCommitOutputs"
	default:
		return "FlagUnknown"
	}
}

// Type is a bit vector composed of Flags that govern various aspects of
// reconstructing the justice transaction from an encrypted blob. The flags can
// be used to signal behaviors such as which inputs are being swept, which
// outputs should be added to the justice transaction, or modify serialization
// of the blob itself.
type Type uint16

// TypeDefault sweeps only commitment outputs to a sweep address controlled by
// the user, and does not give the tower a reward.
const TypeDefault = Type(FlagCommitOutputs)

// Has returns true if the Type has the passed flag enabled.
func (t Type) Has(flag Flag) bool {
	return Flag(t)&flag == flag
}

// TypeFromFlags creates a single Type from an arbitrary list of flags.
func TypeFromFlags(flags ...Flag) Type {
	var typ Type
	for _, flag := range flags {
		typ |= Type(flag)
	}

	return typ
}

// knownFlags maps the supported flags to their name.
var knownFlags = map[Flag]struct{}{
	FlagReward:        {},
	FlagCommitOutputs: {},
}

// String returns a human readable description of a Type.
func (t Type) String() string {
	var (
		hrPieces        []string
		hasUnknownFlags bool
	)

	// Iterate through the possible flags from highest to lowest. This will
	// ensure that the human readable names will be in the same order as the
	// bits (left to right) if the type were to be printed in big-endian
	// byte order.
	for f := Flag(1 << 15); f != 0; f >>= 1 {
		// If this flag is known, we'll add a human-readable name or its
		// inverse depending on whether the type has this flag set.
		if _, ok := knownFlags[f]; ok {
			if t.Has(f) {
				hrPieces = append(hrPieces, f.String())
			} else {
				hrPieces = append(hrPieces, "No-"+f.String())
			}
		} else {
			// Make note of any unknown flags that this type has
			// set. If any are present, we'll prepend the bit-wise
			// representation of the type in the final string.
			if t.Has(f) {
				hasUnknownFlags = true
			}
		}
	}

	// If there were no unknown flags, we'll simply return the list of human
	// readable pieces.
	if !hasUnknownFlags {
		return fmt.Sprintf("[%s]", strings.Join(hrPieces, "|"))
	}

	// Otherwise, we'll prepend the bit-wise representation to the human
	// readable names.
	return fmt.Sprintf("%016b[%s]", t, strings.Join(hrPieces, "|"))
}

// supportedTypes is the set of all configurations known to be supported by the
// package.
var supportedTypes = map[Type]struct{}{
	FlagCommitOutputs.Type():                {},
	(FlagCommitOutputs | FlagReward).Type(): {},
}

// IsSupportedType returns true if the given type is supported by the package.
func IsSupportedType(blobType Type) bool {
	_, ok := supportedTypes[blobType]
	return ok
}

// SupportedTypes returns a list of all supported blob types.
func SupportedTypes() []Type {
	supported := make([]Type, 0, len(supportedTypes))
	for t := range supportedTypes {
		supported = append(supported, t)
	}
	return supported
}
