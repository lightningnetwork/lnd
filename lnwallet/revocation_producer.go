//go:build !integration

package lnwallet

import (
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/shachain"
)

// nextRevocationProducer creates a new revocation producer, deriving the
// revocation root by applying ECDH to a new key from our revocation root
// family and the multisig key we use for the channel. For taproot channels a
// related shachain revocation root is also returned.
func (l *LightningWallet) nextRevocationProducer(res *ChannelReservation,
	keyRing keychain.KeyRing,
) (shachain.Producer, shachain.Producer, error) {

	// Derive the next key in the revocation root family.
	nextRevocationKeyDesc, err := keyRing.DeriveNextKey(
		keychain.KeyFamilyRevocationRoot,
	)
	if err != nil {
		return nil, nil, err
	}

	// If the DeriveNextKey call returns the first key with Index 0, we need
	// to re-derive the key as the keychain/btcwallet.go DerivePrivKey call
	// special-cases Index 0.
	if nextRevocationKeyDesc.Index == 0 {
		nextRevocationKeyDesc, err = keyRing.DeriveNextKey(
			keychain.KeyFamilyRevocationRoot,
		)
		if err != nil {
			return nil, nil, err
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
		return nil, nil, err
	}

	// Once we have the root, we can then generate our shachain producer
	// and from that generate the per-commitment point.
	shaChainRoot := shachain.NewRevocationProducer(revRoot)
	taprootShaChainRoot, err := channeldb.DeriveMusig2Shachain(shaChainRoot)
	if err != nil {
		return nil, nil, err
	}

	return shaChainRoot, taprootShaChainRoot, nil
}
