// +build experimental

package routing

// Conf exposes the experimental command line routing configurations.
type Conf struct {
	AssumeChannelValid bool `long:"assumechanvalid" description:"Skip checking channel spentness during graph validation. (default: false)"`
}

// UseAssumeChannelValid returns true if the router should skip checking for
// spentness when processing channel updates and announcements.
func (c *Conf) UseAssumeChannelValid() bool {
	return c.AssumeChannelValid
}
