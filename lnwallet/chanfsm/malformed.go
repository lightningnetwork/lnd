package chanfsm

import (
	"bytes"
	"fmt"

	"github.com/lightningnetwork/lnd/lnwire"
)

// malformedReason converts a malformed fail into the failure it stands for,
// in its encoded form. It matches what the link records.
func malformedReason(msg *lnwire.UpdateFailMalformedHTLC) ([]byte, error) {
	var failure lnwire.FailureMessage
	switch msg.FailureCode {
	case lnwire.CodeInvalidOnionVersion:
		failure = &lnwire.FailInvalidOnionVersion{
			OnionSHA256: msg.ShaOnionBlob,
		}

	case lnwire.CodeInvalidOnionHmac:
		failure = &lnwire.FailInvalidOnionHmac{
			OnionSHA256: msg.ShaOnionBlob,
		}

	case lnwire.CodeInvalidBlinding:
		failure = &lnwire.FailInvalidBlinding{
			OnionSHA256: msg.ShaOnionBlob,
		}

	// Any other code, including an invalid onion key, is reported as an
	// invalid onion key, so a peer can't pass back a failure that
	// penalizes us more than it should.
	default:
		failure = &lnwire.FailInvalidOnionKey{
			OnionSHA256: msg.ShaOnionBlob,
		}
	}

	var b bytes.Buffer
	if err := lnwire.EncodeFailure(&b, failure, 0); err != nil {
		return nil, fmt.Errorf("unable to encode malformed error: %w",
			err)
	}

	return b.Bytes(), nil
}
