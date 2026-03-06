package onionmessage

import (
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/record"
)

// OnionMessageUpdate is onion message update dispatched to any potential
// subscriber.
type OnionMessageUpdate struct {
	// Peer is the peer pubkey
	Peer [33]byte

	// PathKey is the route blinding ephemeral pubkey to be used for
	// the onion message.
	PathKey [33]byte

	// OnionBlob is the raw serialized mix header used to relay messages in
	// a privacy-preserving manner. This blob should be handled in the same
	// manner as onions used to route HTLCs, with the exception that it uses
	// blinded routes by default.
	OnionBlob []byte

	// CustomRecords contains any custom TLV records included in the
	// payload.
	CustomRecords record.CustomSet

	// ReplyPath contains the reply path information for the onion message.
	ReplyPath *sphinx.BlindedPath

	// EncryptedRecipientData contains the encrypted recipient data for the
	// onion message, created by the creator of the blinded route. This is
	// the receiver for the last leg of the route, and the sender for the
	// first leg up to the introduction point.
	EncryptedRecipientData []byte
}
