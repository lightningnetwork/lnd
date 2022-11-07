package zpay32

import (
	"encoding/hex"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
)

func FuzzDecode(f *testing.F) {
	f.Fuzz(func(t *testing.T, data string) {
		inv, err := Decode(data, &chaincfg.TestNet3Params)
		if err != nil {
			return
		}

		// Call these functions as a sanity check to make sure the
		// invoice is well-formed.
		_ = inv.MinFinalCLTVExpiry()
		_ = inv.Expiry()
	})
}

func FuzzEncode(f *testing.F) {
	f.Fuzz(func(t *testing.T, data string) {
		inv, err := Decode(data, &chaincfg.TestNet3Params)
		if err != nil {
			return
		}

		// Call these functions as a sanity check to make sure the
		// invoice is well-formed.
		_ = inv.MinFinalCLTVExpiry()
		_ = inv.Expiry()

		// Initialize the static key we will be using for this fuzz
		// test.
		testPrivKeyBytes, _ := hex.DecodeString("e126f68f7eafcc8b74f54d269fe206be715000f94dac067d1c04a8ca3b2db734")
		testPrivKey, _ := btcec.PrivKeyFromBytes(testPrivKeyBytes)

		// Then, initialize the testMessageSigner so we can encode out
		// invoices with this private key.
		testMessageSigner := MessageSigner{
			SignCompact: func(msg []byte) ([]byte, error) {
				hash := chainhash.HashB(msg)
				sig, err := ecdsa.SignCompact(testPrivKey, hash, true)
				if err != nil {
					return nil,
						fmt.Errorf("can't sign the "+
							"message: %v", err)
				}
				return sig, nil
			},
		}
		_, err = inv.Encode(testMessageSigner)
		if err != nil {
			return
		}
	})
}
