package zpay32

import (
	"bytes"
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

// validateInvoiceForFuzz performs common validation checks on decoded invoices
// during fuzzing. Returns false if the invoice fails validation.
func validateInvoiceForFuzz(t *testing.T, invoice *Invoice) bool {
	// 1) If a hash is present, it must be exactly 32 bytes
	if len(invoice.PaymentHash) > 0 && len(invoice.PaymentHash) != 32 {
		t.Errorf("payment hash length = %d; want 32", len(invoice.PaymentHash))
		return false
	}

	// 2) If an amount is present, it must never be negative
	if invoice.MilliSat != nil && *invoice.MilliSat < 0 {
		t.Errorf("parsed negative amount: %d", *invoice.MilliSat)
		return false
	}
	
	return true
}

func FuzzDecode(f *testing.F) {
	f.Fuzz(func(t *testing.T, net byte, data string) {
		// We only need the chain params here.
		_, params := getPrefixAndChainParams(net)

		invoice, err := Decode(data, params)
		if err != nil {
			return
		}

		validateInvoiceForFuzz(t, invoice)
	})
}

// appendChecksum returns bech with its bech32 checksum appended (if valid).
// Otherwise returns bech unchanged.
func appendChecksum(bech string) string {
	lower := strings.ToLower(bech)
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
		// Prepend valid HRP and checksum to help the fuzzer.
		hrp, params := getPrefixAndChainParams(net)
		data = hrp + data
		data = appendChecksum(data)

		// Decode; skip invalid.
		inv, err := Decode(data, params)
		if err != nil {
			return
		}
		
		// Validate the initially decoded invoice
		if !validateInvoiceForFuzz(t, inv) {
			return
		}

		// Re-encode.
		encoded, err := inv.Encode(testMessageSigner)
		if err != nil {
			return
		}

		// Round‑trip: decode what we just encoded and compare fields.
		inv2, err := Decode(encoded, params)
		if err != nil {
			t.Errorf("re-decode failed: %v", err)
			return
		}
		
		// Validate the round-trip decoded invoice
		validateInvoiceForFuzz(t, inv2)

		// PaymentHash preserved exactly.
		if !bytes.Equal(inv.PaymentHash[:], inv2.PaymentHash[:]) {
			t.Errorf("payment hash mismatch after round-trip")
		}

		// MilliSat nullability and value preserved.
		if (inv.MilliSat == nil) != (inv2.MilliSat == nil) ||
			(inv.MilliSat != nil && *inv.MilliSat != *inv2.MilliSat) {
			t.Errorf("amount changed after round-trip: %v vs %v",
				inv.MilliSat, inv2.MilliSat)
		}
	})
}