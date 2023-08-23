//go:build integration

package lnwallet

import (
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/shachain"
)

// nextRevocationProducer creates a new revocation producer, deriving the
// revocation root by applying ECDH to a new key from our revocation root family
// and the multisig key we use for the channel.
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

	// Within our itests, we want to make sure we can still restore channel
	// backups created with the old revocation root derivation method. To
	// create a channel in the legacy format during the test, we signal this
	// by setting an explicit pending channel ID. The ID is the hex
	// representation of the string "legacy-revocation".
	itestLegacyFormatChanID := [32]byte{
		0x6c, 0x65, 0x67, 0x61, 0x63, 0x79, 0x2d, 0x72, 0x65, 0x76,
		0x6f, 0x63, 0x61, 0x74, 0x69, 0x6f, 0x6e,
	}
	if res.pendingChanID == itestLegacyFormatChanID {
		revocationRoot, err := l.DerivePrivKey(nextRevocationKeyDesc)
		if err != nil {
			return nil, nil, err
		}

		// Once we have the root, we can then generate our shachain
		// producer and from that generate the per-commitment point.
		revRoot, err := chainhash.NewHash(revocationRoot.Serialize())
		if err != nil {
			return nil, nil, err
		}
		shaChainRoot := shachain.NewRevocationProducer(*revRoot)

		taprootShaChainRoot, err := channeldb.DeriveMusig2Shachain(
			shaChainRoot,
		)
		if err != nil {
			return nil, nil, err
		}

		return shaChainRoot, taprootShaChainRoot, nil
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
	taprootShaChainRoot, err := channeldb.DeriveMusig2Shachain(
		shaChainRoot,
	)
	if err != nil {
		return nil, nil, err
	}

	return shaChainRoot, taprootShaChainRoot, nil
}
