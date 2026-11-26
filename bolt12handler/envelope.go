package bolt12handler

import (
	"github.com/btcsuite/btcd/chainhash/v2"
)

// envelopeBindingTag tags the hash of an encoded invoice envelope that the
// payment path id covers.
const envelopeBindingTag = "bolt12/invoice-envelope"

// EnvelopeBinding returns the hash of an encoded invoice envelope that the
// payment path id covers. A change of any envelope byte changes it. It hashes
// the bytes as the onion carries them and not a re-encoding of the decoded
// data, because the decoder skips unknown records.
func EnvelopeBinding(envelope []byte) [32]byte {
	return *chainhash.TaggedHash([]byte(envelopeBindingTag), envelope)
}
