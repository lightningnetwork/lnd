package blob

import (
	"fmt"
	"strings"

	"github.com/lightningnetwork/lnd/channeldb"
)

// Flag represents a specify option that can be present in a Type.
type Flag uint16

const (
	// FlagReward signals that the justice transaction should contain an
	// additional output for itself. Signatures sent by the client should
	// include the reward script negotiated during session creation. Without
	// the flag, there is only one output sweeping clients funds back to
	// them solely.
	FlagReward Flag = 1

	// FlagCommitOutputs signals that the blob contains the information
	// required to sweep commitment outputs.
	FlagCommitOutputs Flag = 1 << 1

	// FlagAnchorChannel signals that this blob is meant to spend an anchor
	// channel, and therefore must expect a P2WSH-style to-remote output if
	// one exists.
	FlagAnchorChannel Flag = 1 << 2

	// FlagTaprootChannel signals that this blob is meant to spend a
	// taproot channel and therefore must expect P2TR outputs.
	FlagTaprootChannel Flag = 1 << 3
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
	case FlagAnchorChannel:
		return "FlagAnchorChannel"
	case FlagTaprootChannel:
		return "FlagTaprootChannel"
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

const (
	// TypeAltruistCommit sweeps only commitment outputs to a sweep address
	// controlled by the user, and does not give the tower a reward.
	TypeAltruistCommit = Type(FlagCommitOutputs)

	// TypeAltruistAnchorCommit sweeps only commitment outputs from an
	// anchor commitment to a sweep address controlled by the user, and does
	// not give the tower a reward.
	TypeAltruistAnchorCommit = Type(FlagCommitOutputs | FlagAnchorChannel)

	// TypeRewardCommit sweeps only commitment outputs to a sweep address
	// controlled by the user, and pays a negotiated reward to the tower.
	TypeRewardCommit = Type(FlagCommitOutputs | FlagReward)

	// TypeAltruistTaprootCommit sweeps only the commitment outputs from a
	// taproot channel commitment to a sweep address controlled by the user,
	// and does not give the tower a reward.
	TypeAltruistTaprootCommit = Type(FlagCommitOutputs | FlagTaprootChannel)
)

// TypeFromChannel returns the appropriate blob Type for the given channel
// type.
func TypeFromChannel(chanType channeldb.ChannelType) Type {
	switch {
	case chanType.IsTaproot():
		return TypeAltruistTaprootCommit
	case chanType.HasAnchors():
		return TypeAltruistAnchorCommit
	default:
		return TypeAltruistCommit
	}
}

// Identifier returns a unique, stable string identifier for the blob Type.
func (t Type) Identifier() (string, error) {
	switch t {
	case TypeAltruistCommit:
		return "legacy", nil
	case TypeAltruistAnchorCommit:
		return "anchor", nil
	case TypeRewardCommit:
		return "reward", nil
	case TypeAltruistTaprootCommit:
		return "taproot", nil
	default:
		return "", fmt.Errorf("unknown blob type: %v", t)
	}
}

// CommitmentType returns the appropriate CommitmentType for the given blob Type
// and channel type.
func (t Type) CommitmentType(chanType *channeldb.ChannelType) (CommitmentType,
	error) {

	switch {
	case t.Has(FlagTaprootChannel):
		return TaprootCommitment, nil

	case t.Has(FlagAnchorChannel):
		return AnchorCommitment, nil

	case t.Has(FlagCommitOutputs):
		if chanType != nil && chanType.IsTweakless() {
			return LegacyTweaklessCommitment, nil
		}

		return LegacyCommitment, nil

	default:
		return 0, ErrUnknownBlobType
	}
}

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

// IsAnchorChannel returns true if the blob type is for an anchor channel.
func (t Type) IsAnchorChannel() bool {
	return t.Has(FlagAnchorChannel)
}

// IsTaprootChannel returns true if the blob type is for a taproot channel.
func (t Type) IsTaprootChannel() bool {
	return t.Has(FlagTaprootChannel)
}

// knownFlags maps the supported flags to their name.
var knownFlags = map[Flag]struct{}{
	FlagReward:         {},
	FlagCommitOutputs:  {},
	FlagAnchorChannel:  {},
	FlagTaprootChannel: {},
}

// String returns a human-readable description of a Type.
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
	TypeAltruistCommit:        {},
	TypeRewardCommit:          {},
	TypeAltruistAnchorCommit:  {},
	TypeAltruistTaprootCommit: {},
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
