package lncfg

// Onlynet holds the configuration options for the daemon's network
// connection restrictions.
type Onlynet struct {
	Active bool `long:"active" description:"Activate onlynet restrictions"`
	Clear  bool `long:"clear" description:"Only allow outbound connections to clearnet addresses"`
	Onion  bool `long:"onion" description:"Only allow outbound connections to onion addresses"`
}
