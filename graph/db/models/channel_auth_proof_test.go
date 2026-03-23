package models

import (
	"bytes"
	"testing"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

func TestChannelAuthProofFromAnnounceSignaturesV1(t *testing.T) {
	t.Parallel()

	sig := func(b byte) lnwire.Sig {
		raw := bytes.Repeat([]byte{b}, 64)
		s, err := lnwire.NewSigFromWireECDSA(raw)
		require.NoError(t, err)
		return s
	}

	ann1 := &lnwire.AnnounceSignatures1{
		NodeSignature:    sig(1),
		BitcoinSignature: sig(2),
	}
	ann2 := &lnwire.AnnounceSignatures1{
		NodeSignature:    sig(3),
		BitcoinSignature: sig(4),
	}

	proof, err := ChannelAuthProofFromAnnounceSignatures(ann1, ann2, true)
	require.NoError(t, err)
	require.Equal(t, ann1.NodeSignature.ToSignatureBytes(), proof.NodeSig1())
	require.Equal(t, ann2.NodeSignature.ToSignatureBytes(), proof.NodeSig2())
	require.Equal(t, ann1.BitcoinSignature.ToSignatureBytes(),
		proof.BitcoinSig1())
	require.Equal(t, ann2.BitcoinSignature.ToSignatureBytes(),
		proof.BitcoinSig2())
}

func TestChannelAuthProofFromAnnounceSignaturesV2Pending(t *testing.T) {
	t.Parallel()

	ann := lnwire.NewAnnSigs2(
		lnwire.ChannelID{}, lnwire.ShortChannelID{}, lnwire.PartialSig{},
	)

	proof, err := ChannelAuthProofFromAnnounceSignatures(ann, ann, true)
	require.ErrorIs(t, err, ErrV2AnnSigProofAssemblyPending)
	require.Nil(t, proof)
}

func TestChannelAuthProofFromAnnounceSignaturesVersionMismatch(t *testing.T) {
	t.Parallel()

	ann1 := &lnwire.AnnounceSignatures1{}
	ann2 := lnwire.NewAnnSigs2(
		lnwire.ChannelID{}, lnwire.ShortChannelID{}, lnwire.PartialSig{},
	)

	proof, err := ChannelAuthProofFromAnnounceSignatures(ann1, ann2, true)
	require.Error(t, err)
	require.Contains(t, err.Error(), "version mismatch")
	require.Nil(t, proof)
}
