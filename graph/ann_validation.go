package graph

import (
	"bytes"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lnd/lnwire"
)

// ValidateNodeAnn validates the node announcement by ensuring that the
// attached signature is needed a signature of the node announcement under the
// specified node public key.
func ValidateNodeAnn(a *lnwire.NodeAnnouncement) error {
	// Reconstruct the data of announcement which should be covered by the
	// signature so we can verify the signature shortly below
	data, err := a.DataToSign()
	if err != nil {
		return err
	}

	nodeSig, err := a.Signature.ToSignature()
	if err != nil {
		return err
	}
	nodeKey, err := btcec.ParsePubKey(a.NodeID[:])
	if err != nil {
		return err
	}

	// Finally ensure that the passed signature is valid, if not we'll
	// return an error so this node announcement can be rejected.
	dataHash := chainhash.DoubleHashB(data)
	if !nodeSig.Verify(dataHash, nodeKey) {
		var msgBuf bytes.Buffer
		if _, err := lnwire.WriteMessage(&msgBuf, a, 0); err != nil {
			return err
		}

		return errors.Errorf("signature on NodeAnnouncement(%x) is "+
			"invalid: %x", nodeKey.SerializeCompressed(),
			msgBuf.Bytes())
	}

	return nil
}
