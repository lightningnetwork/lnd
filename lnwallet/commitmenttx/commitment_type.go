package commitmenttx

import (
	"github.com/btcsuite/btcd/btcec"
	"github.com/lightningnetwork/lnd/channeldb"
)

// CommitmentType is an interface that abstracts the various ways of
// constructing commitment transactions, and can be used to derive keys,
// scripts and outputs depending on the channel's commitment type.
type CommitmentType interface {
	// DeriveCommitmentKeys generates a new commitment key set using the
	// base points and commitment point. The keys are derived differently
	// depending whether the commitment transaction is ours or the remote
	// peer's.
	DeriveCommitmentKeys(commitPoint *btcec.PublicKey, isOurCommit bool,
		localChanCfg, remoteChanCfg *channeldb.ChannelConfig) *KeyRing
}

// CommitmentFromChanType returns the channel's CommitmentType based on the
// ChannelType.
func CommitmentFromChanType(t channeldb.ChannelType) CommitmentType {
	switch t {
	case channeldb.SingleFunderBit:
		return &Original{}

	case channeldb.DualFunderBit:
		return &Original{}

	case channeldb.SingleFunderTweaklessBit:
		return &Tweakless{}
	}

	panic("unknown chan type")
}

// Original is the basic channel type.
type Original struct{}

// A compile-time assertion to ensure that Original meets the CommitmentType
// interface.
var _ CommitmentType = (*Original)(nil)

// DeriveCommitmentKeys generates a new commitment key set using the base
// points and commitment point for the Original commitment type.
func (s *Original) DeriveCommitmentKeys(commitPoint *btcec.PublicKey,
	isOurCommit bool, localChanCfg,
	remoteChanCfg *channeldb.ChannelConfig) *KeyRing {

	// Return commitment keys with tweaklessCommit=false.
	return deriveCommitmentKeys(
		commitPoint, isOurCommit, false, localChanCfg, remoteChanCfg,
	)
}

// Tweakless is a commitment type equivalent to Original, except that the
// remote commitment key won't be tweaked.
type Tweakless struct {
	Original
}

// A compile-time assertion to ensure that Tweakless meets the CommitmentType
// interface.
var _ CommitmentType = (*Tweakless)(nil)

// DeriveCommitmentKeys generates a new commitment key set using the base
// points and commitment point for the Tweakless commitment type.  The returned
// keys differs from Original in that the remote key won't be tweaked.
func (s *Tweakless) DeriveCommitmentKeys(commitPoint *btcec.PublicKey,
	isOurCommit bool, localChanCfg,
	remoteChanCfg *channeldb.ChannelConfig) *KeyRing {

	// Return commitment keys with tweaklessCommit=true.
	return deriveCommitmentKeys(
		commitPoint, isOurCommit, true, localChanCfg, remoteChanCfg,
	)
}
