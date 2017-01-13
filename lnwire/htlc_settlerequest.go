package lnwire

import (
	"io"

	"github.com/roasbeef/btcd/wire"
)

// HTLCSettleRequest is sent by Alice to Bob when she wishes to settle a
// particular HTLC referenced by its HTLCKey within a specific active channel
// referenced by ChannelPoint. The message allows multiple hash preimages to be
// presented in order to support N-of-M HTLC contracts. A subsequent
// CommitSignature message will be sent by Alice to "lock-in" the removal of the
// specified HTLC, possible containing a batch signature covering several settled
// HTLCs.
type HTLCSettleRequest struct {
	// ChannelPoint references an active channel which holds the HTLC to be
	// settled.
	ChannelPoint *wire.OutPoint

	// HTLCKey denotes the exact HTLC stage within the receiving node's
	// commitment transaction to be removed.
	// TODO(roasbeef): rename to LogIndex
	HTLCKey HTLCKey

	// RedemptionProofs are the R-value preimages required to fully settle
	// an HTLC. The number of preimages in the slice will depend on the
	// specific ContractType of the referenced HTLC.
	RedemptionProofs [][32]byte
}

// NewHTLCSettleRequest returns a new empty HTLCSettleRequest.
func NewHTLCSettleRequest(chanPoint *wire.OutPoint, key HTLCKey,
	redemptionProofs [][32]byte) *HTLCSettleRequest {

	return &HTLCSettleRequest{
		ChannelPoint:     chanPoint,
		HTLCKey:          key,
		RedemptionProofs: redemptionProofs,
	}
}

// A compile time check to ensure HTLCSettleRequest implements the lnwire.Message
// interface.
var _ Message = (*HTLCSettleRequest)(nil)

// Decode deserializes a serialized HTLCSettleRequest message stored in the passed
// io.Reader observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (c *HTLCSettleRequest) Decode(r io.Reader, pver uint32) error {
	// ChannelPoint(8)
	// HTLCKey(8)
	// RedemptionProofs(N*32)
	err := readElements(r,
		&c.ChannelPoint,
		&c.HTLCKey,
		&c.RedemptionProofs,
	)
	if err != nil {
		return err
	}

	return nil
}

// Encode serializes the target HTLCSettleRequest into the passed io.Writer
// observing the protocol version specified.
//
// This is part of the lnwire.Message interface.
func (c *HTLCSettleRequest) Encode(w io.Writer, pver uint32) error {
	err := writeElements(w,
		c.ChannelPoint,
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
	// 36 + 8 + (32 * 15)
	return 524
}

// Validate performs any necessary sanity checks to ensure all fields present
// on the HTLCSettleRequest are valid.
//
// This is part of the lnwire.Message interface.
func (c *HTLCSettleRequest) Validate() error {
	// We're good!
	return nil
}
