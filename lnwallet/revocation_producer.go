//go:build !integration

package lnwallet

import (
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/shachain"
)

// nextRevocationProducer creates a new revocation producer, deriving the
// revocation root by applying ECDH to a new key from our revocation root family
// and the multisig key we use for the channel.
func (l *LightningWallet) nextRevocationProducer(res *ChannelReservation,
	keyRing keychain.KeyRing) (shachain.Producer, error) {

	// Derive the next key in the revocation root family.
	nextRevocationKeyDesc, err := keyRing.DeriveNextKey(
		keychain.KeyFamilyRevocationRoot,
	)
	if err != nil {
		return nil, err
	}

	// If the DeriveNextKey call returns the first key with Index 0, we need
	// to re-derive the key as the keychain/btcwallet.go DerivePrivKey call
	// special-cases Index 0.
	if nextRevocationKeyDesc.Index == 0 {
		nextRevocationKeyDesc, err = keyRing.DeriveNextKey(
			keychain.KeyFamilyRevocationRoot,
		)
		if err != nil {
			return nil, err
		}
	}

	res.nextRevocationKeyLoc = nextRevocationKeyDesc.KeyLocator

	// Perform an ECDH operation between the private key described in
	// nextRevocationKeyDesc and our public multisig key. The result will be
	// used to seed the revocation producer.
	revRoot, err := l.ECDH(
		nextRevocationKeyDesc, res.ourContribution.MultiSigKey.PubKey,
	)
	if err != nil {
		return nil, err
	}

	// Once we have the root, we can then generate our shachain producer
	// and from that generate the per-commitment point.
	return shachain.NewRevocationProducer(revRoot), nil
}
