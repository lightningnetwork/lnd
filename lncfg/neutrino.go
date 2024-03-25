package lncfg

import (
	"time"

	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lnd/lnrpc"
)

// SideloadOpt holds the configuration options for sideloading headers in
// neutrino.
//
//nolint:lll
type SideloadOpt struct {
	Enable        bool   `long:"enable" description:"Indicates sideloading is enabled"`
	SourceType    string `long:"sourceType" description:"Indicates the encoding format of the sideload source" choice:"binary"`
	SourcePath    string `long:"sourcePath" description:"Indicates the path to the sideload source"`
	SkipVerify    bool   `long:"skipVerify" description:"Indicates if to verify headers while sideleoading"`
	SideloadRange uint32 `long:"range" description:"Indicates how much headers should be read from the source at a time"`
}

// Neutrino holds the configuration options for the daemon's connection to
// neutrino.
//
//nolint:lll
type Neutrino struct {
	AddPeers           []string      `short:"a" long:"addpeer" description:"Add a peer to connect with at startup"`
	ConnectPeers       []string      `long:"connect" description:"Connect only to the specified peers at startup"`
	MaxPeers           int           `long:"maxpeers" description:"Max number of inbound and outbound peers"`
	BanDuration        time.Duration `long:"banduration" description:"How long to ban misbehaving peers.  Valid time units are {s, m, h}.  Minimum 1 second"`
	BanThreshold       uint32        `long:"banthreshold" description:"Maximum allowed ban score before disconnecting and banning misbehaving peers."`
	FeeURL             string        `long:"feeurl" description:"DEPRECATED: Use top level 'feeurl' option. Optional URL for fee estimation. If a URL is not specified, static fees will be used for estimation." hidden:"true"`
	AssertFilterHeader string        `long:"assertfilterheader" description:"Optional filter header in height:hash format to assert the state of neutrino's filter header chain on startup. If the assertion does not hold, then the filter header chain will be re-synced from the genesis block."`
	UserAgentName      string        `long:"useragentname" description:"Used to help identify ourselves to other bitcoin peers"`
	UserAgentVersion   string        `long:"useragentversion" description:"Used to help identify ourselves to other bitcoin peers"`
	ValidateChannels   bool          `long:"validatechannels" description:"Validate every channel in the graph during sync by downloading the containing block. This is the inverse of routing.assumechanvalid, meaning that for Neutrino the validation is turned off by default for massively increased graph sync performance. This speedup comes at the risk of using an unvalidated view of the network for routing. Overwrites the value of routing.assumechanvalid if Neutrino is used. (default: false)"`
	BroadcastTimeout   time.Duration `long:"broadcasttimeout" description:"The amount of time to wait before giving up on a transaction broadcast attempt."`
	PersistFilters     bool          `long:"persistfilters" description:"Whether compact filters fetched from the P2P network should be persisted to disk."`
	BlkHdrSideloadOpt  *SideloadOpt  `group:"sideload" namespace:"sideload"`
}

// Validate checks a Neutrino instance's config for correctness, returning nil
// if the instance is uninitialized or its sideload options are valid. Errors
// from sideload option validation are returned.
func (n *Neutrino) Validate() error {
	if n == nil {
		// Consider nil instance as uninitialized; no validation needed.
		return nil
	}

	// Validate sideload options.
	err := n.BlkHdrSideloadOpt.Validate()
	if err != nil {
		return err // Return validation errors.
	}

	return nil
}

// Validate checks SideloadOpt for required source type and path, returning
// errors if they're missing or invalid.
func (s *SideloadOpt) Validate() error {
	if !s.Enable {
		return nil
	}

	// Require source type for sideloading.
	if s.SourceType == "" {
		return errors.New("source type required for sideloading " +
			"headers.")
	}

	// Require source path for sideloading.
	if s.SourcePath == "" {
		return errors.New("source path required for sideloading " +
			"headers.")
	}

	// Check source path validity.
	if !validatePath(s.SourcePath) {
		return errors.New("invalid source path")
	}

	return nil
}

// validatePath verifies if a path is a valid URL or an existing file, returning
// true if valid.
func validatePath(path string) bool {
	// Check path validity as URL.
	if lnrpc.IsValidURL(path) {
		return true
	}

	// Clean/expand the path; check file existence.
	path = CleanAndExpandPath(path)

	return lnrpc.FileExists(path)
}
