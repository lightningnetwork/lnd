// +build !experimental

package watchtower

// Conf specifies the watchtower options that be configured from the command
// line or configuration file. In non-experimental builds, we disallow such
// configuration.
type Conf struct{}

// Apply returns an error signaling that the Conf could not be applied in
// non-experimental builds.
func (c *Conf) Apply(cfg *Config) (*Config, error) {
	return nil, ErrNonExperimentalConf
}
