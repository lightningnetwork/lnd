package lnwire

import (
	"bytes"
	"fmt"
	"io"
	"sync"
)

// CustomTypeStart is the start of the custom type range for peer messages as
// defined in BOLT 01.
const CustomTypeStart MessageType = 32768

var (
	// customTypeOverride contains a set of message types < CustomTypeStart
	// that lnd allows to be treated as custom messages. This allows us to
	// override messages reserved for the protocol level and treat them as
	// custom messages. This set of message types is stored as a global so
	// that we do not need to pass around state when accounting for this
	// set of messages in message creation.
	//
	// Note: This global is protected by the customTypeOverride mutex.
	customTypeOverride map[MessageType]struct{}

	// customTypeOverrideMtx manages concurrent access to
	// customTypeOverride.
	customTypeOverrideMtx sync.RWMutex
)

// SetCustomOverrides validates that the set of override types are outside of
// the custom message range (there's no reason to override messages that are
// already within the range), and updates the customTypeOverride global to hold
// this set of message types. Note that this function will completely overwrite
// the set of overrides, so should be called with the full set of types.
func SetCustomOverrides(overrideTypes []uint16) error {
	customTypeOverrideMtx.Lock()
	defer customTypeOverrideMtx.Unlock()

	customTypeOverride = make(map[MessageType]struct{}, len(overrideTypes))

	for _, t := range overrideTypes {
		msgType := MessageType(t)

		if msgType >= CustomTypeStart {
			return fmt.Errorf("can't override type: %v, already "+
				"in custom range", t)
		}

		customTypeOverride[msgType] = struct{}{}
	}

	return nil
}

// IsCustomOverride returns a bool indicating whether the message type is one
// of the protocol messages that we override for custom use.
func IsCustomOverride(t MessageType) bool {
	customTypeOverrideMtx.RLock()
	defer customTypeOverrideMtx.RUnlock()

	_, ok := customTypeOverride[t]

	return ok
}

// Custom represents an application-defined wire message.
type Custom struct {
	Type MessageType
	Data []byte
}

// A compile time check to ensure Custom implements the lnwire.Message
// interface.
var _ Message = (*Custom)(nil)

// A compile time check to ensure Custom implements the lnwire.SizeableMessage
// interface.
var _ SizeableMessage = (*Custom)(nil)

// NewCustom instantiates a new custom message.
func NewCustom(msgType MessageType, data []byte) (*Custom, error) {
	if msgType < CustomTypeStart && !IsCustomOverride(msgType) {
		return nil, fmt.Errorf("msg type: %d not in custom range: %v "+
			"and not overridden", msgType, CustomTypeStart)
	}

	return &Custom{
		Type: msgType,
		Data: data,
	}, nil
}

// Encode serializes the target Custom message into the passed io.Writer
// implementation.
//
// This is part of the lnwire.Message interface.
func (c *Custom) Encode(b *bytes.Buffer, pver uint32) error {
	_, err := b.Write(c.Data)
	return err
}

// Decode deserializes the serialized Custom message stored in the passed
// io.Reader into the target Custom message.
//
// This is part of the lnwire.Message interface.
func (c *Custom) Decode(r io.Reader, pver uint32) error {
	var b bytes.Buffer
	if _, err := io.Copy(&b, r); err != nil {
		return err
	}

	c.Data = b.Bytes()

	return nil
}

// MsgType returns the uint32 code which uniquely identifies this message as a
// Custom message on the wire.
//
// This is part of the lnwire.Message interface.
func (c *Custom) MsgType() MessageType {
	return c.Type
}

// SerializedSize returns the serialized size of the message in bytes.
//
// This is part of the lnwire.SizeableMessage interface.
func (c *Custom) SerializedSize() (uint32, error) {
	return MessageSerializedSize(c)
}
