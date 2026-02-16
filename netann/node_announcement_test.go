package netann

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// TestValidateNodeAnn2Signature verifies that:
//  1. a correctly signed node_announcement_2 passes both direct v2 signature
//     validation and the generic ValidateNodeAnn dispatcher, and
//  2. changing a signed field after signing invalidates the signature.
func TestValidateNodeAnn2Signature(t *testing.T) {
	t.Parallel()

	ann := signedNodeAnnouncement2(t)

	require.NoError(t, ValidateNodeAnn2Signature(ann))
	require.NoError(t, ValidateNodeAnn(ann))

	// Mutating a signed field should invalidate the signature.
	ann.BlockHeight.Val++

	err := ValidateNodeAnn2Signature(ann)
	require.Error(t, err)
	require.ErrorContains(t, err, "invalid")
}

// TestValidateNodeAnn2Fields verifies that the generic v2 validation path
// enforces node_announcement_2 field constraints before accepting the message.
// Here we assert that a DNS address with port 0 is rejected.
func TestValidateNodeAnn2Fields(t *testing.T) {
	t.Parallel()

	ann := signedNodeAnnouncement2(t, func(ann *lnwire.NodeAnnouncement2) {
		dnsRecord := tlv.ZeroRecordT[tlv.TlvType11, lnwire.DNSAddress]()
		dnsRecord.Val = lnwire.DNSAddress{
			Hostname: "example.com",
			Port:     0,
		}
		ann.DNSHostName = tlv.SomeRecordT(dnsRecord)
	})

	err := ValidateNodeAnn(ann)
	require.Error(t, err)
	require.ErrorContains(t, err, "port cannot be zero")
}

// signedNodeAnnouncement2 creates a minimally valid node_announcement_2 and
// signs it with a freshly generated node key.
//
// The optional mutators are applied before signing so tests can produce custom
// but still consistently signed fixtures (unless they intentionally mutate
// fields after signing).
func signedNodeAnnouncement2(t *testing.T,
	opts ...func(*lnwire.NodeAnnouncement2)) *lnwire.NodeAnnouncement2 {

	t.Helper()

	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	var nodeID [33]byte
	copy(nodeID[:], privKey.PubKey().SerializeCompressed())

	features := lnwire.NewRawFeatureVector()
	ann := &lnwire.NodeAnnouncement2{
		Features: tlv.NewRecordT[tlv.TlvType0](*features),
		BlockHeight: tlv.NewPrimitiveRecord[tlv.TlvType2](
			uint32(100),
		),
		NodeID:            tlv.NewPrimitiveRecord[tlv.TlvType4](nodeID),
		ExtraSignedFields: make(lnwire.ExtraSignedFields),
	}

	for _, opt := range opts {
		opt(ann)
	}

	digest, err := NodeAnn2DigestToSign(ann)
	require.NoError(t, err)

	sig, err := schnorr.Sign(privKey, digest)
	require.NoError(t, err)

	lnSig, err := lnwire.NewSigFromSignature(sig)
	require.NoError(t, err)
	ann.Signature.Val = lnSig

	return ann
}
