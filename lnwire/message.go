// Code derived from https:// github.com/btcsuite/btcd/blob/master/wire/message.go
package lnwire

import (
	"bytes"
	"fmt"
	"io"

	"github.com/btcsuite/btcd/wire"
)

// 4-byte network + 4-byte message id + payload-length 4-byte
const MessageHeaderSize = 12

const MaxMessagePayload = 1024 * 1024 * 32 //  32MB

const (
	// Funding channel open
	CmdFundingRequest      = uint32(200)
	CmdFundingResponse     = uint32(210)
	CmdFundingSignAccept   = uint32(220)
	CmdFundingSignComplete = uint32(230)

	// Close channel
	CmdCloseRequest  = uint32(300)
	CmdCloseComplete = uint32(310)

	// TODO Renumber to 1100
	// HTLC payment
	CmdHTLCAddRequest = uint32(1000)
	CmdHTLCAddAccept  = uint32(1010)
	CmdHTLCAddReject  = uint32(1020)

	// TODO Renumber to 1200
	// HTLC settlement
	CmdHTLCSettleRequest = uint32(1100)
	CmdHTLCSettleAccept  = uint32(1110)

	// HTLC timeout
	CmdHTLCTimeoutRequest = uint32(1300)
	CmdHTLCTimeoutAccept  = uint32(1310)

	// Commitments
	CmdCommitSignature  = uint32(2000)
	CmdCommitRevocation = uint32(2010)

	// Error
	CmdErrorGeneric = uint32(4000)
)

// Every message has these functions:
type Message interface {
	Decode(io.Reader, uint32) error // (io, protocol version)
	Encode(io.Writer, uint32) error // (io, protocol version)
	Command() uint32                // returns ID of the message
	MaxPayloadLength(uint32) uint32 // (version) maxpayloadsize
	Validate() error                // Validates the data struct
	String() string
}

func makeEmptyMessage(command uint32) (Message, error) {
	var msg Message

	switch command {
	case CmdFundingRequest:
		msg = &FundingRequest{}
	case CmdFundingResponse:
		msg = &FundingResponse{}
	case CmdFundingSignAccept:
		msg = &FundingSignAccept{}
	case CmdFundingSignComplete:
		msg = &FundingSignComplete{}
	case CmdCloseRequest:
		msg = &CloseRequest{}
	case CmdCloseComplete:
		msg = &CloseComplete{}
	case CmdHTLCAddRequest:
		msg = &HTLCAddRequest{}
	case CmdHTLCAddAccept:
		msg = &HTLCAddAccept{}
	case CmdHTLCAddReject:
		msg = &HTLCAddReject{}
	case CmdHTLCSettleRequest:
		msg = &HTLCSettleRequest{}
	case CmdHTLCSettleAccept:
		msg = &HTLCSettleAccept{}
	case CmdHTLCTimeoutRequest:
		msg = &HTLCTimeoutRequest{}
	case CmdHTLCTimeoutAccept:
		msg = &HTLCTimeoutAccept{}
	case CmdCommitSignature:
		msg = &CommitSignature{}
	case CmdCommitRevocation:
		msg = &CommitRevocation{}
	case CmdErrorGeneric:
		msg = &ErrorGeneric{}
	default:
		return nil, fmt.Errorf("unhandled command [%d]", command)
	}

	return msg, nil
}

type messageHeader struct {
	// NOTE(j): We don't need to worry about the magic overlapping with
	// bitcoin since this is inside encrypted comms anyway, but maybe we
	// should use the XOR (^wire.TestNet3) just in case???
	magic   wire.BitcoinNet // which Blockchain Technology(TM) to use
	command uint32
	length  uint32
}

func readMessageHeader(r io.Reader) (int, *messageHeader, error) {
	var headerBytes [MessageHeaderSize]byte
	n, err := io.ReadFull(r, headerBytes[:])
	if err != nil {
		return n, nil, err
	}
	hr := bytes.NewReader(headerBytes[:])

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

//  discardInput reads n bytes from reader r in chunks and discards the read
//  bytes.  This is used to skip payloads when various errors occur and helps
//  prevent rogue nodes from causing massive memory allocation through forging
//  header length.
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
		return totalBytes, fmt.Errorf("message payload is too large - encoded %d bytes, but maximum message payload is %d bytes", lenp, MaxMessagePayload)
	}

	// Enforce maximum message payload on the message type
	mpl := msg.MaxPayloadLength(pver)
	if uint32(lenp) > mpl {
		return totalBytes, fmt.Errorf("message payload is too large - encoded %d bytes, but maximum message payload of type %x is %d bytes", lenp, cmd, mpl)
	}

	// Create header for the message
	hdr := messageHeader{}
	hdr.magic = btcnet
	hdr.command = cmd
	hdr.length = uint32(lenp)

	//  Encode the header for the message.  This is done to a buffer
	//  rather than directly to the writer since writeElements doesn't
	//  return the number of bytes written.
	hw := bytes.NewBuffer(make([]byte, 0, MessageHeaderSize))
	writeElements(hw, hdr.magic, hdr.command, hdr.length)

	// Write header
	n, err := w.Write(hw.Bytes())
	totalBytes += n
	if err != nil {
		return totalBytes, err
	}

	// Write payload
	n, err = w.Write(payload)
	totalBytes += n
	if err != nil {
		return totalBytes, err
	}

	return totalBytes, nil
}

func ReadMessage(r io.Reader, pver uint32, btcnet wire.BitcoinNet) (int, Message, []byte, error) {
	totalBytes := 0
	n, hdr, err := readMessageHeader(r)
	totalBytes += n
	if err != nil {
		return totalBytes, nil, nil, err
	}

	// Enforce maximum message payload
	if hdr.length > MaxMessagePayload {
		return totalBytes, nil, nil, fmt.Errorf("message payload is too large - header indicates %d bytes, but max message payload is %d bytes.", hdr.length, MaxMessagePayload)
	}

	// Check for messages in the wrong bitcoin network
	if hdr.magic != btcnet {
		discardInput(r, hdr.length)
		return totalBytes, nil, nil, fmt.Errorf("message from other network [%v]", hdr.magic)
	}

	// Create struct of appropriate message type based on the command
	command := hdr.command
	msg, err := makeEmptyMessage(command)
	if err != nil {
		discardInput(r, hdr.length)
		return totalBytes, nil, nil, fmt.Errorf("ReadMessage %s", err.Error())
	}

	// Check for maximum length based on the message type
	mpl := msg.MaxPayloadLength(pver)
	if hdr.length > mpl {
		discardInput(r, hdr.length)
		return totalBytes, nil, nil, fmt.Errorf("payload exceeds max length. indicates %v bytes, but max of message type %v is %v.", hdr.length, command, mpl)
	}

	// Read payload
	payload := make([]byte, hdr.length)
	n, err = io.ReadFull(r, payload)
	totalBytes += n
	if err != nil {
		return totalBytes, nil, nil, err
	}

	// Unmarshal message
	pr := bytes.NewBuffer(payload)
	err = msg.Decode(pr, pver)
	if err != nil {
		return totalBytes, nil, nil, err
	}

	// Validate the data
	err = msg.Validate()
	if err != nil {
		return totalBytes, nil, nil, err
	}

	// We're good!
	return totalBytes, msg, payload, nil
}
