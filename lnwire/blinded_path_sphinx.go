package lnwire

import (
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	sphinx "github.com/lightningnetwork/lightning-onion"
)

// NewBlindedPathFromSphinx rebuilds a sphinx-form blinded path as the lnwire
// equivalent. A nil path yields a nil path and no error.
func NewBlindedPathFromSphinx(p *sphinx.BlindedPath) (*BlindedPath, error) {
	if p == nil {
		return nil, nil
	}

	hops := make([]BlindedHop, len(p.BlindedHops))
	for i, h := range p.BlindedHops {
		hops[i].BlindedNodeID = h.BlindedNodePub
		hops[i].EncryptedData = h.CipherText
	}

	introNode, err := NewPubkeyIntro(p.IntroductionPoint)
	if err != nil {
		return nil, err
	}

	return &BlindedPath{
		IntroductionNode: introNode,
		BlindingPoint:    p.BlindingPoint,
		Hops:             hops,
	}, nil
}

// ToSphinx assembles the sphinx-form representation used by the routing layer.
// It accepts only a pubkey-form introduction node, because the sciddir form
// needs a channel-graph lookup.
func (p *BlindedPath) ToSphinx() (*sphinx.BlindedPath, error) {
	if p.IntroductionNode == nil {
		return nil, fmt.Errorf("nil intro node")
	}

	introPub, err := introductionNodePubkey(p.IntroductionNode)
	if err != nil {
		return nil, err
	}

	if p.BlindingPoint == nil {
		return nil, fmt.Errorf("nil blinding point")
	}

	hops := make([]*sphinx.BlindedHopInfo, len(p.Hops))
	for i := range p.Hops {
		if p.Hops[i].BlindedNodeID == nil {
			return nil, fmt.Errorf("nil blinded hop %d", i)
		}

		hops[i] = &sphinx.BlindedHopInfo{
			BlindedNodePub: p.Hops[i].BlindedNodeID,
			CipherText:     p.Hops[i].EncryptedData,
		}
	}

	return &sphinx.BlindedPath{
		IntroductionPoint: introPub,
		BlindingPoint:     p.BlindingPoint,
		BlindedHops:       hops,
	}, nil
}

// introductionNodePubkey extracts the key of a pubkey-form introduction
// node. It returns an error for any other form.
func introductionNodePubkey(intro IntroductionNode) (*btcec.PublicKey, error) {
	switch v := intro.(type) {
	case PubkeyIntro:
		if v.Pubkey == nil {
			return nil, fmt.Errorf("%w: nil pubkey",
				ErrInvalidIntroNode)
		}

		return v.Pubkey, nil

	case SciddirIntro:
		return nil, fmt.Errorf("sciddir intro is not supported")

	default:
		return nil, fmt.Errorf("%w: %T", ErrInvalidIntroNode, intro)
	}
}
