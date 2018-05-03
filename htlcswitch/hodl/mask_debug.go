// +build debug

package hodl

import (
	"fmt"
	"strings"
)

// DebugBuild signals that this is a debug build.
const DebugBuild = true

// MaskFromFlags merges a variadic set of Flags into a single Mask.
func MaskFromFlags(flags ...Flag) Mask {
	var mask Mask
	for _, flag := range flags {
		mask |= Mask(flag)
	}

	return mask
}

// Active returns true if the bit corresponding to the flag is set within the
// mask.
func (m Mask) Active(flag Flag) bool {
	return (Flag(m) & flag) > 0
}

// String returns a human-readable description of all active Flags.
func (m Mask) String() string {
	if m == MaskNone {
		return "hodl.Mask(NONE)"
	}

	var activeFlags []string
	for i := uint(0); i < 32; i++ {
		flag := Flag(1 << i)
		if m.Active(flag) {
			activeFlags = append(activeFlags, flag.String())
		}
	}

	return fmt.Sprintf("hodl.Mask(%s)", strings.Join(activeFlags, "|"))
}
