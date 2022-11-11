package hop

import (
	"github.com/btcsuite/btcd/btcec/v2"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/keychain"
)

type BlindHopProcessor struct{}

func NewBlindHopProcessor() *BlindHopProcessor {
	return &BlindHopProcessor{}
}

func (b *BlindHopProcessor) DecryptBlindedPayload(nodeID keychain.SingleKeyECDH,
	blindingPoint *btcec.PublicKey, payload []byte) ([]byte, error) {

	return sphinx.DecryptBlindedData(
		nodeID, blindingPoint, payload,
	)
}

func (b *BlindHopProcessor) NextBlindingPoint(nodeID keychain.SingleKeyECDH,
	blindingPoint *btcec.PublicKey) (*btcec.PublicKey, error) {

	// NOTE(8/8/22): We have pulled the sphinx dependency
	// out of 'htlcswitch' only to depend on it here?
	// To what end?
	return sphinx.NextEphemeral(nodeID, blindingPoint)
}
