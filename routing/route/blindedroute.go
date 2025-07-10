package route

import (
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/lnwire"
)

// OnionMessageBlindedPathToSphinxPath converts a complete blinded path intended
// for sending an onion message to a PaymentPath that contains the per-hop
// payloads used to encoding the routing data for each hop in the route. This
// method also accepts an optional EOB payload for the final hop.
func OnionMessageBlindedPathToSphinxPath(blindedPath *sphinx.BlindedPath,
	replyPath *lnwire.ReplyPath, finalPayloads []*lnwire.FinalHopPayload) (
	*sphinx.PaymentPath, error) {

	var path sphinx.PaymentPath

	// We can only construct a route if there are hops provided.
	if len(blindedPath.BlindedHops) == 0 {
		return nil, ErrNoRouteHopsProvided
	}

	// Check maximum route length. We keep the maximum the same as
	// sphinx.NumMaxHops for simplicity. In theory the maximum for onion
	// messages could be higher, namely 481. See:
	// https://delvingbitcoin.org/t/onion-messaging-dos-threat-mitigations
	if len(blindedPath.BlindedHops) > sphinx.NumMaxHops {
		return nil, ErrMaxRouteHopsExceeded
	}

	// For each hop encoded within the route, we'll convert the hop struct
	// to an OnionHop with matching per-hop payload within the path as used
	// by the sphinx package.
	for i, hop := range blindedPath.BlindedHops {
		// Create an onionMessagePayload with the encrypted data for
		// this hop.
		onionMessagePayload := &lnwire.OnionMessagePayload{
			EncryptedData: hop.CipherText,
		}

		// If we're on the final hop include the tlvs intended for the
		// final hop and the reply path (if provided).
		finalHop := i == len(blindedPath.BlindedHops)-1
		if finalHop {
			onionMessagePayload.FinalHopPayloads = finalPayloads
			onionMessagePayload.ReplyPath = replyPath
		}

		// create a sphinx hop for this blinded hop.
		hop, err := createSphinxHop(
			*hop.BlindedNodePub, onionMessagePayload,
		)
		if err != nil {
			return nil, fmt.Errorf("sphinx hop %v: %w", i, err)
		}
		path[i] = *hop
	}

	return &path, nil
}

// createSphinxHop encodes an onion message payload and produces a sphinx
// onion hop for it.
func createSphinxHop(nodeID btcec.PublicKey,
	onionMessagePayload *lnwire.OnionMessagePayload) (*sphinx.OnionHop,
	error) {

	encodeOnionMessagePayload, err := onionMessagePayload.Encode()
	if err != nil {
		return nil, fmt.Errorf("failed onion message payload encode: "+
			"%w", err)
	}

	hopPayload, err := sphinx.NewTLVHopPayload(encodeOnionMessagePayload)
	if err != nil {
		return nil, fmt.Errorf("failed creation of tlv hop payload: "+
			"%w", err)
	}

	return &sphinx.OnionHop{
		NodePub:    nodeID,
		HopPayload: hopPayload,
	}, nil
}
