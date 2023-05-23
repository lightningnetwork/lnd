package zpay32

import (
	"strings"
	"testing"

	"github.com/btcsuite/btcd/chaincfg"
)

func FuzzDecode(f *testing.F) {
	f.Fuzz(func(t *testing.T, data string) {
		_, _ = Decode(data, &chaincfg.TestNet3Params)
	})
}

// appendChecksum returns a string containing bech followed by its bech32
// checksum if a checksum could be calculated. Otherwise, the function returns
// bech unchanged.
//
// This code is based on checksum calculation in zpay32/bech32.go.
func appendChecksum(bech string) string {
	lower := strings.ToLower(bech)

	// The string is invalid if the last '1' is non-existent or it is the
	// first character of the string (no human-readable part).
	one := strings.LastIndexByte(lower, '1')
	if one < 1 {
		return bech
	}
	hrp := lower[:one]
	data := lower[one+1:]

	decoded, err := toBytes(data)
	if err != nil {
		return bech
	}

	checksum, err := toChars(bech32Checksum(hrp, decoded))
	if err != nil {
		return bech
	}

	return bech + checksum
}

func FuzzEncode(f *testing.F) {
	f.Fuzz(func(t *testing.T, data string) {
		// Make it easier for the fuzzer to generate valid invoice
		// encodings by adding the required prefix and valid checksum.
		data = "lnbc" + data
		data = appendChecksum(data)

		inv, err := Decode(data, &chaincfg.MainNetParams)
		if err != nil {
			return
		}

		// Re-encode the invoice using our private key from unit tests.
		_, err = inv.Encode(testMessageSigner)
		if err != nil {
			return
		}
	})
}
