package blob

import (
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/txscript"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwire"
)

// JusticeKit is lé Blob of Justice. The JusticeKit contains information
// required to construct a justice transaction, that sweeps a remote party's
// revoked commitment transaction. It supports encryption and decryption using
// chacha20poly1305, allowing the client to encrypt the contents of the blob,
// and for a watchtower to later decrypt if action must be taken. The encoding
// format is versioned to allow future extensions.
type JusticeKit struct {
	// BlobType encodes a bitfield that inform the tower of various features
	// requested by the client when resolving a breach. Examples include
	// whether the justice transaction contains a reward for the tower, or
	// whether the channel is a legacy or anchor channel.
	//
	// NOTE: This value is not serialized in the encrypted payload. It is
	// stored separately and added to the JusticeKit after decryption.
	BlobType Type

	// SweepAddress is the witness program of the output where the client's
	// fund will be deposited. This value is included in the blobs, as
	// opposed to the session info, such that the sweep addresses can't be
	// correlated across sessions and/or towers.
	//
	// NOTE: This is chosen to be the length of a maximally sized witness
	// program.
	SweepAddress []byte

	// RevocationPubKey is the compressed pubkey that guards the revocation
	// clause of the remote party's to-local output.
	RevocationPubKey PubKey

	// LocalDelayPubKey is the compressed pubkey in the to-local script of
	// the remote party, which guards the path where the remote party
	// claims their commitment output.
	LocalDelayPubKey PubKey

	// CSVDelay is the relative timelock in the remote party's to-local
	// output, which the remote party must wait out before sweeping their
	// commitment output.
	CSVDelay uint32

	// CommitToLocalSig is a signature under RevocationPubKey using
	// SIGHASH_ALL.
	CommitToLocalSig lnwire.Sig

	// CommitToRemotePubKey is the public key in the to-remote output of the
	// revoked
	// commitment transaction.
	//
	// NOTE: This value is only used if it contains a valid compressed
	// public key.
	CommitToRemotePubKey PubKey

	// CommitToRemoteSig is a signature under CommitToRemotePubKey using
	// SIGHASH_ALL.
	//
	// NOTE: This value is only used if CommitToRemotePubKey contains a
	// valid compressed public key.
	CommitToRemoteSig lnwire.Sig
}

// CommitToLocalWitnessScript returns the serialized witness script for the
// commitment to-local output.
func (b *JusticeKit) CommitToLocalWitnessScript() ([]byte, error) {
	revocationPubKey, err := btcec.ParsePubKey(
		b.RevocationPubKey[:],
	)
	if err != nil {
		return nil, err
	}

	localDelayedPubKey, err := btcec.ParsePubKey(
		b.LocalDelayPubKey[:],
	)
	if err != nil {
		return nil, err
	}

	return input.CommitScriptToSelf(
		b.CSVDelay, localDelayedPubKey, revocationPubKey,
	)
}

// CommitToLocalRevokeWitnessStack constructs a witness stack spending the
// revocation clause of the commitment to-local output.
//
//	<revocation-sig> 1
func (b *JusticeKit) CommitToLocalRevokeWitnessStack() ([][]byte, error) {
	toLocalSig, err := b.CommitToLocalSig.ToSignature()
	if err != nil {
		return nil, err
	}

	witnessStack := make([][]byte, 2)
	witnessStack[0] = append(toLocalSig.Serialize(),
		byte(txscript.SigHashAll))
	witnessStack[1] = []byte{1}

	return witnessStack, nil
}

// HasCommitToRemoteOutput returns true if the blob contains a to-remote p2wkh
// pubkey.
func (b *JusticeKit) HasCommitToRemoteOutput() bool {
	return btcec.IsCompressedPubKey(b.CommitToRemotePubKey[:])
}

// CommitToRemoteWitnessScript returns the witness script for the commitment
// to-remote output given the blob type. The script returned will either be for
// a p2wpkh to-remote output or an p2wsh anchor to-remote output which includes
// a CSV delay.
func (b *JusticeKit) CommitToRemoteWitnessScript() ([]byte, error) {
	if !btcec.IsCompressedPubKey(b.CommitToRemotePubKey[:]) {
		return nil, ErrNoCommitToRemoteOutput
	}

	// If this is a blob for an anchor channel, we'll return the p2wsh
	// output containing a CSV delay of 1.
	if b.BlobType.IsAnchorChannel() {
		pk, err := btcec.ParsePubKey(b.CommitToRemotePubKey[:])
		if err != nil {
			return nil, err
		}

		return input.CommitScriptToRemoteConfirmed(pk)
	}

	return b.CommitToRemotePubKey[:], nil
}

// CommitToRemoteWitnessStack returns a witness stack spending the commitment
// to-remote output, which consists of a single signature satisfying either the
// legacy or anchor witness scripts.
//
//	<to-remote-sig>
func (b *JusticeKit) CommitToRemoteWitnessStack() ([][]byte, error) {
	toRemoteSig, err := b.CommitToRemoteSig.ToSignature()
	if err != nil {
		return nil, err
	}

	witnessStack := make([][]byte, 1)
	witnessStack[0] = append(toRemoteSig.Serialize(),
		byte(txscript.SigHashAll))

	return witnessStack, nil
}
