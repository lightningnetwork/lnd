package lnwire

import (
	"fmt"
	"io"

	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcutil"
)

// SingleFundingResponse is the message Bob sends to Alice after she initiates
// the single funder channel workflow via a SingleFundingRequest message. Once
// Alice receives Bob's reponse, then she has all the items neccessary to
// construct the funding transaction, and both commitment transactions.
type SingleFundingResponse struct {
	// ChannelID serves to uniquely identify the future channel created by
	// the initiated single funder workflow.
	ChannelID uint64

	// ChannelDerivationPoint is an secp256k1 point which will be used to
	// derive the public key the responder will use for the half of the
	// 2-of-2 multi-sig. Using the channel derivation point (CDP), and the
	// responder's identity public key (A), the channel public key is
	// computed as: C = A + CDP. In order to be valid all CDP's MUST have
	// an odd y-coordinate.
	ChannelDerivationPoint *btcec.PublicKey

	// CommitmentKey is key the responder to the funding workflow wishes to
	// use within their versino of the commitment transaction for any
	// delayed (CSV) or immediate outputs to them.
	CommitmentKey *btcec.PublicKey

	// RevocationKey is the initial key to be used for the revocation
	// clause within the self-output of the responder's commitment
	// transaction. Once an initial new state is created, the responder
	// will send a preimage which will allow the initiator to sweep the
	// responder's funds if the violate the contract.
	RevocationKey *btcec.PublicKey

	// CsvDelay is the number of blocks to use for the relative time lock
	// in the pay-to-self output of both commitment transactions.
	CsvDelay uint32

	// DeliveryPkScript defines the public key script that the initiator
	// would like to use to receive their balance in the case of a
	// cooperative close. Only the following script templates are
	// supported: P2PKH, P2WKH, P2SH, and P2WSH.
	DeliveryPkScript PkScript

	// DustLimit is the threshold below which no HTLC output should be
	// generated for remote commitment transaction; ie. HTLCs below
	// this amount are not enforceable onchain for their point of view.
	DustLimit btcutil.Amount

	// ConfirmationDepth is the number of confirmations that the initiator
	// of a funding workflow is requesting be required before the channel
	// is considered fully open.
	ConfirmationDepth uint32
}

// NewSingleFundingResponse creates, and returns a new empty
// SingleFundingResponse.
func NewSingleFundingResponse(chanID uint64, rk, ck, cdp *btcec.PublicKey,
	delay uint32, deliveryScript PkScript,
	dustLimit btcutil.Amount, confDepth uint32) *SingleFundingResponse {

	return &SingleFundingResponse{
		ChannelID:              chanID,
		ChannelDerivationPoint: cdp,
		CommitmentKey:          ck,
		RevocationKey:          rk,
		CsvDelay:               delay,
		DeliveryPkScript:       deliveryScript,
		DustLimit:              dustLimit,
		ConfirmationDepth:      confDepth,
	}
}

// A compile time check to ensure SingleFundingResponse implements the
// lnwire.Message interface.
var _ Message = (*SingleFundingResponse)(nil)

// Decode deserializes the serialized SingleFundingResponse stored in the passed
// io.Reader into the target SingleFundingResponse using the deserialization
// rules defined by the passed protocol version.
//
// This is part of the lnwire.Message interface.
func (c *SingleFundingResponse) Decode(r io.Reader, pver uint32) error {
	// ChannelID (8)
	// ChannelDerivationPoint (33)
	// CommitmentKey (33)
	// RevocationKey (33)
	// CsvDelay (4)
	// DeliveryPkScript (final delivery)
	// DustLimit (8)
	// ConfirmationDepth (4)
	err := readElements(r,
		&c.ChannelID,
		&c.ChannelDerivationPoint,
		&c.CommitmentKey,
		&c.RevocationKey,
		&c.CsvDelay,
		&c.DeliveryPkScript,
		&c.DustLimit,
		&c.ConfirmationDepth)
	if err != nil {
		return err
	}

	return nil
}

// Encode serializes the target SingleFundingResponse into the passed io.Writer
// implementation. Serialization will observe the rules defined by the passed
// protocol version.
//
// This is part of the lnwire.Message interface.
func (c *SingleFundingResponse) Encode(w io.Writer, pver uint32) error {
	err := writeElements(w,
		c.ChannelID,
		c.ChannelDerivationPoint,
		c.CommitmentKey,
		c.RevocationKey,
		c.CsvDelay,
		c.DeliveryPkScript,
		c.DustLimit,
		c.ConfirmationDepth)
	if err != nil {
		return err
	}

	return nil
}

// Command returns the uint32 code which uniquely identifies this message as a
// SingleFundingResponse on the wire.
//
// This is part of the lnwire.Message interface.
func (c *SingleFundingResponse) Command() uint32 {
	return CmdSingleFundingResponse
}

// MaxPayloadLength returns the maximum allowed payload length for a
// SingleFundingResponse. This is calculated by summing the max length of all
// the fields within a SingleFundingResponse. To enforce a maximum
// DeliveryPkScript size, the size of a P2PKH public key script is used.
//
// This is part of the lnwire.Message interface.
func (c *SingleFundingResponse) MaxPayloadLength(uint32) uint32 {
	var length uint32

	// ChannelID - 8 bytes
	length += 8

	// ChannelDerivationPoint - 33 bytes
	length += 33

	// CommitmentKey - 33 bytes
	length += 33

	// RevocationKey - 33 bytes
	length += 33

	// CsvDelay - 4 bytes
	length += 4

	// DeliveryPkScript - 25 bytes
	length += 25

	// DustLimit - 8 bytes
	length += 8

	// ConfirmationDepth - 4 bytes
	length += 4

	return length
}

// Validate examines each populated field within the SingleFundingResponse for
// field sanity. For example, all fields MUST NOT be negative, and all pkScripts
// must belong to the allowed set of public key scripts.
//
// This is part of the lnwire.Message interface.
func (c *SingleFundingResponse) Validate() error {
	// The channel derivation point must be non-nil, and have an odd
	// y-coordinate.
	if c.ChannelDerivationPoint == nil {
		return fmt.Errorf("The channel derivation point must be non-nil")
	}
	//if c.ChannelDerivationPoint.Y.Bit(0) != 1 {
	//	return fmt.Errorf("The channel derivation point must have an odd " +
	//		"y-coordinate")
	//}

	// The delivery pkScript must be amongst the supported script
	// templates.
	if !isValidPkScript(c.DeliveryPkScript) {
		return fmt.Errorf("Valid delivery public key scripts MUST be: " +
			"P2PKH, P2WKH, P2SH, or P2WSH.")
	}

	if c.DustLimit <= 0 {
		return fmt.Errorf("Dust limit shouldn't be below or equal to " +
			"zero.")
	}

	if c.ConfirmationDepth == 0 {
		return fmt.Errorf("ConfirmationDepth must be non-zero")
	}

	// We're good!
	return nil
}
