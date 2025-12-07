// Copyright (c) 2013-2017 The btcsuite developers
// Copyright (c) 2015-2016 The Decred developers
// code derived from https://github .com/btcsuite/btcd/blob/master/wire/message.go
// Copyright (C) 2015-2022 The Lightning Network Developers

package lnwire

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
)

// MessageTypeSize is the size in bytes of the message type field in the header
// of all messages.
const MessageTypeSize = 2

// MessageType is the unique 2 byte big-endian integer that indicates the type
// of message on the wire. All messages have a very simple header which
// consists simply of 2-byte message type. We omit a length field, and checksum
// as the Lightning Protocol is intended to be encapsulated within a
// confidential+authenticated cryptographic messaging protocol.
type MessageType uint16

// The currently defined message types within this current version of the
// Lightning protocol.
const (
	MsgWarning                 MessageType = 1
	MsgStfu                                = 2
	MsgInit                                = 16
	MsgError                               = 17
	MsgPing                                = 18
	MsgPong                                = 19
	MsgOpenChannel                         = 32
	MsgAcceptChannel                       = 33
	MsgFundingCreated                      = 34
	MsgFundingSigned                       = 35
	MsgChannelReady                        = 36
	MsgShutdown                            = 38
	MsgClosingSigned                       = 39
	MsgClosingComplete                     = 40
	MsgClosingSig                          = 41
	MsgDynPropose                          = 111
	MsgDynAck                              = 113
	MsgDynReject                           = 115
	MsgDynCommit                           = 117
	MsgUpdateAddHTLC                       = 128
	MsgUpdateFulfillHTLC                   = 130
	MsgUpdateFailHTLC                      = 131
	MsgCommitSig                           = 132
	MsgRevokeAndAck                        = 133
	MsgUpdateFee                           = 134
	MsgUpdateFailMalformedHTLC             = 135
	MsgChannelReestablish                  = 136
	MsgChannelAnnouncement                 = 256
	MsgNodeAnnouncement                    = 257
	MsgChannelUpdate                       = 258
	MsgAnnounceSignatures                  = 259
	MsgAnnounceSignatures2                 = 260
	MsgQueryShortChanIDs                   = 261
	MsgReplyShortChanIDsEnd                = 262
	MsgQueryChannelRange                   = 263
	MsgReplyChannelRange                   = 264
	MsgGossipTimestampRange                = 265
	MsgChannelAnnouncement2                = 267
	MsgNodeAnnouncement2                   = 269
	MsgChannelUpdate2                      = 271
	MsgOnionMessage                        = 513
	MsgKickoffSig                          = 777

	// MsgEnd defines the end of the official message range of the protocol.
	// If a new message is added beyond this message, then this should be
	// modified.
	MsgEnd = 778
)

// IsChannelUpdate is a filter function that discerns channel update messages
// from the other messages in the Lightning Network Protocol.
func (t MessageType) IsChannelUpdate() bool {
	switch t {
	case MsgUpdateAddHTLC:
		return true
	case MsgUpdateFulfillHTLC:
		return true
	case MsgUpdateFailHTLC:
		return true
	case MsgUpdateFailMalformedHTLC:
		return true
	case MsgUpdateFee:
		return true
	default:
		return false
	}
}

// ErrorEncodeMessage is used when failed to encode the message payload.
func ErrorEncodeMessage(err error) error {
	return fmt.Errorf("failed to encode message to buffer, got %w", err)
}

// ErrorWriteMessageType is used when failed to write the message type.
func ErrorWriteMessageType(err error) error {
	return fmt.Errorf("failed to write message type, got %w", err)
}

// ErrorPayloadTooLarge is used when the payload size exceeds the
// MaxMsgBody.
func ErrorPayloadTooLarge(size int) error {
	return fmt.Errorf(
		"message payload is too large - encoded %d bytes, "+
			"but maximum message payload is %d bytes",
		size, MaxMsgBody,
	)
}

// String return the string representation of message type.
func (t MessageType) String() string {
	switch t {
	case MsgWarning:
		return "Warning"
	case MsgStfu:
		return "Stfu"
	case MsgInit:
		return "Init"
	case MsgOpenChannel:
		return "MsgOpenChannel"
	case MsgAcceptChannel:
		return "MsgAcceptChannel"
	case MsgFundingCreated:
		return "MsgFundingCreated"
	case MsgFundingSigned:
		return "MsgFundingSigned"
	case MsgChannelReady:
		return "ChannelReady"
	case MsgShutdown:
		return "Shutdown"
	case MsgClosingSigned:
		return "ClosingSigned"
	case MsgDynPropose:
		return "DynPropose"
	case MsgDynAck:
		return "DynAck"
	case MsgDynReject:
		return "DynReject"
	case MsgDynCommit:
		return "DynCommit"
	case MsgKickoffSig:
		return "KickoffSig"
	case MsgUpdateAddHTLC:
		return "UpdateAddHTLC"
	case MsgUpdateFailHTLC:
		return "UpdateFailHTLC"
	case MsgUpdateFulfillHTLC:
		return "UpdateFulfillHTLC"
	case MsgCommitSig:
		return "CommitSig"
	case MsgRevokeAndAck:
		return "RevokeAndAck"
	case MsgUpdateFailMalformedHTLC:
		return "UpdateFailMalformedHTLC"
	case MsgChannelReestablish:
		return "ChannelReestablish"
	case MsgError:
		return "Error"
	case MsgChannelAnnouncement:
		return "ChannelAnnouncement"
	case MsgChannelUpdate:
		return "ChannelUpdate"
	case MsgNodeAnnouncement:
		return "NodeAnnouncement1"
	case MsgPing:
		return "Ping"
	case MsgAnnounceSignatures:
		return "AnnounceSignatures"
	case MsgPong:
		return "Pong"
	case MsgUpdateFee:
		return "UpdateFee"
	case MsgQueryShortChanIDs:
		return "QueryShortChanIDs"
	case MsgReplyShortChanIDsEnd:
		return "ReplyShortChanIDsEnd"
	case MsgQueryChannelRange:
		return "QueryChannelRange"
	case MsgReplyChannelRange:
		return "ReplyChannelRange"
	case MsgGossipTimestampRange:
		return "GossipTimestampRange"
	case MsgClosingComplete:
		return "ClosingComplete"
	case MsgClosingSig:
		return "ClosingSig"
	case MsgAnnounceSignatures2:
		return "MsgAnnounceSignatures2"
	case MsgChannelAnnouncement2:
		return "ChannelAnnouncement2"
	case MsgNodeAnnouncement2:
		return "NodeAnnouncement2"
	case MsgChannelUpdate2:
		return "ChannelUpdate2"
	case MsgOnionMessage:
		return "OnionMessage"
	default:
		return "<unknown>"
	}
}

// UnknownMessage is an implementation of the error interface that allows the
// creation of an error in response to an unknown message.
type UnknownMessage struct {
	messageType MessageType
}

// Error returns a human readable string describing the error.
//
// This is part of the error interface.
func (u *UnknownMessage) Error() string {
	return fmt.Sprintf("unable to parse message of unknown type: %v",
		u.messageType)
}

// Serializable is an interface which defines a lightning wire serializable
// object.
type Serializable interface {
	// Decode reads the bytes stream and converts it to the object.
	Decode(io.Reader, uint32) error

	// Encode converts object to the bytes stream and write it into the
	// write buffer.
	Encode(*bytes.Buffer, uint32) error
}

// Message is an interface that defines a lightning wire protocol message. The
// interface is general in order to allow implementing types full control over
// the representation of its data.
type Message interface {
	Serializable
	MsgType() MessageType
}

// LinkUpdater is an interface implemented by most messages in BOLT 2 that are
// allowed to update the channel state.
type LinkUpdater interface {
	// All LinkUpdater messages are messages and so we embed the interface
	// so that we can treat it as a message if all we know about it is that
	// it is a LinkUpdater message.
	Message

	// TargetChanID returns the channel id of the link for which this
	// message is intended.
	TargetChanID() ChannelID
}

// SizeableMessage is an interface that extends the base Message interface with
// a method to calculate the serialized size of a message.
type SizeableMessage interface {
	Message

	// SerializedSize returns the serialized size of the message in bytes.
	// The returned size includes the message type header bytes.
	SerializedSize() (uint32, error)
}

// MessageSerializedSize calculates the serialized size of a message in bytes.
// This is a helper function that can be used by all message types to implement
// the SerializedSize method.
func MessageSerializedSize(msg Message) (uint32, error) {
	var buf bytes.Buffer

	// Encode the message to the buffer.
	if err := msg.Encode(&buf, 0); err != nil {
		return 0, err
	}

	// Add the size of the message type.
	return uint32(buf.Len()) + MessageTypeSize, nil
}

// makeEmptyMessage creates a new empty message of the proper concrete type
// based on the passed message type.
func makeEmptyMessage(msgType MessageType) (Message, error) {
	var msg Message

	switch msgType {
	case MsgWarning:
		msg = &Warning{}
	case MsgStfu:
		msg = &Stfu{}
	case MsgInit:
		msg = &Init{}
	case MsgOpenChannel:
		msg = &OpenChannel{}
	case MsgAcceptChannel:
		msg = &AcceptChannel{}
	case MsgFundingCreated:
		msg = &FundingCreated{}
	case MsgFundingSigned:
		msg = &FundingSigned{}
	case MsgChannelReady:
		msg = &ChannelReady{}
	case MsgShutdown:
		msg = &Shutdown{}
	case MsgClosingSigned:
		msg = &ClosingSigned{}
	case MsgDynPropose:
		msg = &DynPropose{}
	case MsgDynAck:
		msg = &DynAck{}
	case MsgDynReject:
		msg = &DynReject{}
	case MsgDynCommit:
		msg = &DynCommit{}
	case MsgKickoffSig:
		msg = &KickoffSig{}
	case MsgUpdateAddHTLC:
		msg = &UpdateAddHTLC{}
	case MsgUpdateFailHTLC:
		msg = &UpdateFailHTLC{}
	case MsgUpdateFulfillHTLC:
		msg = &UpdateFulfillHTLC{}
	case MsgCommitSig:
		msg = &CommitSig{}
	case MsgRevokeAndAck:
		msg = &RevokeAndAck{}
	case MsgUpdateFee:
		msg = &UpdateFee{}
	case MsgUpdateFailMalformedHTLC:
		msg = &UpdateFailMalformedHTLC{}
	case MsgChannelReestablish:
		msg = &ChannelReestablish{}
	case MsgError:
		msg = &Error{}
	case MsgChannelAnnouncement:
		msg = &ChannelAnnouncement1{}
	case MsgChannelUpdate:
		msg = &ChannelUpdate1{}
	case MsgNodeAnnouncement:
		msg = &NodeAnnouncement1{}
	case MsgPing:
		msg = &Ping{}
	case MsgAnnounceSignatures:
		msg = &AnnounceSignatures1{}
	case MsgPong:
		msg = &Pong{}
	case MsgQueryShortChanIDs:
		msg = &QueryShortChanIDs{}
	case MsgReplyShortChanIDsEnd:
		msg = &ReplyShortChanIDsEnd{}
	case MsgQueryChannelRange:
		msg = &QueryChannelRange{}
	case MsgReplyChannelRange:
		msg = &ReplyChannelRange{}
	case MsgGossipTimestampRange:
		msg = &GossipTimestampRange{}
	case MsgClosingComplete:
		msg = &ClosingComplete{}
	case MsgClosingSig:
		msg = &ClosingSig{}
	case MsgAnnounceSignatures2:
		msg = &AnnounceSignatures2{}
	case MsgChannelAnnouncement2:
		msg = &ChannelAnnouncement2{}
	case MsgNodeAnnouncement2:
		msg = &NodeAnnouncement2{}
	case MsgChannelUpdate2:
		msg = &ChannelUpdate2{}
	case MsgOnionMessage:
		msg = &OnionMessage{}
	default:
		// If the message is not within our custom range and has not
		// specifically been overridden, return an unknown message.
		//
		// Note that we do not allow custom message overrides to replace
		// known message types, only protocol messages that are not yet
		// known to lnd.
		if msgType < CustomTypeStart && !IsCustomOverride(msgType) {
			return nil, &UnknownMessage{msgType}
		}

		msg = &Custom{
			Type: msgType,
		}
	}

	return msg, nil
}

// MakeEmptyMessage creates a new empty message of the proper concrete type
// based on the passed message type. This is exported to be used in tests.
func MakeEmptyMessage(msgType MessageType) (Message, error) {
	return makeEmptyMessage(msgType)
}

// WriteMessage writes a lightning Message to a buffer including the necessary
// header information and returns the number of bytes written. If any error is
// encountered, the buffer passed will be reset to its original state since we
// don't want any broken bytes left. In other words, no bytes will be written
// if there's an error. Either all or none of the message bytes will be written
// to the buffer.
//
// NOTE: this method is not concurrent safe.
func WriteMessage(buf *bytes.Buffer, msg Message, pver uint32) (int, error) {
	// Record the size of the bytes already written in buffer.
	oldByteSize := buf.Len()

	// cleanBrokenBytes is a helper closure that helps reset the buffer to
	// its original state. It truncates all the bytes written in current
	// scope.
	var cleanBrokenBytes = func(b *bytes.Buffer) int {
		b.Truncate(oldByteSize)
		return 0
	}

	// Write the message type.
	var mType [2]byte
	binary.BigEndian.PutUint16(mType[:], uint16(msg.MsgType()))
	msgTypeBytes, err := buf.Write(mType[:])
	if err != nil {
		return cleanBrokenBytes(buf), ErrorWriteMessageType(err)
	}

	// Use the write buffer to encode our message.
	if err := msg.Encode(buf, pver); err != nil {
		return cleanBrokenBytes(buf), ErrorEncodeMessage(err)
	}

	// Enforce maximum overall message payload. The write buffer now has
	// the size of len(originalBytes) + len(payload) + len(type). We want
	// to enforce the payload here, so we subtract it by the length of the
	// type and old bytes.
	lenp := buf.Len() - oldByteSize - msgTypeBytes
	if lenp > MaxMsgBody {
		return cleanBrokenBytes(buf), ErrorPayloadTooLarge(lenp)
	}

	return buf.Len() - oldByteSize, nil
}

// ReadMessage reads, validates, and parses the next Lightning message from r
// for the provided protocol version.
func ReadMessage(r io.Reader, pver uint32) (Message, error) {
	// First, we'll read out the first two bytes of the message so we can
	// create the proper empty message.
	var mType [2]byte
	if _, err := io.ReadFull(r, mType[:]); err != nil {
		return nil, err
	}

	msgType := MessageType(binary.BigEndian.Uint16(mType[:]))

	// Now that we know the target message type, we can create the proper
	// empty message type and decode the message into it.
	msg, err := makeEmptyMessage(msgType)
	if err != nil {
		return nil, err
	}
	if err := msg.Decode(r, pver); err != nil {
		return nil, err
	}

	return msg, nil
}
