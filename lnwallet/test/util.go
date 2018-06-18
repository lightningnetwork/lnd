package test

import (
	"encoding/hex"

	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcutil"
)

// PubkeyFromHex parses a Bitcoin public key from a hex encoded string.
func PubkeyFromHex(keyHex string) (*btcec.PublicKey, error) {
	bytes, err := hex.DecodeString(keyHex)
	if err != nil {
		return nil, err
	}
	return btcec.ParsePubKey(bytes, btcec.S256())
}

// PrivkeyFromHex parses a Bitcoin private key from a hex encoded string.
func PrivkeyFromHex(keyHex string) (*btcec.PrivateKey, error) {
	bytes, err := hex.DecodeString(keyHex)
	if err != nil {
		return nil, err
	}
	key, _ := btcec.PrivKeyFromBytes(btcec.S256(), bytes)
	return key, nil

}

// PubkeyToHex serializes a Bitcoin public key to a hex encoded string.
func PubkeyToHex(key *btcec.PublicKey) string {
	return hex.EncodeToString(key.SerializeCompressed())
}

// PrivkeyToHex serializes a Bitcoin private key to a hex encoded string.
func PrivkeyToHex(key *btcec.PrivateKey) string {
	return hex.EncodeToString(key.Serialize())
}

// SignatureFromHex parses a Bitcoin signature from a hex encoded string.
func SignatureFromHex(sigHex string) (*btcec.Signature, error) {
	bytes, err := hex.DecodeString(sigHex)
	if err != nil {
		return nil, err
	}
	return btcec.ParseSignature(bytes, btcec.S256())
}

// BlockFromHex parses a full Bitcoin block from a hex encoded string.
func BlockFromHex(blockHex string) (*btcutil.Block, error) {
	bytes, err := hex.DecodeString(blockHex)
	if err != nil {
		return nil, err
	}
	return btcutil.NewBlockFromBytes(bytes)
}

// TxFromHex parses a full Bitcoin transaction from a hex encoded string.
func TxFromHex(txHex string) (*btcutil.Tx, error) {
	bytes, err := hex.DecodeString(txHex)
	if err != nil {
		return nil, err
	}
	return btcutil.NewTxFromBytes(bytes)
}
