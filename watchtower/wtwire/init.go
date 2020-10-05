package wtwire

import (
	"fmt"
	"io"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/feature"
	"github.com/lightningnetwork/lnd/lnwire"
)

// Init is the first message sent over the watchtower wire protocol, and
// specifies connection features bits and level of requiredness maintained by
// the sending node. The Init message also sends the chain hash identifying the
// network that the sender is on.
type Init struct {
	// ConnFeatures are the feature bits being advertised for the duration
	// of a single connection with a peer.
	ConnFeatures *lnwire.RawFeatureVector

	// ChainHash is the genesis hash of the chain that the advertiser claims
	// to be on.
	ChainHash chainhash.Hash
}

// NewInitMessage generates a new Init message from a raw connection feature
// vector and chain hash.
func NewInitMessage(connFeatures *lnwire.RawFeatureVector,
	chainHash chainhash.Hash) *Init {

	return &Init{
		ConnFeatures: connFeatures,
		ChainHash:    chainHash,
	}
}

// Encode serializes the target Init into the passed io.Writer observing the
// protocol version specified.
//
// This is part of the wtwire.Message interface.
func (msg *Init) Encode(w io.Writer, pver uint32) error {
	return WriteElements(w,
		msg.ConnFeatures,
		msg.ChainHash,
	)
}

// Decode deserializes a serialized Init message stored in the passed io.Reader
// observing the specified protocol version.
//
// This is part of the wtwire.Message interface.
func (msg *Init) Decode(r io.Reader, pver uint32) error {
	return ReadElements(r,
		&msg.ConnFeatures,
		&msg.ChainHash,
	)
}

// MsgType returns the integer uniquely identifying this message type on the
// wire.
//
// This is part of the wtwire.Message interface.
func (msg *Init) MsgType() MessageType {
	return MsgInit
}

// MaxPayloadLength returns the maximum allowed payload size for an Init
// complete message observing the specified protocol version.
//
// This is part of the wtwire.Message interface.
func (msg *Init) MaxPayloadLength(uint32) uint32 {
	return MaxMessagePayload
}

// A compile-time constraint to ensure Init implements the Message interface.
var _ Message = (*Init)(nil)

// CheckRemoteInit performs basic validation of the remote party's Init message.
// This method checks that the remote Init's chain hash matches our advertised
// chain hash and that the remote Init does not contain any required feature
// bits that we don't understand.
func (msg *Init) CheckRemoteInit(remoteInit *Init,
	featureNames map[lnwire.FeatureBit]string) error {

	// Check that the remote peer is on the same chain.
	if msg.ChainHash != remoteInit.ChainHash {
		return NewErrUnknownChainHash(remoteInit.ChainHash)
	}

	remoteConnFeatures := lnwire.NewFeatureVector(
		remoteInit.ConnFeatures, featureNames,
	)

	// Check that the remote peer doesn't have any required connection
	// feature bits that we ourselves are unaware of.
	return feature.ValidateRequired(remoteConnFeatures)
}

// ErrUnknownChainHash signals that the remote Init has a different chain hash
// from the one we advertised.
type ErrUnknownChainHash struct {
	hash chainhash.Hash
}

// NewErrUnknownChainHash creates an ErrUnknownChainHash using the remote Init's
// chain hash.
func NewErrUnknownChainHash(hash chainhash.Hash) *ErrUnknownChainHash {
	return &ErrUnknownChainHash{hash}
}

// Error returns a human-readable error displaying the unknown chain hash.
func (e *ErrUnknownChainHash) Error() string {
	return fmt.Sprintf("remote init has unknown chain hash: %s", e.hash)
}
