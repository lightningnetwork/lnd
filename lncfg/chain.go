package lncfg

import (
	"fmt"

	"github.com/lightningnetwork/lnd/lnwire"
)

// Chain holds the configuration options for the daemon's chain settings.
//
//nolint:ll
type Chain struct {
	Active   bool   `long:"active" description:"DEPRECATED: If the chain should be active or not. This field is now ignored since only the Bitcoin chain is supported" hidden:"true"`
	ChainDir string `long:"chaindir" description:"The directory to store the chain's data within."`

	Node string `long:"node" description:"The blockchain interface to use." choice:"btcd" choice:"bitcoind" choice:"neutrino" choice:"nochainbackend"`

	MainNet         bool     `long:"mainnet" description:"Use the main network"`
	TestNet3        bool     `long:"testnet" description:"Use the test network"`
	TestNet4        bool     `long:"testnet4" description:"Use the testnet4 test network"`
	SimNet          bool     `long:"simnet" description:"Use the simulation test network"`
	RegTest         bool     `long:"regtest" description:"Use the regression test network"`
	SigNet          bool     `long:"signet" description:"Use the signet test network"`
	SigNetChallenge string   `long:"signetchallenge" description:"Connect to a custom signet network defined by this challenge instead of using the global default signet test network -- Can be specified multiple times"`
	SigNetSeedNode  []string `long:"signetseednode" description:"Specify a seed node for the signet network instead of using the global default signet network seed nodes"`

	DefaultNumChanConfs int                 `long:"defaultchanconfs" description:"The default number of confirmations a channel must have before it's considered open. If this is not set, we will scale the value according to the channel size."`
	DefaultRemoteDelay  int                 `long:"defaultremotedelay" description:"The default number of blocks we will require our channel counterparty to wait before accessing its funds in case of unilateral close. If this is not set, we will scale the value according to the channel size."`
	MaxLocalDelay       uint16              `long:"maxlocaldelay" description:"The maximum blocks we will allow our funds to be timelocked before accessing its funds in case of unilateral close. If a peer proposes a value greater than this, we will reject the channel."`
	MinHTLCIn           lnwire.MilliSatoshi `long:"minhtlc" description:"The smallest HTLC we are willing to accept on our channels, in millisatoshi"`
	MinHTLCOut          lnwire.MilliSatoshi `long:"minhtlcout" description:"The smallest HTLC we are willing to send out on our channels, in millisatoshi"`
	BaseFee             lnwire.MilliSatoshi `long:"basefee" description:"The base fee in millisatoshi we will charge for forwarding payments on our channels"`
	FeeRate             lnwire.MilliSatoshi `long:"feerate" description:"The fee rate used when forwarding payments on our channels. The total fee charged is basefee + (amount * feerate / 1000000), where amount is the forwarded amount."`
	TimeLockDelta       uint32              `long:"timelockdelta" description:"The CLTV delta we will subtract from a forwarded HTLC's timelock value"`
	DNSSeeds            []string            `long:"dnsseed" description:"The seed DNS server(s) to use for initial peer discovery. Must be specified as a '<primary_dns>[,<soa_primary_dns>]' tuple where the SOA address is needed for DNS resolution through Tor but is optional for clearnet users. Multiple tuples can be specified, will overwrite the default seed servers."`
}

// Validate performs validation on our chain config.
func (c *Chain) Validate(minTimeLockDelta uint32, minDelay uint16) error {
	if c.TimeLockDelta < minTimeLockDelta {
		return fmt.Errorf("timelockdelta must be at least %v",
			minTimeLockDelta)
	}

	// Check that our max local delay isn't set below some reasonable
	// minimum value. We do this to prevent setting an unreasonably low
	// delay, which would mean that the node would accept no channels.
	if c.MaxLocalDelay < minDelay {
		return fmt.Errorf("MaxLocalDelay must be at least: %v",
			minDelay)
	}

	return nil
}

// IsLocalNetwork returns true if the chain is a local network, such as
// simnet or regtest.
func (c *Chain) IsLocalNetwork() bool {
	return c.SimNet || c.RegTest
}
