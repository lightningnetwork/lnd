package lncfg

import "time"

// Neutrino holds the configuration options for the daemon's connection to
// neutrino.
//
//nolint:ll
type Neutrino struct {
	AddPeers           []string      `short:"a" long:"addpeer" description:"Add a peer to connect with at startup"`
	ConnectPeers       []string      `long:"connect" description:"Connect only to the specified peers at startup"`
	MaxPeers           int           `long:"maxpeers" description:"Max number of inbound and outbound peers"`
	BanDuration        time.Duration `long:"banduration" description:"How long to ban misbehaving peers.  Valid time units are {s, m, h}.  Minimum 1 second"`
	BanThreshold       uint32        `long:"banthreshold" description:"Maximum allowed ban score before disconnecting and banning misbehaving peers."`
	AssertFilterHeader string        `long:"assertfilterheader" description:"Optional filter header in height:hash format to assert the state of neutrino's filter header chain on startup. If the assertion does not hold, then the filter header chain will be re-synced from the genesis block."`
	UserAgentName      string        `long:"useragentname" description:"Used to help identify ourselves to other bitcoin peers"`
	UserAgentVersion   string        `long:"useragentversion" description:"Used to help identify ourselves to other bitcoin peers"`
	ValidateChannels   bool          `long:"validatechannels" description:"Validate every channel in the graph during sync by downloading the containing block. This is the inverse of routing.assumechanvalid, meaning that for Neutrino the validation is turned off by default for massively increased graph sync performance. This speedup comes at the risk of using an unvalidated view of the network for routing. Overwrites the value of routing.assumechanvalid if Neutrino is used. (default: false)"`
	BroadcastTimeout   time.Duration `long:"broadcasttimeout" description:"The amount of time to wait before giving up on a transaction broadcast attempt."`
	PersistFilters     bool          `long:"persistfilters" description:"Whether compact filters fetched from the P2P network should be persisted to disk."`
}
