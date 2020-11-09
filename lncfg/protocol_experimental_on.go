// +build dev

package lncfg

// ExperimentalProtocol is a sub-config that houses any experimental protocol
// features that also require a build-tag to activate.
type ExperimentalProtocol struct {
	// Anchors should be set if we want to support opening or accepting
	// channels having the anchor commitment type.
	Anchors bool `long:"anchors" description:"EXPERIMENTAL: enable experimental support for anchor commitments, won't work with watchtowers"`
}

// AnchorCommitments returns true if support for the anchor commitment type
// should be signaled.
func (l *ExperimentalProtocol) AnchorCommitments() bool {
	return l.Anchors
}
