package lnwire

import (
	"fmt"
	"io"

	"github.com/lightningnetwork/lnd/tlv"
)

const (
	// AliasScidRecordType is the type of the experimental record to denote
	// the alias being used in an option_scid_alias channel.
	AliasScidRecordType tlv.Type = 1
)

// ShortChannelID represents the set of data which is needed to retrieve all
// necessary data to validate the channel existence.
type ShortChannelID struct {
	// BlockHeight is the height of the block where funding transaction
	// located.
	//
	// NOTE: This field is limited to 3 bytes.
	BlockHeight uint32

	// TxIndex is a position of funding transaction within a block.
	//
	// NOTE: This field is limited to 3 bytes.
	TxIndex uint32

	// TxPosition indicating transaction output which pays to the channel.
	TxPosition uint16
}

// NewShortChanIDFromInt returns a new ShortChannelID which is the decoded
// version of the compact channel ID encoded within the uint64. The format of
// the compact channel ID is as follows: 3 bytes for the block height, 3 bytes
// for the transaction index, and 2 bytes for the output index.
func NewShortChanIDFromInt(chanID uint64) ShortChannelID {
	return ShortChannelID{
		BlockHeight: uint32(chanID >> 40),
		TxIndex:     uint32(chanID>>16) & 0xFFFFFF,
		TxPosition:  uint16(chanID),
	}
}

// ToUint64 converts the ShortChannelID into a compact format encoded within a
// uint64 (8 bytes).
func (c ShortChannelID) ToUint64() uint64 {
	// TODO(roasbeef): explicit error on overflow?
	return ((uint64(c.BlockHeight) << 40) | (uint64(c.TxIndex) << 16) |
		(uint64(c.TxPosition)))
}

// String generates a human-readable representation of the channel ID.
func (c ShortChannelID) String() string {
	return fmt.Sprintf("%d:%d:%d", c.BlockHeight, c.TxIndex, c.TxPosition)
}

// AltString generates a human-readable representation of the channel ID
// with 'x' as a separator.
func (c ShortChannelID) AltString() string {
	return fmt.Sprintf("%dx%dx%d", c.BlockHeight, c.TxIndex, c.TxPosition)
}

// Record returns a TLV record that can be used to encode/decode a
// ShortChannelID to/from a TLV stream.
func (c *ShortChannelID) Record() tlv.Record {
	return tlv.MakeStaticRecord(
		AliasScidRecordType, c, 8, EShortChannelID, DShortChannelID,
	)
}

// IsDefault returns true if the ShortChannelID represents the zero value for
// its type.
func (c ShortChannelID) IsDefault() bool {
	return c == ShortChannelID{}
}

// EShortChannelID is an encoder for ShortChannelID. It is exported so other
// packages can use the encoding scheme.
func EShortChannelID(w io.Writer, val interface{}, buf *[8]byte) error {
	if v, ok := val.(*ShortChannelID); ok {
		return tlv.EUint64T(w, v.ToUint64(), buf)
	}
	return tlv.NewTypeForEncodingErr(val, "lnwire.ShortChannelID")
}

// DShortChannelID is a decoder for ShortChannelID. It is exported so other
// packages can use the decoding scheme.
func DShortChannelID(r io.Reader, val interface{}, buf *[8]byte,
	l uint64) error {

	if v, ok := val.(*ShortChannelID); ok {
		var scid uint64
		err := tlv.DUint64(r, &scid, buf, 8)
		if err != nil {
			return err
		}

		*v = NewShortChanIDFromInt(scid)
		return nil
	}
	return tlv.NewTypeForDecodingErr(val, "lnwire.ShortChannelID", l, 8)
}
