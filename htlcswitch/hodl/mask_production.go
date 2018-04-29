// +build !debug

package hodl

// DebugBuild signals that this is a production build.
const DebugBuild = false

// MaskFromFlags in production always returns MaskNone.
func MaskFromFlags(_ ...Flag) Mask {
	return MaskNone
}

// Active in production always returns false for all Flags.
func (m Mask) Active(_ Flag) bool {
	return false
}

// String returns the human-readable identifier for MaskNone.
func (m Mask) String() string {
	return "hodl.Mask(NONE)"
}
