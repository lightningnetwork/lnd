package onionmessage

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/record"
)

// ErrPathEndsAtSelf is returned when every hop of a blinded path belongs to
// this node, so no other node is left to send to.
var ErrPathEndsAtSelf = errors.New("blinded path ends at this node")

// HopPeeler removes the hops of this node at the start of a blinded path.
// A sender calls it before it builds an onion message on a path whose
// introduction node is this node, because no peer connection leads to the own
// node.
type HopPeeler struct {
	router   OnionRouter
	resolver NodeIDResolver
	ourPub   *btcec.PublicKey
}

// NewHopPeeler creates a HopPeeler for the node with the key ourPub. The
// router must decrypt with the private key of ourPub.
func NewHopPeeler(router OnionRouter, resolver NodeIDResolver,
	ourPub *btcec.PublicKey) *HopPeeler {

	return &HopPeeler{
		router:   router,
		resolver: resolver,
		ourPub:   ourPub,
	}
}

// PeelOwnHops returns path without the hops of this node at its start. It
// returns path as it is when its introduction node is another node. It does
// not change path.
func (p *HopPeeler) PeelOwnHops(ctx context.Context,
	path *sphinx.BlindedPath) (*sphinx.BlindedPath, error) {

	for path.IntroductionPoint.IsEqual(p.ourPub) {
		if len(path.BlindedHops) < 2 {
			return nil, ErrPathEndsAtSelf
		}

		next, err := p.peel(ctx, path)
		if err != nil {
			return nil, err
		}
		path = next
	}

	return path, nil
}

// peel removes the first hop of path, which belongs to this node.
func (p *HopPeeler) peel(ctx context.Context,
	path *sphinx.BlindedPath) (*sphinx.BlindedPath, error) {

	// The decryption works in place, and the caller still owns path.
	cipherText := bytes.Clone(path.BlindedHops[0].CipherText)
	plainText, err := p.router.DecryptBlindedHopData(
		path.BlindingPoint, cipherText,
	)
	if err != nil {
		return nil, fmt.Errorf("decrypt own hop: %w", err)
	}

	routeData, err := record.DecodeBlindedRouteData(
		bytes.NewReader(plainText),
	)
	if err != nil {
		return nil, fmt.Errorf("decode own hop: %w", err)
	}

	nextNode, err := nextNodeID(ctx, p.resolver, routeData)
	if err != nil {
		return nil, fmt.Errorf("next node of own hop: %w", err)
	}

	nextPathKey := deriveNextPathKey(
		p.router, path.BlindingPoint, routeData.NextBlindingOverride,
	)
	if nextPathKey == nil {
		return nil, errors.New("derive the next path key")
	}

	return &sphinx.BlindedPath{
		IntroductionPoint: nextNode,
		BlindingPoint:     nextPathKey,
		BlindedHops:       path.BlindedHops[1:],
	}, nil
}
