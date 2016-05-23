package lnwire

import (
	"fmt"
	"io"
)

// HTLCAddRequest is the message sent by Alice to Bob when she wishes to add an
// HTLC to his remote commitment transaction. In addition to information
// detailing the value, and contract type of the HTLC, and onion blob is also
// included which allows Bob to derive the next hop in the route. The HTLC
// added by this message is to be added to the remote node's "pending" HTLC's.
// A subsequent CommitSignature message will move the pending HTLC to the newly
// created commitment transaction, marking them as "staged".
type HTLCAddRequest struct {
	// ChannelID is the particular active channel that this HTLCAddRequest
	// is binded to.
	ChannelID uint64

	// Expiry is the number of blocks after which this HTLC should expire.
	// It is the receiver's duty to ensure that the outgoing HTLC has a
	// sufficient expiry value to allow her to redeem the incmoing HTLC.
	Expiry uint32

	// Amount to pay in the hop
	// Difference between hop and first item in blob is the fee to complete

	// Amount is the number of credits this HTLC is worth.
	Amount CreditsAmount

	// RefundContext is for payment cancellation
	// TODO(j): not currently in use, add later
	RefundContext HTLCKey

	// ContractType defines the particular output script to be used for
	// this HTLC. This value defaults to zero for regular HTLCs. For
	// multi-sig HTLCs, then first 4 bit represents N, while the second 4
	// bits are M, within the N-of-M multi-sig.
	ContractType uint8

	// RedemptionHashes are the hashes to be used within the HTLC script.
	// An HTLC is only fufilled once Bob is provided with the required
	// number of pre-images for each of the listed hashes. For regular HTLC's
	// this slice only has one hash. However, for "multi-sig" HTLC's, the
	// length of this slice should be N.
	RedemptionHashes [][20]byte

	// Data to parse&pass on to the next node
	// Nested HTLCAddRequests with a uint32 in front for the size
	Blob []byte

	// OnionBlob is the raw serialized mix header used to route an HTLC in
	// a privacy-preserving manner. The mix header is defined currently to
	// be parsed as a 4-tuple: (groupElement, routingInfo, headerMAC, body).
	// First the receiving node should use the groupElement, and its current
	// onion key to derive a shared secret with the source. Once the shared
	// secret has been derived, the headerMAC should be checked FIRST. Note
	// that the MAC only covers the routingInfo field. If the MAC matches,
	// and the shared secret is fresh, then the node should stip off a layer
	// of encryption, exposing the next hop to be used in the subsequent
	// HTLCAddRequest message.
	OnionBlob []byte
}

// Decode deserializes a serialized HTLCAddRequest message stored in the passed
// io.Reader observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (c *HTLCAddRequest) Decode(r io.Reader, pver uint32) error {
	// ChannelID(8)
	// Expiry(4)
	// Amount(4)
	// ContractType(1)
	// RedemptionHashes (numOfHashes * 20 + numOfHashes)
	// Blob(2+blobsize)
	// OnionBlog
	err := readElements(r,
		&c.ChannelID,
		&c.Expiry,
		&c.Amount,
		&c.ContractType,
		&c.RedemptionHashes,
		&c.Blob,
		&c.OnionBlob,
	)
	if err != nil {
		return err
	}

	return nil
}

// NewHTLCAddRequest returns a new empty HTLCAddRequest message.
func NewHTLCAddRequest() *HTLCAddRequest {
	return &HTLCAddRequest{}
}

// A compile time check to ensure HTLCAddRequest implements the lnwire.Message
// interface.
var _ Message = (*HTLCAddRequest)(nil)

// Encode serializes the target HTLCAddRequest into the passed io.Writer observing
// the protocol version specified.
//
// This is part of the lnwire.Message interface.
func (c *HTLCAddRequest) Encode(w io.Writer, pver uint32) error {
	err := writeElements(w,
		c.ChannelID,
		c.Expiry,
		c.Amount,
		c.ContractType,
		c.RedemptionHashes,
		c.Blob,
		c.OnionBlob,
	)
	if err != nil {
		return err
	}

	return nil
}

// Command returns the integer uniquely identifying this message type on the
// wire.
//
// This is part of the lnwire.Message interface.
func (c *HTLCAddRequest) Command() uint32 {
	return CmdHTLCAddRequest
}

// MaxPayloadLength returns the maximum allowed payload size for a HTLCAddRequest
// complete message observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (c *HTLCAddRequest) MaxPayloadLength(uint32) uint32 {
	// base size ~110, but blob can be variable.
	// shouldn't be bigger than 8K though...
	return 8192
}

// Validate performs any necessary sanity checks to ensure all fields present
// on the HTLCAddRequest are valid.
//
// This is part of the lnwire.Message interface.
func (c *HTLCAddRequest) Validate() error {
	if c.Amount < 0 {
		// While fees can be negative, it's too confusing to allow
		// negative payments. Maybe for some wallets, but not this one!
		return fmt.Errorf("Amount paid cannot be negative.")
	}
	// We're good!
	return nil
}

// String returns the string representation of the target HTLCAddRequest.
//
// This is part of the lnwire.Message interface.
func (c *HTLCAddRequest) String() string {
	var redemptionHashes string
	for i, rh := range c.RedemptionHashes {
		redemptionHashes += fmt.Sprintf("\n\tSlice\t%d\n", i)
		redemptionHashes += fmt.Sprintf("\t\tRedemption Hash: %x\n", rh)
	}

	return fmt.Sprintf("\n--- Begin HTLCAddRequest ---\n") +
		fmt.Sprintf("ChannelID:\t%d\n", c.ChannelID) +
		fmt.Sprintf("Expiry:\t\t%d\n", c.Expiry) +
		fmt.Sprintf("Amount\t\t%d\n", c.Amount) +
		fmt.Sprintf("ContractType:\t%d (%b)\n", c.ContractType, c.ContractType) +
		fmt.Sprintf("RedemptionHashes:") +
		redemptionHashes +
		fmt.Sprintf("Blob:\t\t\t\t%x\n", c.Blob) +
		fmt.Sprintf("OnionBlob:\t\t\t\t%x\n", c.OnionBlob) +
		fmt.Sprintf("--- End HTLCAddRequest ---\n")
}
