package commitmenttx

import (
	"github.com/btcsuite/btcd/btcec"
	"github.com/lightningnetwork/lnd/channeldb"
)

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
