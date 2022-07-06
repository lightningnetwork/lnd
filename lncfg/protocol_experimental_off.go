//go:build !dev
// +build !dev

package lncfg

// ExperimentalProtocol is a sub-config that houses any experimental protocol
// features that also require a build-tag to activate.
type ExperimentalProtocol struct {
}

// CustomMessageOverrides returns the set of protocol messages that we override
// to allow custom handling.
func (p ExperimentalProtocol) CustomMessageOverrides() []uint16 {
	return nil
}
