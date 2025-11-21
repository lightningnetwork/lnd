package onionmessage

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"github.com/btcsuite/btcd/btcec/v2"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/tlv"
)

// Payload encapsulates all information delivered to a hop in an
// onion_message_packet payload. A Hop for onion messaging _by definition_
// represents a TLV payload. The primary forwarding instruction can be accessed
// via ForwardingInfo, and custom records can be accessed by other member
// functions.
type Payload struct {
	// customRecords are user-defined records in the custom type range that
	// were included in the payload.
	customRecords record.CustomSet

	// encryptedData is a blob of data encrypted by the receiver for use
	// in blinded routes.
	encryptedData []byte

	// replyPath is the blinded route a reply to an onion message should
	// take to receive the sender of the original onion message.
	replyPath *lnwire.ReplyPath
}

// handleOnionMessage decodes and processes an onion message.
func processOnionMessage(router *sphinx.Router,
	msg lnwire.OnionMessage) (*Payload, *btcec.PublicKey, *btcec.PublicKey,
	[]byte, error) {

	var onionPkt sphinx.OnionPacket

	err := onionPkt.Decode(bytes.NewReader(msg.OnionBlob))
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("could not decode onion packet: %w", err)
	}

	blindingPoint := sphinx.WithBlindingPoint(msg.PathKey)
	processedPkt, err := router.ProcessOnionPacket(
		&onionPkt, nil, 10, blindingPoint,
	)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("%w: could not process onion packet: %w",
			ErrBadOnionBlob, err)
	}

	payload, err := ParseTLVPayloadOnionMessage(
		bytes.NewReader(processedPkt.Payload.Payload),
	)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("could not parse onion message "+
			"payload: %w", err)
	}

	// if we are the exit node, return the payload.
	//
	// TODO(gijs): We return the payload here for now, so that we can run
	// itests that check the final payload for correctness. But if we *only*
	// support forwarding, we should probably return an error here.
	if processedPkt.Action == sphinx.ExitNode {
		return payload, nil, nil, nil, nil
	}

	//  If the Action field indicates a failure occurred during packet
	//  processing, we return an error. This check is probably superfluous
	//  here, as we would expect an error to be returned by
	//  ProcessOnionPacket above in such cases.
	if processedPkt.Action == sphinx.Failure {
		return nil, nil, nil, nil, fmt.Errorf("%w: could not process onion packet: %w",
			ErrBadOnionBlob, err)

	}

	// Otherwise, we must be a forwarding node.
	decrypted, err := router.DecryptBlindedHopData(
		msg.PathKey, payload.encryptedData,
	)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("decrypt blinded data: %w", err)
	}

	r := bytes.NewReader(decrypted)
	routeData, err := record.DecodeBlindedRouteData(r)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("decode blinded route data: %w", err)
	}

	//TODO(gijs): Show we validate blinded features here, like we do in
	//htlcswitch/hop/payload.go

	nextEph, err := routeData.NextBlindingOverride.UnwrapOrFuncErr(
		func() (tlv.RecordT[tlv.TlvType8, *btcec.PublicKey], error) {
			next, err := router.NextEphemeral(
				msg.PathKey,
			)
			if err != nil {
				return routeData.NextBlindingOverride.Zero(),
					err
			}

			return tlv.NewPrimitiveRecord[tlv.TlvType8](next), nil
		})
	if err != nil {
		return nil, nil, nil, nil, err
	}

	nextNodeID, err := routeData.NextNodeID.UnwrapOrErr(
		errors.New("next node ID empty"),
	)

	if err != nil {
		return nil, nil, nil, nil, err
	}

	buf := new(bytes.Buffer)

	err = processedPkt.NextPacket.Encode(buf)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("could not encode next packet: %w", err)
	}
	nextPacket := buf.Bytes()

	return payload, nextNodeID.Val, nextEph.Val, nextPacket, nil
}

// ParseTLVPayloadOnionMessage builds a new Hop from the passed io.Reader and
// returns a map of all the types that were found in the onion message payload.
// This function does not perform validation of TLV types included in the
// payload.
func ParseTLVPayloadOnionMessage(r io.Reader) (*Payload, error) {

	onionMsgPayload := lnwire.NewOnionMessagePayload()
	onionMsgPayload, _, err := onionMsgPayload.Decode(r)
	if err != nil {
		return nil, err
	}

	customRecords := make(record.CustomSet)
	for _, v := range onionMsgPayload.FinalHopPayloads {
		customRecords[uint64(v.TLVType)] = v.Value
	}

	return &Payload{
		replyPath:     onionMsgPayload.ReplyPath,
		encryptedData: onionMsgPayload.EncryptedData,
		customRecords: customRecords,
	}, nil
}
