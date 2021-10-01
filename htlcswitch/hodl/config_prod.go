//go:build !dev
// +build !dev

package hodl

// Config is an empty struct disabling command line hodl flags in production.
type Config struct{}

// Mask in production always returns MaskNone.
func (c *Config) Mask() Mask {
	return MaskNone
}
