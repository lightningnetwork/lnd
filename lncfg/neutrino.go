package lncfg

import "time"

// Neutrino holds the configuration options for the daemon's connection to
// neutrino.
type Neutrino struct {
	AddPeers           []string      `short:"a" long:"addpeer" description:"Add a peer to connect with at startup"`
	ConnectPeers       []string      `long:"connect" description:"Connect only to the specified peers at startup"`
	MaxPeers           int           `long:"maxpeers" description:"Max number of inbound and outbound peers"`
	BanDuration        time.Duration `long:"banduration" description:"How long to ban misbehaving peers.  Valid time units are {s, m, h}.  Minimum 1 second"`
	BanThreshold       uint32        `long:"banthreshold" description:"Maximum allowed ban score before disconnecting and banning misbehaving peers."`
	FeeURL             string        `long:"feeurl" description:"Optional URL for fee estimation. If a URL is not specified, static fees will be used for estimation."`
	AssertFilterHeader string        `long:"assertfilterheader" description:"Optional filter header in height:hash format to assert the state of neutrino's filter header chain on startup. If the assertion does not hold, then the filter header chain will be re-synced from the genesis block."`
}
