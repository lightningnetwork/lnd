package netann

import (
	"bytes"
	"errors"
	"fmt"
	"image/color"
	"net"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
)

const (
	// nodeAnn2MsgName is a string representing the name of the
	// NodeAnnouncement2 message. This string will be used during the
	// construction of the tagged hash message to be signed when producing
	// the signature for the NodeAnnouncement2 message.
	nodeAnn2MsgName = "node_announcement_2"

	// nodeAnn2SigFieldName is the name of the signature field of the
	// NodeAnnouncement2 message. This string will be used during the
	// construction of the tagged hash message to be signed when producing
	// the signature for the NodeAnnouncement2 message.
	nodeAnn2SigFieldName = "signature"
)

// NodeAnnModifier is a closure that makes in-place modifications to an
// lnwire.NodeAnnouncement1.
type NodeAnnModifier func(*lnwire.NodeAnnouncement1)

// NodeAnnSetAlias is a functional option that sets the alias of the
// given node announcement.
func NodeAnnSetAlias(alias lnwire.NodeAlias) func(*lnwire.NodeAnnouncement1) {
	return func(nodeAnn *lnwire.NodeAnnouncement1) {
		nodeAnn.Alias = alias
	}
}

// NodeAnnSetAddrs is a functional option that allows updating the addresses of
// the given node announcement.
func NodeAnnSetAddrs(addrs []net.Addr) func(*lnwire.NodeAnnouncement1) {
	return func(nodeAnn *lnwire.NodeAnnouncement1) {
		nodeAnn.Addresses = addrs
	}
}

// NodeAnnSetColor is a functional option that sets the color of the
// given node announcement.
func NodeAnnSetColor(newColor color.RGBA) func(*lnwire.NodeAnnouncement1) {
	return func(nodeAnn *lnwire.NodeAnnouncement1) {
		nodeAnn.RGBColor = newColor
	}
}

// NodeAnnSetFeatures is a functional option that allows updating the features of
// the given node announcement.
func NodeAnnSetFeatures(
	features *lnwire.RawFeatureVector) func(*lnwire.NodeAnnouncement1) {

	return func(nodeAnn *lnwire.NodeAnnouncement1) {
		nodeAnn.Features = features
	}
}

// NodeAnnSetTimestamp is a functional option that sets the timestamp of the
// announcement to the current time, or increments it if the timestamp is
// already in the future.
func NodeAnnSetTimestamp(nodeAnn *lnwire.NodeAnnouncement1) {
	newTimestamp := uint32(time.Now().Unix())
	if newTimestamp <= nodeAnn.Timestamp {
		// Increment the prior value to  ensure the timestamp
		// monotonically increases, otherwise the announcement won't
		// propagate.
		newTimestamp = nodeAnn.Timestamp + 1
	}
	nodeAnn.Timestamp = newTimestamp
}

// SignNodeAnnouncement signs the lnwire.NodeAnnouncement1 provided, which
// should be the most recent, valid update, otherwise the timestamp may not
// monotonically increase from the prior.
func SignNodeAnnouncement(signer lnwallet.MessageSigner,
	keyLoc keychain.KeyLocator, nodeAnn *lnwire.NodeAnnouncement1) error {

	// Create the DER-encoded ECDSA signature over the message digest.
	sig, err := SignAnnouncement(signer, keyLoc, nodeAnn)
	if err != nil {
		return err
	}

	// Parse the DER-encoded signature into a fixed-size 64-byte array.
	nodeAnn.Signature, err = lnwire.NewSigFromSignature(sig)
	return err
}

// ValidateNodeAnn validates a node announcement according to its version-
// specific rules.
func ValidateNodeAnn(a lnwire.NodeAnnouncement) error {
	switch a := a.(type) {
	case *lnwire.NodeAnnouncement1:
		err := ValidateNodeAnnFields(a)
		if err != nil {
			return fmt.Errorf("invalid node ann "+
				"fields: %w", err)
		}

		return ValidateNodeAnnSignature(a)

	case *lnwire.NodeAnnouncement2:
		err := validateNodeAnn2Fields(a)
		if err != nil {
			return fmt.Errorf("invalid node ann "+
				"fields: %w", err)
		}

		return ValidateNodeAnn2Signature(a)

	default:
		return fmt.Errorf("unhandled implementation of "+
			"lnwire.NodeAnnouncement: %T", a)
	}
}

// ValidateNodeAnnFields validates the fields of a node announcement.
func ValidateNodeAnnFields(a *lnwire.NodeAnnouncement1) error {
	// Check that it only has at most one DNS address.
	hasDNSAddr := false
	for _, addr := range a.Addresses {
		dnsAddr, ok := addr.(*lnwire.DNSAddress)
		if !ok {
			continue
		}
		if hasDNSAddr {
			return errors.New("node announcement contains " +
				"multiple DNS addresses. Only one is allowed")
		}

		hasDNSAddr = true

		err := lnwire.ValidateDNSAddr(dnsAddr.Hostname, dnsAddr.Port)
		if err != nil {
			return err
		}
	}

	return nil
}

// validateNodeAnn2Fields validates the fields of a v2 node announcement.
func validateNodeAnn2Fields(a *lnwire.NodeAnnouncement2) error {
	var validationErr error
	a.DNSHostName.ValOpt().WhenSome(func(dns lnwire.DNSAddress) {
		validationErr = lnwire.ValidateDNSAddr(
			dns.Hostname, dns.Port,
		)
	})

	return validationErr
}

// ValidateNodeAnn2Signature validates the node announcement by ensuring that
// the attached signature is a valid signature of the node announcement under
// the announced node public key.
func ValidateNodeAnn2Signature(a *lnwire.NodeAnnouncement2) error {
	digest, err := NodeAnn2DigestToSign(a)
	if err != nil {
		return fmt.Errorf("unable to reconstruct message data: %w", err)
	}

	nodeSig, err := a.Signature.Val.ToSignature()
	if err != nil {
		return err
	}

	nodeKey, err := btcec.ParsePubKey(a.NodeID.Val[:])
	if err != nil {
		return err
	}

	if !nodeSig.Verify(digest, nodeKey) {
		return fmt.Errorf("signature on NodeAnnouncement2(%x) is "+
			"invalid", nodeKey.SerializeCompressed())
	}

	return nil
}

// NodeAnn2DigestToSign computes the digest of the node announcement message to
// be signed.
func NodeAnn2DigestToSign(a *lnwire.NodeAnnouncement2) ([]byte, error) {
	data, err := lnwire.SerialiseFieldsToSign(a)
	if err != nil {
		return nil, err
	}

	hash := MsgHash(nodeAnn2MsgName, nodeAnn2SigFieldName, data)

	return hash[:], nil
}

// ValidateNodeAnnSignature validates the node announcement by ensuring that the
// attached signature is needed a signature of the node announcement under the
// specified node public key.
func ValidateNodeAnnSignature(a *lnwire.NodeAnnouncement1) error {
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

		return fmt.Errorf("signature on NodeAnnouncement1(%x) is "+
			"invalid: %x", nodeKey.SerializeCompressed(),
			msgBuf.Bytes())
	}

	return nil
}
