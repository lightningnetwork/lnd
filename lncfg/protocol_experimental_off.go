// +build !dev

package lncfg

// ExperimentalProtocol is a sub-config that houses any experimental protocol
// features that also require a build-tag to activate.
type ExperimentalProtocol struct {
}

// AnchorCommitments returns true if support for the anchor commitment type
// should be signaled.
func (l *ExperimentalProtocol) AnchorCommitments() bool {
	return false
}
