package commitmenttx

import (
	"github.com/btcsuite/btcd/btcec"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/input"
)

// ScriptIinfo holds a reedeem script and hash.
type ScriptInfo struct {
	// PkScript is the outputs' PkScript.
	PkScript []byte

	// WitnessScript is the full script required to properly redeem the
	// output with PkSript. This field will only be populated if a PkScript
	// is p2wsh or p2sh.
	WitnessScript []byte
}

// CommitmentType is a type that wraps the type of channel we are dealine with,
// and abstracts the various ways of constructing commitment transactions. It
// can be used to derive keys, scripts and outputs depending on the channel's
// commitment type.
type CommitmentType struct {
	chanType channeldb.ChannelType
}

// NewCommitmentType creates a new CommitmentType based on the ChannelType.
func NewCommitmentType(chanType channeldb.ChannelType) CommitmentType {
	c := CommitmentType{
		chanType: chanType,
	}

	// TODO: validate bit combinations.
	return c
}

// DeriveCommitmentKeys generates a new commitment key set using the base
// points and commitment point for the this commitment type.
func (c CommitmentType) DeriveCommitmentKeys(commitPoint *btcec.PublicKey,
	isOurCommit bool, localChanCfg,
	remoteChanCfg *channeldb.ChannelConfig) *KeyRing {

	// Return commitment keys with tweaklessCommit set according to channel
	// type.
	return deriveCommitmentKeys(
		commitPoint, isOurCommit, c.chanType.IsTweakless(),
		localChanCfg, remoteChanCfg,
	)
}

// CommitScriptToRemote creates the script that will pay to the non-owner of
// the commitment transaction, adding a delay to the script based on the
// commitment type.
func (c CommitmentType) CommitScriptToRemote(csvTimeout uint32,
	key *btcec.PublicKey) (*ScriptInfo, error) {

	p2wkh, err := input.CommitScriptUnencumbered(key)
	if err != nil {
		return nil, err
	}

	// Since this is a regular P2WKH, the WitnessScipt doesn't have to be
	// set.
	return &ScriptInfo{
		PkScript: p2wkh,
	}, nil
}
