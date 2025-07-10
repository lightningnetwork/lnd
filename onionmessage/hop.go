package onionmessage

import (
	"bytes"

	"github.com/btcsuite/btcd/btcec/v2"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/tlv"
)

// forwardAction contains the information needed to forward an onion message to
// the next node as well as update any subscribers with the payload we received.
type forwardAction struct {
	// nextNodeID is the public key of the peer to forward the message to
	nextNodeID *btcec.PublicKey

	// nextPathKey is the path key for the next hop, used for route
	// blinding.
	nextPathKey *btcec.PublicKey

	// nextPacket is the serialized onion packet to send to the next hop.
	nextPacket []byte

	// payload contains the decoded payload for this hop, which may include
	// custom records and routing information.
	payload *lnwire.OnionMessagePayload
}

// deliverAction contains the information needed to deliver the payload to any
// subscribers. Since we only support forwarding onion messages, this is only
// needed in itest to verify correct handling and behavior.
type deliverAction struct {
	// payload contains the decoded payload for this hop, which may include
	// custom records and routing information.
	payload *lnwire.OnionMessagePayload
}

type routingAction = fn.Either[forwardAction, deliverAction]

// NodeIDResolver defines an interface to resolve a node public key from a short
// channel ID.
type NodeIDResolver interface {
	RemotePubFromSCID(scid lnwire.ShortChannelID) (*btcec.PublicKey, error)
}

// processOnionMessage decodes and processes an onion message packet and its
// contents. It assumes route blinding is used, so it also decrypts encrypted
// recipient data, and derives the next path key. It returns a fn.Result type
// containing a routingAction, which contains all the information required to
// execute the next step in the routing process.
func processOnionMessage(router *sphinx.Router, resolver NodeIDResolver,
	msg *lnwire.OnionMessage) fn.Result[routingAction] {

	var onionPkt sphinx.OnionPacket
	err := onionPkt.Decode(bytes.NewReader(msg.OnionBlob))
	if err != nil {
		return fn.Err[routingAction](err)
	}

	// TODO(gijs): We should not use the magic value 10 here. It's the
	// incomingCltv value and only has use for the replay protection that we
	// don't need anyway.
	processedPkt, err := router.ProcessOnionPacket(
		&onionPkt, nil, 10, sphinx.WithBlindingPoint(msg.PathKey),
	)
	if err != nil {
		return fn.Err[routingAction](err)
	}

	payload := lnwire.NewOnionMessagePayload()
	payload, _, err = payload.Decode(
		bytes.NewReader(processedPkt.Payload.Payload),
	)
	if err != nil {
		return fn.Err[routingAction](err)
	}

	// Create a shallow copy of the payload but deep copy the EncryptedData
	// field, as the decryption below will overwrite the EncryptedData field
	// in-place.
	originalPayload := *payload
	originalPayload.EncryptedData = bytes.Clone(payload.EncryptedData)

	decrypted, err := router.DecryptBlindedHopData(
		msg.PathKey, payload.EncryptedData,
	)
	if err != nil {
		return fn.Err[routingAction](err)
	}

	routeData, err := record.DecodeBlindedRouteData(
		bytes.NewReader(decrypted),
	)
	if err != nil {
		return fn.Err[routingAction](err)
	}

	nextPathKey := deriveNextPathKey(router, msg.PathKey,
		routeData.NextBlindingOverride)

	action, err := createRoutingAction(
		resolver, processedPkt, &originalPayload, routeData,
		nextPathKey,
	)
	if err != nil {
		return fn.Err[routingAction](err)
	}

	return fn.Ok(action)
}

// createRoutingAction creates the routing action based on whether we are
// forwarding or the receiver of the onion message.
func createRoutingAction(resolver NodeIDResolver,
	packet *sphinx.ProcessedPacket, payload *lnwire.OnionMessagePayload,
	routeData *record.BlindedRouteData,
	nextPathKey *btcec.PublicKey) (routingAction, error) {

	if isForwarding(packet) {
		var nextNodeID *btcec.PublicKey
		if routeData.NextNodeID.IsSome() {
			n, err := routeData.NextNodeID.UnwrapOrErr(
				ErrNextNodeIdEmpty,
			)
			if err != nil {
				return routingAction{}, err
			}
			nextNodeID = n.Val
		} else {
			scid, err := routeData.ShortChannelID.UnwrapOrErr(
				ErrNextNodeIdEmpty,
			)
			if err != nil {
				return routingAction{}, err
			}
			nextNodeID, err = resolver.RemotePubFromSCID(scid.Val)
			if err != nil {
				return routingAction{}, err
			}
		}

		buf := new(bytes.Buffer)
		err := packet.NextPacket.Encode(buf)
		if err != nil {
			return routingAction{}, err
		}
		nextPacket := buf.Bytes()

		return fn.NewLeft[forwardAction, deliverAction](forwardAction{
			nextNodeID:  nextNodeID,
			nextPathKey: nextPathKey,
			nextPacket:  nextPacket,
			payload:     payload,
		}), nil
	}

	return fn.NewRight[forwardAction](deliverAction{
		payload: payload,
	}), nil
}

// deriveNextPathKey derives the next path key using the router and current
// path key. If an override is provided, it is used instead.
func deriveNextPathKey(router *sphinx.Router, currentPathKey *btcec.PublicKey,
	override tlv.OptionalRecordT[tlv.TlvType8,
		*btcec.PublicKey]) *btcec.PublicKey {

	// If an override is provided, use it.
	return override.UnwrapOrFunc(func() tlv.RecordT[tlv.TlvType8,
		*btcec.PublicKey] {

		// Otherwise, derive the next path key using the router.
		nextKey, err := router.NextEphemeral(currentPathKey)
		if err != nil {
			// If the derivation fails, return a zero key.
			return override.Zero()
		}

		return tlv.NewPrimitiveRecord[tlv.TlvType8](nextKey)
	}).Val
}

// isForwarding checks if the packet is to be forwarded or delivered.
func isForwarding(packet *sphinx.ProcessedPacket) bool {
	return packet.Action != sphinx.ExitNode
}
