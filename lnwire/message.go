package lnwire

// code derived from https://github .com/btcsuite/btcd/blob/master/wire/message.go

import (
	"bytes"
	"fmt"
	"io"

	"github.com/roasbeef/btcd/wire"
)

// MessageHeaderSize is the number of bytes in a lightning message header.
// The bytes are allocated as follows: network magic 4 bytes + command 4
// bytes + payload length 4 bytes. Note that a checksum is omitted as lightning
// messages are assumed to be transmitted over an AEAD secured connection which
// provides integrity over the entire message.
const MessageHeaderSize = 12

// MaxMessagePayload is the maximum bytes a message can be regardless of other
// individual limits imposed by messages themselves.
const MaxMessagePayload = 1024 * 1024 * 32 //  32MB

// Commands used in lightning message headers which detail the type of message.
// TODO(roasbeef): update with latest type numbering from spec
const (
	CmdInit = uint32(1)

	// Commands for opening a channel funded by one party (single funder).
	CmdSingleFundingRequest      = uint32(100)
	CmdSingleFundingResponse     = uint32(110)
	CmdSingleFundingComplete     = uint32(120)
	CmdSingleFundingSignComplete = uint32(130)

	// Command for locking a funded channel
	CmdFundingLocked = uint32(200)

	// Commands for the workflow of cooperatively closing an active channel.
	CmdCloseRequest  = uint32(300)
	CmdCloseComplete = uint32(310)

	// Commands for negotiating HTLCs.
	CmdUpdateAddHTLC    = uint32(1000)
	CmdUpdateFufillHTLC = uint32(1010)
	CmdUpdateFailHTLC   = uint32(1020)

	// Commands for modifying commitment transactions.
	CmdCommitSig    = uint32(2000)
	CmdRevokeAndAck = uint32(2010)

	// Commands for reporting protocol errors.
	CmdError = uint32(4000)

	// Commands for discovery service.
	CmdChannelAnnouncement       = uint32(5000)
	CmdChannelUpdateAnnouncement = uint32(5010)
	CmdNodeAnnouncement          = uint32(5020)
	CmdAnnounceSignatures        = uint32(5030)

	// Commands for connection keep-alive.
	CmdPing = uint32(6000)
	CmdPong = uint32(6010)
)

// UnknownMessage is an implementation of the error interface that allows the
// creation of an error in response to an unknown message.
type UnknownMessage struct {
	messageType uint32
}

// Error returns a human readable string describing the error.
//
// This is part of the error interface.
func (u *UnknownMessage) Error() string {
	return fmt.Sprintf("unable to parse message of unknown type: %v",
		u.messageType)
}

// Message is an interface that defines a lightning wire protocol message. The
// interface is general in order to allow implementing types full control over
// the representation of its data.
type Message interface {
	Decode(io.Reader, uint32) error
	Encode(io.Writer, uint32) error
	Command() uint32
	MaxPayloadLength(uint32) uint32
	Validate() error
}

// makeEmptyMessage creates a new empty message of the proper concrete type
// based on the command ID.
func makeEmptyMessage(command uint32) (Message, error) {
	var msg Message

	switch command {
	case CmdInit:
		msg = &Init{}
	case CmdSingleFundingRequest:
		msg = &SingleFundingRequest{}
	case CmdSingleFundingResponse:
		msg = &SingleFundingResponse{}
	case CmdSingleFundingComplete:
		msg = &SingleFundingComplete{}
	case CmdSingleFundingSignComplete:
		msg = &SingleFundingSignComplete{}
	case CmdFundingLocked:
		msg = &FundingLocked{}
	case CmdCloseRequest:
		msg = &CloseRequest{}
	case CmdCloseComplete:
		msg = &CloseComplete{}
	case CmdUpdateAddHTLC:
		msg = &UpdateAddHTLC{}
	case CmdUpdateFailHTLC:
		msg = &UpdateFailHTLC{}
	case CmdUpdateFufillHTLC:
		msg = &UpdateFufillHTLC{}
	case CmdCommitSig:
		msg = &CommitSig{}
	case CmdRevokeAndAck:
		msg = &RevokeAndAck{}
	case CmdError:
		msg = &Error{}
	case CmdChannelAnnouncement:
		msg = &ChannelAnnouncement{}
	case CmdChannelUpdateAnnouncement:
		msg = &ChannelUpdateAnnouncement{}
	case CmdNodeAnnouncement:
		msg = &NodeAnnouncement{}
	case CmdPing:
		msg = &Ping{}
	case CmdAnnounceSignatures:
		msg = &AnnounceSignatures{}
	case CmdPong:
		msg = &Pong{}
	default:
		return nil, fmt.Errorf("unhandled command [%d]", command)
	}

	return msg, nil
}

// messageHeader represents the header structure for all lightning protocol
// messages.
type messageHeader struct {
	// magic represents Which Blockchain Technology(TM) to use.
	// NOTE(j): We don't need to worry about the magic overlapping with
	// bitcoin since this is inside encrypted comms anyway, but maybe we
	// should use the XOR (^wire.TestNet3) just in case???
	magic   wire.BitcoinNet // 4 bytes
	command uint32          // 4 bytes
	length  uint32          // 4 bytes
}

// readMessageHeader reads a lightning protocol message header from r.
func readMessageHeader(r io.Reader) (int, *messageHeader, error) {
	// As the message header is a fixed size structure, read bytes for the
	// entire header at once.
	var headerBytes [MessageHeaderSize]byte
	n, err := io.ReadFull(r, headerBytes[:])
	if err != nil {
		return n, nil, err
	}
	hr := bytes.NewReader(headerBytes[:])

	// Create and populate the message header from the raw header bytes.
	hdr := messageHeader{}
	err = readElements(hr,
		&hdr.magic,
		&hdr.command,
		&hdr.length)
	if err != nil {
		return n, nil, err
	}

	return n, &hdr, nil
}

// discardInput reads n bytes from reader r in chunks and discards the read
// bytes. This is used to skip payloads when various errors occur and helps
// prevent rogue nodes from causing massive memory allocation through forging
// header length.
func discardInput(r io.Reader, n uint32) {
	maxSize := uint32(10 * 1024) // 10k at a time
	numReads := n / maxSize
	bytesRemaining := n % maxSize
	if n > 0 {
		buf := make([]byte, maxSize)
		for i := uint32(0); i < numReads; i++ {
			io.ReadFull(r, buf)
		}
	}
	if bytesRemaining > 0 {
		buf := make([]byte, bytesRemaining)
		io.ReadFull(r, buf)
	}
}

// WriteMessage writes a lightning Message to w including the necessary header
// information and returns the number of bytes written.
func WriteMessage(w io.Writer, msg Message, pver uint32, btcnet wire.BitcoinNet) (int, error) {
	totalBytes := 0

	cmd := msg.Command()

	// Encode the message payload
	var bw bytes.Buffer
	err := msg.Encode(&bw, pver)
	if err != nil {
		return totalBytes, err
	}
	payload := bw.Bytes()
	lenp := len(payload)

	// Enforce maximum overall message payload
	if lenp > MaxMessagePayload {
		return totalBytes, fmt.Errorf("message payload is too large - "+
			"encoded %d bytes, but maximum message payload is %d bytes",
			lenp, MaxMessagePayload)
	}

	// Enforce maximum message payload on the message type
	mpl := msg.MaxPayloadLength(pver)
	if uint32(lenp) > mpl {
		return totalBytes, fmt.Errorf("message payload is too large - "+
			"encoded %d bytes, but maximum message payload of "+
			"type %x is %d bytes", lenp, cmd, mpl)
	}

	// Create header for the message.
	hdr := messageHeader{magic: btcnet, command: cmd, length: uint32(lenp)}

	// Encode the header for the message. This is done to a buffer
	// rather than directly to the writer since writeElements doesn't
	// return the number of bytes written.
	hw := bytes.NewBuffer(make([]byte, 0, MessageHeaderSize))
	if err := writeElements(hw, hdr.magic, hdr.command, hdr.length); err != nil {
		return 0, nil
	}

	// Write the header first.
	n, err := w.Write(hw.Bytes())
	totalBytes += n
	if err != nil {
		return totalBytes, err
	}

	// Write payload the payload itself after the header.
	n, err = w.Write(payload)
	totalBytes += n
	return totalBytes, err
}

// ReadMessage reads, validates, and parses the next bitcoin Message from r for
// the provided protocol version and bitcoin network.  It returns the number of
// bytes read in addition to the parsed Message and raw bytes which comprise the
// message.  This function is the same as ReadMessage except it also returns the
// number of bytes read.
func ReadMessage(r io.Reader, pver uint32, btcnet wire.BitcoinNet) (int, Message, []byte, error) {
	totalBytes := 0
	n, hdr, err := readMessageHeader(r)
	totalBytes += n
	if err != nil {
		return totalBytes, nil, nil, err
	}

	// Enforce maximum message payload
	if hdr.length > MaxMessagePayload {
		return totalBytes, nil, nil, fmt.Errorf("message payload is "+
			"too large - header indicates %d bytes, but max "+
			"message payload is %d bytes.", hdr.length,
			MaxMessagePayload)
	}

	// Check for messages in the wrong network.
	if hdr.magic != btcnet {
		discardInput(r, hdr.length)
		return totalBytes, nil, nil, fmt.Errorf("message from other "+
			"network [%v]", hdr.magic)
	}

	// Create struct of appropriate message type based on the command.
	command := hdr.command
	msg, err := makeEmptyMessage(command)
	if err != nil {
		discardInput(r, hdr.length)
		return totalBytes, nil, nil, &UnknownMessage{
			messageType: command,
		}
	}

	// Check for maximum length based on the message type.
	mpl := msg.MaxPayloadLength(pver)
	if hdr.length > mpl {
		discardInput(r, hdr.length)
		return totalBytes, nil, nil, fmt.Errorf("payload exceeds max "+
			"length. indicates %v bytes, but max of message type %v is %v.",
			hdr.length, command, mpl)
	}

	// Read payload.
	payload := make([]byte, hdr.length)
	n, err = io.ReadFull(r, payload)
	totalBytes += n
	if err != nil {
		return totalBytes, nil, nil, err
	}

	// Unmarshal message.
	pr := bytes.NewBuffer(payload)
	if err = msg.Decode(pr, pver); err != nil {
		return totalBytes, nil, nil, err
	}

	// Validate the data.
	if err = msg.Validate(); err != nil {
		return totalBytes, nil, nil, err
	}

	return totalBytes, msg, payload, nil
}
