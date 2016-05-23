package lnwire

import (
	"bytes"
	"fmt"
	"io"

	"github.com/roasbeef/btcd/btcec"
)

// SingleFundingResponse is the message Bob sends to Alice after she initiates
// the single funder channel workflow via a SingleFundingRequest message. Once
// Alice receives Bob's reponse, then she has all the items neccessary to
// construct the funding transaction, and both commitment transactions.
type SingleFundingResponse struct {
	// ChannelID serves to uniquely identify the future channel created by
	// the initiated single funder workflow.
	ChannelID uint64

	// RevocationHash is the initial revocation hash to be used for the
	// responders's commitment transaction to derive their revocation public
	// key as: P + G*revocationHash, where P is the initiator's channel
	// public key.
	RevocationHash [20]byte

	// ChannelDerivationPoint is an secp256k1 point which will be used to
	// derive the public key the responder will use for the half of the
	// 2-of-2 multi-sig. Using the channel derivation point (CDP), and the
	// initiators identity public key (A), the channel public key is
	// computed as: C = A + CDP. In order to be valid all CDP's MUST have
	// an odd y-coordinate.
	ChannelDerivationPoint *btcec.PublicKey

	// DeliveryPkScript defines the public key script that the initiator
	// would like to use to receive their balance in the case of a
	// cooperative close. Only the following script templates are
	// supported: P2PKH, P2WKH, P2SH, and P2WSH.
	DeliveryPkScript PkScript
}

// Decode deserializes the serialized SingleFundingResponse stored in the passed
// io.Reader into the target SingleFundingResponse using the deserialization
// rules defined by the passed protocol version.
//
// This is part of the lnwire.Message interface.
func (c *SingleFundingResponse) Decode(r io.Reader, pver uint32) error {
	// ChannelID (8)
	// Revocation Hash (20)
	// Pubkey (32)
	// DeliveryPkScript (final delivery)
	err := readElements(r,
		&c.ChannelID,
		&c.RevocationHash,
		&c.ChannelDerivationPoint,
		&c.DeliveryPkScript)
	if err != nil {
		return err
	}

	return nil
}

// NewSingleFundingResponse creates, and returns a new empty
// SingleFundingResponse.
func NewSingleFundingResponse() *SingleFundingResponse {
	return &SingleFundingResponse{}
}

// Encode serializes the target SingleFundingResponse into the passed io.Writer
// implementation. Serialization will observe the rules defined by the passed
// protocol version.
//
// This is part of the lnwire.Message interface.
func (c *SingleFundingResponse) Encode(w io.Writer, pver uint32) error {
	// ChannelID (8)
	// Revocation Hash (20)
	// Channel Derivation Point (32)
	// DeliveryPkScript (final delivery)
	err := writeElements(w,
		c.ChannelID,
		c.RevocationHash,
		c.ChannelDerivationPoint,
		c.DeliveryPkScript)
	if err != nil {
		return err
	}

	return nil
}

// Command returns the uint32 code which uniquely identifies this message as a
// SingleFundingRequest on the wire.
//
// This is part of the lnwire.Message interface.
func (c *SingleFundingResponse) Command() uint32 {
	return CmdSingleFundingResponse
}

// MaxPayloadLength returns the maximum allowed payload length for a
// SingleFundingResponse. This is calculated by summing the max length of all
// the fields within a SingleFundingResponse. To enforce a maximum
// DeliveryPkScript size, the size of a P2PKH public key script is used.
// Therefore, the final breakdown is: 8 + 20 + 32 + 25 = 85
//
// This is part of the lnwire.Message interface.
func (c *SingleFundingResponse) MaxPayloadLength(uint32) uint32 {
	return 85
}

// Validate examines each populated field within the SingleFundingResponse for
// field sanity. For example, all fields MUST NOT be negative, and all pkScripts
// must belong to the allowed set of public key scripts.
//
// This is part of the lnwire.Message interface.
func (c *SingleFundingResponse) Validate() error {
	var zeroHash [20]byte
	if bytes.Equal(zeroHash[:], c.RevocationHash[:]) {
		return fmt.Errorf("revocation has must be non-zero")
	}

	// The channel derivation point must be non-nil, and have an odd
	// y-coordinate.
	if c.ChannelDerivationPoint == nil {
		return fmt.Errorf("The channel derivation point must be non-nil")
	}
	if c.ChannelDerivationPoint.Y.Bit(0) != 1 {
		return fmt.Errorf("The channel derivation point must have an odd " +
			"y-coordinate")
	}

	// The delivery pkScript must be amongst the supported script
	// templates.
	if !isValidPkScript(c.DeliveryPkScript) {
		return fmt.Errorf("Valid delivery public key scripts MUST be: " +
			"P2PKH, P2WKH, P2SH, or P2WSH.")
	}

	// We're good!
	return nil
}

// String returns the string representation of the SingleFundingResponse.
//
// This is part of the lnwire.Message interface.
func (c *SingleFundingResponse) String() string {
	var serializedPubkey []byte
	if &c.ChannelDerivationPoint != nil && c.ChannelDerivationPoint.X != nil {
		serializedPubkey = c.ChannelDerivationPoint.SerializeCompressed()
	}

	return fmt.Sprintf("\n--- Begin SingleFundingResponse ---\n") +
		fmt.Sprintf("ChannelID:\t\t\t%d\n", c.ChannelID) +
		fmt.Sprintf("RevocationHash\t\t\t%x\n", c.RevocationHash) +
		fmt.Sprintf("ChannelDerivationPoint\t\t\t\t%x\n", serializedPubkey) +
		fmt.Sprintf("DeliveryPkScript\t\t%x\n", c.DeliveryPkScript) +
		fmt.Sprintf("--- End SingleFundingResponse ---\n")
}
