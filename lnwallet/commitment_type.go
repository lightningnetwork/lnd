package lnwallet

import (
	"github.com/btcsuite/btcd/btcec"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwallet/commitmenttx"
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

	// The anchor chennel type MUST be tweakless.
	if chanType.HasAnchors() && !chanType.IsTweakless() {
		panic("invalid channel type combination")
	}

	return c
}

// DeriveCommitmentKeys generates a new commitment key set using the base
// points and commitment point for the this commitment type.
func (c CommitmentType) DeriveCommitmentKeys(commitPoint *btcec.PublicKey,
	isOurCommit bool, localChanCfg,
	remoteChanCfg *channeldb.ChannelConfig) *commitmenttx.KeyRing {

	// Return commitment keys with tweaklessCommit set according to channel
	// type.
	return commitmenttx.DeriveCommitmentKeys(
		commitPoint, isOurCommit, c.chanType.IsTweakless(),
		localChanCfg, remoteChanCfg,
	)
}

// CommitScriptToRemote creates the script that will pay to the non-owner of
// the commitment transaction, adding a delay to the script based on the
// commitment type.
func (c CommitmentType) CommitScriptToRemote(csvTimeout uint32,
	key *btcec.PublicKey) (*ScriptInfo, error) {

	// If this channel type has anchors, we derive the delayed to_remote
	// script.
	if c.chanType.HasAnchors() {
		script, err := input.CommitScriptToRemote(csvTimeout, key)
		if err != nil {
			return nil, err
		}

		p2wsh, err := input.WitnessScriptHash(script)
		if err != nil {
			return nil, err
		}

		return &ScriptInfo{
			PkScript:      p2wsh,
			WitnessScript: script,
		}, nil
	}

	// Otherwise the te_remote will be a simple p2wkh.
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

// CommitScriptAnchors return the scripts to use for the local and remote
// anchor. The returned values can be nil to indicate the ouputs shouldn't be
// added.
func (c CommitmentType) CommitScriptAnchors(localChanCfg,
	remoteChanCfg *channeldb.ChannelConfig) (*ScriptInfo, *ScriptInfo, error) {

	// If this channel type has no anchors we can return immediately.
	if c.chanType.HasAnchors() {
		return nil, nil, nil
	}

	// Then the anchor output spendable by the local node.
	localAnchorScript, err := input.CommitScriptAnchor(
		localChanCfg.MultiSigKey.PubKey,
	)
	if err != nil {
		return nil, nil, err
	}

	localAnchorScriptHash, err := input.WitnessScriptHash(localAnchorScript)
	if err != nil {
		return nil, nil, err
	}

	// And the anchor spemdable by the remote.
	remoteAnchorScript, err := input.CommitScriptAnchor(
		remoteChanCfg.MultiSigKey.PubKey,
	)
	if err != nil {
		return nil, nil, err
	}

	remoteAnchorScriptHash, err := input.WitnessScriptHash(
		remoteAnchorScript,
	)
	if err != nil {
		return nil, nil, err
	}

	return &ScriptInfo{
			PkScript:      localAnchorScript,
			WitnessScript: localAnchorScriptHash,
		},
		&ScriptInfo{
			PkScript:      remoteAnchorScript,
			WitnessScript: remoteAnchorScriptHash,
		}, nil
}

// CommitWeight returns the base commitment weight before adding HTLCs.
func (c CommitmentType) CommitWeight() int64 {
	// If this commitment has anchors, it will be slightly heavier.
	if c.chanType.HasAnchors() {
		return input.AnchorCommitWeight
	}

	return input.CommitWeight
}
