package lnencrypt

import (
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/keychain"
)

var (
	testWalletPrivKey = []byte{
		0x2b, 0xd8, 0x06, 0xc9, 0x7f, 0x0e, 0x00, 0xaf,
		0x1a, 0x1f, 0xc3, 0x32, 0x8f, 0xa7, 0x63, 0xa9,
		0x26, 0x97, 0x23, 0xc8, 0xdb, 0x8f, 0xac, 0x4f,
		0x93, 0xaf, 0x71, 0xdb, 0x18, 0x6d, 0x6e, 0x90,
	}
)

type MockKeyRing struct {
	Fail bool
}

func (m *MockKeyRing) DeriveNextKey(
	keyFam keychain.KeyFamily) (keychain.KeyDescriptor, error) {

	return keychain.KeyDescriptor{}, nil
}

func (m *MockKeyRing) DeriveKey(
	keyLoc keychain.KeyLocator) (keychain.KeyDescriptor, error) {

	if m.Fail {
		return keychain.KeyDescriptor{}, fmt.Errorf("fail")
	}

	_, pub := btcec.PrivKeyFromBytes(testWalletPrivKey)
	return keychain.KeyDescriptor{
		PubKey: pub,
	}, nil
}
