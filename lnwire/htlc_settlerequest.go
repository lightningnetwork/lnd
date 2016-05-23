package lnwire

import (
	"fmt"
	"io"
)

// HTLCSettleRequest is sent by Alice to Bob when she wishes to settle a
// particular HTLC referenced by its HTLCKey within a specific active channel
// referenced by ChannelID. The message allows multiple hash preimages to be
// presented in order to support N-of-M HTLC contracts. A subsequent
// CommitSignature message will be sent by Alice to "lock-in" the removal of the
// specified HTLC, possible containing a batch signature covering several settled
// HTLC's.
type HTLCSettleRequest struct {
	// ChannelID references an active channel which holds the HTLC to be
	// settled.
	ChannelID uint64

	// HTLCKey denotes the exact HTLC stage within the receiving node's
	// commitment transaction to be removed.
	HTLCKey HTLCKey

	// RedemptionProofs are the R-value preimages required to fully settle
	// an HTLC. The number of preimages in the slice will depend on the
	// specific ContractType of the referenced HTLC.
	RedemptionProofs [][20]byte
}

// Decode deserializes a serialized HTLCSettleRequest message stored in the passed
// io.Reader observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (c *HTLCSettleRequest) Decode(r io.Reader, pver uint32) error {
	// ChannelID(8)
	// HTLCKey(8)
	// Expiry(4)
	// Amount(4)
	// NextHop(20)
	// ContractType(1)
	// RedemptionHashes (numOfHashes * 20 + numOfHashes)
	// Blob(2+blobsize)
	err := readElements(r,
		&c.ChannelID,
		&c.HTLCKey,
		&c.RedemptionProofs,
	)
	if err != nil {
		return err
	}

	return nil
}

// NewHTLCSettleRequest returns a new empty HTLCSettleRequest.
func NewHTLCSettleRequest() *HTLCSettleRequest {
	return &HTLCSettleRequest{}
}

// A compile time check to ensure HTLCSettleRequest implements the lnwire.Message
// interface.
var _ Message = (*HTLCSettleRequest)(nil)

// Serializes the item from the HTLCSettleRequest struct
// Writes the data to w
func (c *HTLCSettleRequest) Encode(w io.Writer, pver uint32) error {
	err := writeElements(w,
		c.ChannelID,
		c.HTLCKey,
		c.RedemptionProofs,
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
func (c *HTLCSettleRequest) Command() uint32 {
	return CmdHTLCSettleRequest
}

// MaxPayloadLength returns the maximum allowed payload size for a HTLCSettleRequest
// complete message observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (c *HTLCSettleRequest) MaxPayloadLength(uint32) uint32 {
	// 8 + 8 + (21 * 15)
	return 331
}

// Validate performs any necessary sanity checks to ensure all fields present
// on the HTLCSettleRequest are valid.
//
// This is part of the lnwire.Message interface.
func (c *HTLCSettleRequest) Validate() error {
	// We're good!
	return nil
}

// String returns the string representation of the target HTLCSettleRequest.
//
// This is part of the lnwire.Message interface.
func (c *HTLCSettleRequest) String() string {
	var redemptionProofs string
	for i, rh := range c.RedemptionProofs {
		redemptionProofs += fmt.Sprintf("\n\tSlice\t%d\n", i)
		redemptionProofs += fmt.Sprintf("\t\tRedemption Proof: %x\n", rh)
	}

	return fmt.Sprintf("\n--- Begin HTLCSettleRequest ---\n") +
		fmt.Sprintf("ChannelID:\t%d\n", c.ChannelID) +
		fmt.Sprintf("HTLCKey:\t%d\n", c.HTLCKey) +
		fmt.Sprintf("RedemptionHashes:") +
		redemptionProofs +
		fmt.Sprintf("--- End HTLCSettleRequest ---\n")
}
