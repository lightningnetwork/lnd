package zpay32

import (
	"strings"
	"testing"

	"github.com/btcsuite/btcd/chaincfg"
)

// getPrefixAndChainParams selects network chain parameters based on the fuzzer-
// selected input byte "net". 50% of the time mainnet is selected, while the
// other 50% of the time one of the test networks is selected. For each network
// the appropriate invoice HRP prefix is also returned, with a small chance that
// no prefix is returned, allowing the fuzzer to generate invalid prefixes too.
func getPrefixAndChainParams(net byte) (string, *chaincfg.Params) {
	switch {
	case net == 0x00:
		return "", &chaincfg.RegressionNetParams
	case net < 0x20:
		return "lnbcrt", &chaincfg.RegressionNetParams

	case net == 0x20:
		return "", &chaincfg.TestNet3Params
	case net < 0x40:
		return "lntb", &chaincfg.TestNet3Params

	case net == 0x40:
		return "", &chaincfg.SimNetParams
	case net < 0x60:
		return "lnsb", &chaincfg.SimNetParams

	case net == 0x60:
		return "", &chaincfg.SigNetParams
	case net < 0x80:
		return "lntbs", &chaincfg.SigNetParams

	case net == 0x80:
		return "", &chaincfg.MainNetParams
	default:
		return "lnbc", &chaincfg.MainNetParams
	}
}

func FuzzDecode(f *testing.F) {
	f.Fuzz(func(t *testing.T, net byte, data string) {
		_, chainParams := getPrefixAndChainParams(net)
		_, _ = Decode(data, chainParams)
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
	f.Fuzz(func(t *testing.T, net byte, data string) {
		// Make it easier for the fuzzer to generate valid invoice
		// encodings by adding the required prefix and valid checksum.
		hrpPrefix, chainParams := getPrefixAndChainParams(net)
		data = hrpPrefix + data
		data = appendChecksum(data)

		inv, err := Decode(data, chainParams)
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
