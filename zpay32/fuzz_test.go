package zpay32

import (
	"bytes"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/chaincfg"
)

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
	f.Add(byte(0xFF), "lnbc100n1p38q7dzpp5ypz09jrd8p993snjwnm68g0l4ftwwj5ep5r8c7z73rh4pxvw5khqdqqcqzpgxqyz5vqsp5usyc4lk9chsfp53kvcnvq456ganh60d89reykdngsmtj6yw0fmas9qyyssqj9ye8k3888pjhle3l5qpgday3basdukmmprh3xazelgacdcmc6a84f0x9jzvn9lyha7dgesg68dl5k3h3655fj2ggkpn4470dcj59cqvsa9k4")
	
	f.Fuzz(func(t *testing.T, net byte, data string) {
		_, chainParams := getPrefixAndChainParams(net)
		invoice, err := Decode(data, chainParams)
		if err != nil {
			return
		}
		
		if len(invoice.PaymentHash) > 0 {
			if len(invoice.PaymentHash) != 32 {
				t.Errorf("Invalid payment hash length: %d", len(invoice.PaymentHash))
			}
		}
		
		if invoice.MilliSat != nil {
			amount := *invoice.MilliSat
			if amount < 0 {
				t.Errorf("Negative amount parsed: %v", amount)
			}
		}
	})
}

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
		hrpPrefix, chainParams := getPrefixAndChainParams(net)
		data = hrpPrefix + data
		data = appendChecksum(data)

		inv, err := Decode(data, chainParams)
		if err != nil {
			return
		}

		encoded, err := inv.Encode(testMessageSigner)
		if err != nil {
			return
		}
		
		decodedAgain, err := Decode(encoded, chainParams)
		if err != nil {
			t.Errorf("Failed to decode re-encoded invoice: %v", err)
			return
		}
		
		if !bytes.Equal(inv.PaymentHash[:], decodedAgain.PaymentHash[:]) {
			t.Errorf("Payment hash mismatch after re-encoding")
		}
		
		if (inv.MilliSat == nil) != (decodedAgain.MilliSat == nil) {
			t.Errorf("MilliSat nullability changed after re-encoding")
		} else if inv.MilliSat != nil && decodedAgain.MilliSat != nil {
			if *inv.MilliSat != *decodedAgain.MilliSat {
				t.Errorf("Amount changed: %v vs %v", *inv.MilliSat, *decodedAgain.MilliSat)
			}
		}
		
		if (inv.Description == nil) != (decodedAgain.Description == nil) {
			t.Errorf("Description nullability changed after re-encoding")
		} else if inv.Description != nil && decodedAgain.Description != nil {
			if *inv.Description != *decodedAgain.Description {
				t.Errorf("Description changed after re-encoding")
			}
		}
	})
}