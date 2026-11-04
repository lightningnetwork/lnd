//go:build !dev
// +build !dev

package lncfg

// ExperimentalProtocol is a sub-config that houses any experimental protocol
// features that also require a build-tag to activate.
type ExperimentalProtocol struct {
}

// Bolt12OffersEnabled returns false, because a release build has no BOLT 12
// offers.
func (p ExperimentalProtocol) Bolt12OffersEnabled() bool {
	return false
}
