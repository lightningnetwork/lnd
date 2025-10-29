package lncfg

// Tor holds the configuration options for the daemon's connection to tor.
//
//nolint:ll
type Tor struct {
	Active                      bool     `long:"active" description:"Allow outbound and inbound connections to be routed through Tor"`
	SOCKS                       string   `long:"socks" description:"The host:port that Tor's exposed SOCKS5 proxy is listening on"`
	DNS                         string   `long:"dns" description:"The DNS server as host:port that Tor will use for SRV queries - NOTE must have TCP resolution enabled"`
	StreamIsolation             bool     `long:"streamisolation" description:"Enable Tor stream isolation by randomizing user credentials for each connection."`
	SkipProxyForClearNetTargets bool     `long:"skip-proxy-for-clearnet-targets" description:"Allow the node to establish direct connections to services not running behind Tor."`
	NoProxyTargets              []string `long:"no-proxy-targets" description:"Specify a target that should bypass the configured proxy. Each value is either an IP address, a CIDR range, a zone (*.example.com) or a hostname (localhost). A best effort is made to parse the string and errors are ignored. Can be specified multiple times. (default: localhost,127.0.0.0/8,::1/128)"`
	Control                     string   `long:"control" description:"The host:port that Tor is listening on for Tor control connections"`
	TargetIPAddress             string   `long:"targetipaddress" description:"IP address that Tor should use as the target of the hidden service"`
	Password                    string   `long:"password" description:"The password used to arrive at the HashedControlPassword for the control port. If provided, the HASHEDPASSWORD authentication method will be used instead of the SAFECOOKIE one."`
	V2                          bool     `long:"v2" description:"DEPRECATED: Tor v2 onion services are obsolete and support will be removed in v0.21.0. Use v3 instead." hidden:"true"`
	V3                          bool     `long:"v3" description:"Automatically set up a v3 onion service to listen for inbound connections"`
	PrivateKeyPath              string   `long:"privatekeypath" description:"The path to the private key of the onion service being created"`
	EncryptKey                  bool     `long:"encryptkey" description:"Encrypts the Tor private key file on disk"`
	WatchtowerKeyPath           string   `long:"watchtowerkeypath" description:"The path to the private key of the watchtower onion service being created"`
}
