package chancloser

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

type closeReplyCase struct {
	name          string
	variant       closeSigVariant
	local, remote btcutil.Amount
}

func TestCloseeReplyVariants(t *testing.T) {
	for _, taproot := range []bool{false, true} {
		for _, tc := range []closeReplyCase{
			{"both", closeSigBoth, 10000, 10000},
			{"closer only", closeSigCloserOnly, 1, 10000},
			{"closee only", closeSigCloseeOnly, 10000, 1},
		} {
			name := tc.name + map[bool]string{
				true: " taproot", false: " legacy",
			}[taproot]
			t.Run(name, func(t *testing.T) {
				testCloseeReplyVariant(t, tc, taproot)
			})
		}
	}
}

func testCloseeReplyVariant(t *testing.T, tc closeReplyCase, taproot bool) {
	var scalar btcec.ModNScalar
	scalar.SetInt(1)
	key, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	partial := musig2.NewPartialSignature(&scalar, key.PubKey())
	localPartial := lnwallet.NewMusigPartialSig(
		&partial, lnwire.Musig2Nonce{}, lnwire.Musig2Nonce{}, nil,
		fn.None[chainhash.Hash](),
	)
	remotePartial := lnwire.PartialSigWithNonce{
		PartialSig: lnwire.PartialSig{Sig: scalar},
		Nonce:      lnwire.Musig2Nonce{1, 2, 3},
	}

	msg := lnwire.ClosingComplete{}
	if taproot {
		switch tc.variant {
		case closeSigBoth:
			msg.TaprootClosingSigs.CloserAndClosee =
				newPartialSigWithNonceTlv[tlv.TlvType7](
					remotePartial,
				)
		case closeSigCloserOnly:
			msg.TaprootClosingSigs.CloserNoClosee =
				newPartialSigWithNonceTlv[tlv.TlvType5](
					remotePartial,
				)
		case closeSigCloseeOnly:
			msg.TaprootClosingSigs.NoCloserClosee =
				newPartialSigWithNonceTlv[tlv.TlvType6](
					remotePartial,
				)
		}
	} else {
		switch tc.variant {
		case closeSigBoth:
			msg.ClosingSigs.CloserAndClosee =
				newSigTlv[tlv.TlvType3](
					remoteWireSig,
				)
		case closeSigCloserOnly:
			msg.ClosingSigs.CloserNoClosee =
				newSigTlv[tlv.TlvType1](
					remoteWireSig,
				)
		case closeSigCloseeOnly:
			msg.ClosingSigs.NoCloserClosee =
				newSigTlv[tlv.TlvType2](
					remoteWireSig,
				)
		}
	}
	dust := lnwallet.DustLimitForSize(len(localAddr))
	_, _, variant, err := extractSigAndNonceFromClosingComplete(
		msg, tc.local < dust, taproot,
	)
	require.NoError(t, err)
	require.Equal(t, tc.variant, variant)

	tx, err := lnwallet.CreateCooperativeCloseTx(
		wire.TxIn{}, dust, dust, tc.local, tc.remote,
		localAddr, remoteAddr,
	)
	require.NoError(t, err)
	haveLocal, haveRemote := false, false
	for _, out := range tx.TxOut {
		if string(out.PkScript) == string(localAddr) {
			haveLocal = true
		}
		if string(out.PkScript) == string(remoteAddr) {
			haveRemote = true
		}
	}
	require.Equal(t, tc.variant != closeSigCloserOnly, haveLocal)
	require.Equal(t, tc.variant != closeSigCloseeOnly, haveRemote)

	env := &Environment{}
	if taproot {
		env.LocalMusigSession = newMockMusigSession()
		env.RemoteMusigSession = newMockMusigSession()
	}
	reply, err := createClosingSigMessage(
		env, localSigWire, localPartial, localAddr,
		remoteAddr, 0, 0, variant,
	)
	require.NoError(t, err)

	// The mock session's nonce is not a valid curve point.
	if taproot {
		var nonce lnwire.Musig2Nonce
		copy(nonce[:33], key.PubKey().SerializeCompressed())
		copy(nonce[33:], key.PubKey().SerializeCompressed())
		reply.NextCloseeNonce = newNonceTlv(nonce)
	}
	var encoded bytes.Buffer
	require.NoError(t, reply.Encode(&encoded, 0))
	var decoded lnwire.ClosingSig
	require.NoError(t, decoded.Decode(&encoded, 0))
	result, _ := validateAndExtractSigAndNonce(decoded, taproot)
	_, err = result.Unpack()
	require.NoError(t, err)
	reply = &decoded
	if taproot {
		require.Equal(t, variant == closeSigBoth,
			reply.TaprootPartialSigs.CloserAndClosee.IsSome())
		require.Equal(t, variant == closeSigCloserOnly,
			reply.TaprootPartialSigs.CloserNoClosee.IsSome())
		require.Equal(t, variant == closeSigCloseeOnly,
			reply.TaprootPartialSigs.NoCloserClosee.IsSome())
	} else {
		require.Equal(t, variant == closeSigBoth,
			reply.ClosingSigs.CloserAndClosee.IsSome())
		require.Equal(t, variant == closeSigCloserOnly,
			reply.ClosingSigs.CloserNoClosee.IsSome())
		require.Equal(t, variant == closeSigCloseeOnly,
			reply.ClosingSigs.NoCloserClosee.IsSome())
	}
}
