package zpay32

import (
	"testing"

	"github.com/btcsuite/btcd/chaincfg"
)

func FuzzDecode(f *testing.F) {
	f.Fuzz(func(t *testing.T, data string) {
		_, _ = Decode(data, &chaincfg.TestNet3Params)
	})
}

func FuzzEncode(f *testing.F) {
	f.Fuzz(func(t *testing.T, data string) {
		inv, err := Decode(data, &chaincfg.TestNet3Params)
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
