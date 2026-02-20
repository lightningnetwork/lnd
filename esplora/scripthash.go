package esplora

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
)

// ScripthashFromScript converts a pkScript (output script) to a scripthash.
// The scripthash is the SHA256 hash of the script with the bytes reversed
// (displayed in little-endian order).
func ScripthashFromScript(pkScript []byte) string {
	hash := sha256.Sum256(pkScript)

	// Reverse the hash bytes for Esplora's format.
	reversed := make([]byte, len(hash))
	for i := 0; i < len(hash); i++ {
		reversed[i] = hash[len(hash)-1-i]
	}

	return hex.EncodeToString(reversed)
}

// ScripthashFromAddress converts a Bitcoin address to a scripthash.
// This creates the appropriate pkScript for the address type and then computes
// the scripthash.
func ScripthashFromAddress(address string,
	params *chaincfg.Params) (string, error) {

	addr, err := btcutil.DecodeAddress(address, params)
	if err != nil {
		return "", fmt.Errorf("failed to decode address: %w", err)
	}

	pkScript, err := txscript.PayToAddrScript(addr)
	if err != nil {
		return "", fmt.Errorf("failed to create pkScript: %w", err)
	}

	return ScripthashFromScript(pkScript), nil
}

// ScripthashFromAddressUnchecked converts a Bitcoin address to a scripthash
// without network validation. This is useful when the network parameters are
// not available but the address format is known to be valid.
func ScripthashFromAddressUnchecked(address string) (string, error) {
	// Try mainnet first, then testnet, then regtest.
	networks := []*chaincfg.Params{
		&chaincfg.MainNetParams,
		&chaincfg.TestNet3Params,
		&chaincfg.RegressionNetParams,
		&chaincfg.SigNetParams,
	}

	for _, params := range networks {
		scripthash, err := ScripthashFromAddress(address, params)
		if err == nil {
			return scripthash, nil
		}
	}

	return "", fmt.Errorf("failed to decode address on any network: %s",
		address)
}

// ReverseBytes reverses a byte slice in place and returns it.
func ReverseBytes(b []byte) []byte {
	for i, j := 0, len(b)-1; i < j; i, j = i+1, j-1 {
		b[i], b[j] = b[j], b[i]
	}
	return b
}

// ReversedHash returns a copy of the hash with bytes reversed. This is useful
// for converting between internal byte order and display order.
func ReversedHash(hash []byte) []byte {
	reversed := make([]byte, len(hash))
	for i := 0; i < len(hash); i++ {
		reversed[i] = hash[len(hash)-1-i]
	}
	return reversed
}
