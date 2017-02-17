package lnwire

import (
	"io"
	"github.com/go-errors/errors"
)

// Init is the first message reveals the features supported or required by this
// node. Nodes wait for receipt of the other's features to simplify error
// diagnosis where features are incompatible. Each node MUST wait to receive
// init before sending any other messages.
type Init struct {
	// GlobalFeatures is feature vector which affects HTLCs and thus are
	// also advertised to other nodes.
	GlobalFeatures *FeatureVector

	// LocalFeatures is feature vector which only affect the protocol
	// between two nodes.
	LocalFeatures *FeatureVector
}

// NewInitMessage creates new instance of init message object.
func NewInitMessage(gf, lf *FeatureVector) *Init {
	return &Init{
		GlobalFeatures: gf,
		LocalFeatures:  lf,
	}
}

// Decode deserializes a serialized Init message stored in the passed
// io.Reader observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (msg *Init) Decode(r io.Reader, pver uint32) error {
	// LocalFeatures(~)
	// GlobalFeatures(~)
	err := readElements(r,
		&msg.LocalFeatures,
		&msg.GlobalFeatures,
	)
	if err != nil {
		return err
	}

	return nil
}

// A compile time check to ensure Init implements the lnwire.Message
// interface.
var _ Message = (*Init)(nil)

// Encode serializes the target Init into the passed io.Writer observing
// the protocol version specified.
//
// This is part of the lnwire.Message interface.
func (msg *Init) Encode(w io.Writer, pver uint32) error {
	err := writeElements(w,
		msg.LocalFeatures,
		msg.GlobalFeatures,
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
func (msg *Init) Command() uint32 {
	return CmdInit
}

// MaxPayloadLength returns the maximum allowed payload size for a Init
// complete message observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (msg *Init) MaxPayloadLength(uint32) uint32 {
	return 2 + maxAllowedSize + 2 + maxAllowedSize
}

// Validate performs any necessary sanity checks to ensure all fields present
// on the Init are valid.
//
// This is part of the lnwire.Message interface.
func (msg *Init) Validate() error {
	if msg.GlobalFeatures.serializedSize() > maxAllowedSize {
		return errors.Errorf("global feature vector exceed max allowed "+
			"size %v", maxAllowedSize)
	}

	if msg.LocalFeatures.serializedSize() > maxAllowedSize {
		return errors.Errorf("local feature vector exceed max allowed "+
			"size %v", maxAllowedSize)
	}

	return nil
}
