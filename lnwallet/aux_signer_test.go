package lnwallet

import (
	"testing"

	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// sigHashOverrideSigner is a MockAuxSigner whose HtlcSigHashType always
// returns the configured sighash type, used to simulate an aux signer that
// negotiates a non-default HTLC sighash.
type sigHashOverrideSigner struct {
	*MockAuxSigner

	sigHash txscript.SigHashType
}

// HtlcSigHashType returns the configured sighash type.
func (s *sigHashOverrideSigner) HtlcSigHashType(
	_ HtlcSigHashReq) fn.Option[txscript.SigHashType] {

	return fn.Some(s.sigHash)
}

// TestResolveHtlcSigHashTypeCustomOnly asserts that a negotiated HTLC sighash
// type can only ever take effect on aux/custom (taproot asset) channels: for
// any channel type without a tapscript root, ResolveHtlcSigHashType returns
// the standard sighash flags even when an aux signer requests SigHashDefault.
//
// For reference, the sighash each channel type is expected to use for
// second-level HTLC transactions:
//
//	legacy (no anchors):  SIGHASH_ALL                  (0x01)
//	anchor / taproot:     SIGHASH_SINGLE|ANYONECANPAY  (0x83)
//	custom + negotiated:  SIGHASH_DEFAULT              (0x00)
func TestResolveHtlcSigHashTypeCustomOnly(t *testing.T) {
	t.Parallel()

	taprootChanType := channeldb.SimpleTaprootFeatureBit |
		channeldb.AnchorOutputsBit |
		channeldb.ZeroHtlcTxFeeBit |
		channeldb.SingleFunderTweaklessBit

	customChanType := taprootChanType | channeldb.TapscriptRootBit

	// An aux signer that always negotiates SigHashDefault.
	signer := fn.Some[AuxSigner](&sigHashOverrideSigner{
		MockAuxSigner: NewAuxSignerMock(nil),
		sigHash:       txscript.SigHashDefault,
	})

	req := HtlcSigHashReq{}

	// A non-custom taproot channel must keep the standard sighash flags,
	// no matter what the aux signer says.
	got := ResolveHtlcSigHashType(taprootChanType, signer, req)
	require.Equal(t, HtlcSigHashType(taprootChanType), got,
		"non-custom taproot channel must ignore aux signer sighash")

	// Same for a legacy (non-taproot) channel type.
	legacyChanType := channeldb.SingleFunderTweaklessBit
	got = ResolveHtlcSigHashType(legacyChanType, signer, req)
	require.Equal(t, HtlcSigHashType(legacyChanType), got,
		"legacy channel must ignore aux signer sighash")

	// A custom channel (tapscript root) with the same signer negotiates
	// SigHashDefault.
	got = ResolveHtlcSigHashType(customChanType, signer, req)
	require.Equal(t, txscript.SigHashDefault, got)

	// A custom channel without an aux signer falls back to the standard
	// flags.
	noSigner := fn.None[AuxSigner]()
	got = ResolveHtlcSigHashType(customChanType, noSigner, req)
	require.Equal(t, HtlcSigHashType(customChanType), got)
}
