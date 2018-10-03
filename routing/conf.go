// +build !experimental

package routing

// Conf provides the command line routing configuration. There are no fields in
// the production build so that this section is hidden by default.
type Conf struct{}

// UseAssumeChannelValid always returns false when not in experimental builds.
func (c *Conf) UseAssumeChannelValid() bool {
	return false
}
