package bolt12

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// TestSignatureVerifyVector verifies every signed invoice_request vector in
// signature-test.json against the Merkle root, tagged digest, and signature
// the spec records for it. Each match runs as its own subtest, so a vector
// added to the file later is exercised rather than skipped.
func TestSignatureVerifyVector(t *testing.T) {
	t.Parallel()

	vectors := loadSignatureVectors(t)

	// The raw view parses the same file into the same order, so index i
	// holds the untyped form of vectors[i]. It supplies the
	// "H(signature_tag,merkle)" key, whose comma cannot be expressed as a
	// struct tag.
	rawVectors := loadSignatureRawVectors(t)
	require.Len(t, rawVectors, len(vectors))

	var signed int
	for i, tc := range vectors {
		if tc.TLV != "invoice_request" || tc.Bolt12 == "" {
			continue
		}
		signed++

		// The vector's own comment is a paragraph of spec prose, so
		// the file index names the subtest instead.
		t.Run(fmt.Sprintf("vector %d", i), func(t *testing.T) {
			t.Parallel()

			verifyInvoiceRequestSigVector(t, tc, rawVectors[i])
		})
	}

	require.NotZero(t, signed, "no signed invoice_request vector found")
}

// verifyInvoiceRequestSigVector checks one signed invoice_request vector. The
// records feeding the root come from the raw stream rather than the typed
// decoder, so a decoder defect cannot mask a Merkle or signature defect.
func verifyInvoiceRequestSigVector(t *testing.T, tc sigTestVector,
	raw json.RawMessage) {

	// Decode the bech32 string and convert the TLV bytes into the record
	// view merkleRoot consumes.
	_, tlvBytes, err := decodeBech32(tc.Bolt12)
	require.NoError(t, err)

	records := streamToRecords(t, tlvBytes)

	// Keep only the records that participate in the signature root.
	unsigned := signableTLVs(records)

	root, err := merkleRoot(unsigned)
	require.NoError(t, err)

	expectedRoot, err := hex.DecodeString(tc.Merkle)
	require.NoError(t, err)
	require.Equal(t, expectedRoot, root[:])

	// The tag the package builds from its own constants must be the tag the
	// vector was signed under.
	require.Equal(
		t, tc.SignatureTag,
		signatureTagPrefix+tagMsgInvoiceRequest+tagFieldSignature,
	)

	sigDigest := taggedHash(tc.SignatureTag, root[:])

	var rawMap map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &rawMap))

	var expectedDigestHex string
	require.NoError(t, json.Unmarshal(
		rawMap["H(signature_tag,merkle)"], &expectedDigestHex,
	))

	expectedDigest, err := hex.DecodeString(expectedDigestHex)
	require.NoError(t, err)
	require.Equal(t, expectedDigest, sigDigest[:])

	// The vector carries no key of its own, so the signer is the
	// invreq_payer_id the message names. Reading it from the stream keeps
	// the check independent of the typed decoder, and lets a vector signed
	// by another key verify against that key instead of failing.
	payerID := payerIDFromStream(t, tlvBytes)

	sigBytes, err := hex.DecodeString(tc.Signature)
	require.NoError(t, err)
	require.Len(t, sigBytes, 64, "vector signature is not 64 bytes")

	var sig [64]byte
	copy(sig[:], sigBytes)

	require.NoError(t, verifySignature(
		tagMsgInvoiceRequest, tagFieldSignature, root, sig, payerID,
	))
}

// TestVerifyInvoiceRequestVector drives the typed verify path with the spec's
// signed invoice_request: the wire form is decoded, the vector's signature
// attached, and the result verified against the invreq_payer_id the request
// carries. This pins the tag choice, key extraction, and signable-range filter
// of the public API against the spec.
func TestVerifyInvoiceRequestVector(t *testing.T) {
	t.Parallel()

	vectors := loadSignatureVectors(t)

	// Locate the invoice_request vector.
	var tc sigTestVector
	for _, v := range vectors {
		if v.Bolt12 != "" && v.TLV == "invoice_request" {
			tc = v
			break
		}
	}
	require.NotEmpty(t, tc.Bolt12)

	hrp, tlvBytes, err := decodeBech32(tc.Bolt12)
	require.NoError(t, err)
	require.Equal(t, "lnr", hrp)

	ir, err := DecodeInvoiceRequest(tlvBytes)
	require.NoError(t, err)

	sigBytes, err := hex.DecodeString(tc.Signature)
	require.NoError(t, err)

	var sig [64]byte
	copy(sig[:], sigBytes)

	ir.Signature = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType240](sig),
	)

	require.NoError(t, VerifyInvoiceRequest(ir))
}

// TestSignatureVerifyRejectsTampering asserts that every way a malicious
// mediator can tamper with a signed message fails verification, so the
// tree-of-fields guarantee cannot collapse.
func TestSignatureVerifyRejectsTampering(t *testing.T) {
	t.Parallel()

	bobPriv, bobPub := bobKey()

	var msg [32]byte
	for i := range msg {
		msg[i] = byte(i + 1)
	}
	sig, err := signMessage("invoice_request", "signature", msg, bobPriv)
	require.NoError(t, err)

	// Sanity: untouched signature still verifies.
	require.NoError(t, verifySignature(
		"invoice_request", "signature", msg, sig, bobPub,
	))

	t.Run("tampered root", func(t *testing.T) {
		t.Parallel()

		tampered := msg
		tampered[0] ^= 0x01
		require.ErrorIs(
			t, verifySignature(
				"invoice_request", "signature",
				tampered, sig, bobPub,
			),
			ErrInvalidSignature,
		)
	})

	t.Run("tampered signature byte", func(t *testing.T) {
		t.Parallel()

		tamperedSig := sig
		tamperedSig[0] ^= 0xff
		require.ErrorIs(t,
			verifySignature(
				"invoice_request", "signature",
				msg, tamperedSig, bobPub,
			),
			ErrInvalidSignature,
		)
	})

	t.Run("wrong public key", func(t *testing.T) {
		t.Parallel()

		_, alicePub := aliceKey()
		require.ErrorIs(t,
			verifySignature(
				"invoice_request", "signature",
				msg, sig, alicePub,
			),
			ErrInvalidSignature,
		)
	})

	t.Run("cross-tag replay rejected", func(t *testing.T) {
		t.Parallel()

		// Same root, same signature, but verify under the
		// invoice tag instead of invoice_request.
		require.ErrorIs(t,
			verifySignature(
				"invoice", "signature",
				msg, sig, bobPub,
			),
			ErrInvalidSignature,
		)
	})

	t.Run("malformed 64-byte signature", func(t *testing.T) {
		t.Parallel()

		// r = 2^256 - 1 exceeds the secp256k1 field prime, so
		// ParseSignature rejects the encoding before Verify runs.
		// An all-zero signature would parse cleanly and fail at
		// Verify instead, never exercising the parse branch.
		var malformed [64]byte
		for i := range malformed[:32] {
			malformed[i] = 0xff
		}

		err := verifySignature(
			"invoice_request", "signature",
			msg, malformed, bobPub,
		)
		require.ErrorIs(t, err, ErrInvalidSignature)
		require.ErrorContains(t, err, "parse signature")
	})
}

// TestNilKeyGuards pins the cryptographic key guards in the API.
func TestNilKeyGuards(t *testing.T) {
	t.Parallel()

	var (
		root [32]byte
		sig  [64]byte
	)

	tests := []struct {
		name string
		call func(t *testing.T) error
		want error
	}{
		{
			name: "sign message nil key",
			call: func(t *testing.T) error {
				_, err := signMessage(
					"invoice_request", "signature", root,
					nil,
				)

				return err
			},
			want: ErrNilPrivateKey,
		},
		{
			name: "sign invoice request nil key",
			call: func(t *testing.T) error {
				_, err := SignInvoiceRequest(
					validInvoiceRequest(t), nil,
				)

				return err
			},
			want: ErrNilPrivateKey,
		},
		{
			name: "sign invoice nil key",
			call: func(t *testing.T) error {
				_, err := SignInvoice(validInvoice(t), nil)

				return err
			},
			want: ErrNilPrivateKey,
		},
		{
			name: "verify signature nil key",
			call: func(t *testing.T) error {
				return verifySignature(
					"invoice_request", "signature",
					root, sig, nil,
				)
			},
			want: ErrNilPublicKey,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			require.ErrorIs(t, tc.call(t), tc.want)
		})
	}
}

// TestVerifyInvoiceDirect drives VerifyInvoice end to end using a minimal valid
// Invoice constructed via validInvoice.
func TestVerifyInvoiceDirect(t *testing.T) {
	t.Parallel()

	priv, pub := bobKey()

	tests := []struct {
		name string

		// mutate adjusts the valid fixture to isolate the case under
		// test, signing when the case expects success.
		mutate func(t *testing.T, inv *Invoice)

		// wantErr is nil for the happy path. wantContains pins the
		// field context in wrapped errors.
		wantErr      error
		wantContains string
	}{
		{
			name: "valid round-trip verifies",
			mutate: func(t *testing.T, inv *Invoice) {
				_, err := inv.Encode()
				require.NoError(t, err)

				sig, err := SignInvoice(inv, priv)
				require.NoError(t, err)

				inv.Signature = tlv.SomeRecordT(
					tlv.NewPrimitiveRecord[tlv.TlvType240](
						sig,
					),
				)
			},
		},
		{
			name: "missing invoice_node_id",
			mutate: func(t *testing.T, inv *Invoice) {
				inv.InvoiceNodeID = tlv.OptionalRecordT[
					tlv.TlvType176, *btcec.PublicKey,
				]{}
			},
			wantErr: ErrMissingNodeID,
		},
		{
			// A present-but-nil invoice_node_id passes the
			// presence check but has no key to verify
			// against.
			name: "nil invoice_node_id",
			mutate: func(t *testing.T, inv *Invoice) {
				inv.InvoiceNodeID = tlv.SomeRecordT(
					tlv.NewPrimitiveRecord[tlv.TlvType176](
						(*btcec.PublicKey)(nil),
					),
				)
			},
			wantErr:      ErrNilPublicKey,
			wantContains: "invoice_node_id",
		},
		{
			name: "missing signature",
			mutate: func(t *testing.T, inv *Invoice) {
				// The fixture carries no signature.
			},
			wantErr: ErrMissingSignature,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			inv := validInvoice(t)
			inv.InvoiceNodeID = tlv.SomeRecordT(
				tlv.NewPrimitiveRecord[tlv.TlvType176](pub),
			)
			tc.mutate(t, inv)

			err := VerifyInvoice(inv)
			require.ErrorIs(t, err, tc.wantErr)
			if tc.wantContains != "" {
				require.Contains(
					t, err.Error(), tc.wantContains,
				)
			}
		})
	}
}

// TestVerifyInvoiceRequestDirect drives VerifyInvoiceRequest end to end using a
// minimal valid InvoiceRequest constructed via validInvoiceRequest.
func TestVerifyInvoiceRequestDirect(t *testing.T) {
	t.Parallel()

	priv, pub := bobKey()

	tests := []struct {
		name string

		// mutate adjusts the valid fixture to isolate the case
		// under test, signing when the case expects success.
		mutate func(t *testing.T, ir *InvoiceRequest)

		// wantErr is nil for the happy path. wantContains pins
		// the field context in wrapped errors.
		wantErr      error
		wantContains string
	}{
		{
			name: "valid round-trip verifies",
			mutate: func(t *testing.T, ir *InvoiceRequest) {
				sig, err := SignInvoiceRequest(ir, priv)
				require.NoError(t, err)

				ir.Signature = tlv.SomeRecordT(
					tlv.NewPrimitiveRecord[tlv.TlvType240](
						sig,
					),
				)
			},
		},
		{
			name: "missing invreq_payer_id",
			mutate: func(t *testing.T, ir *InvoiceRequest) {
				ir.InvreqPayerID = tlv.OptionalRecordT[
					tlv.TlvType88, *btcec.PublicKey,
				]{}
			},
			wantErr: ErrMissingPayerID,
		},
		{
			// A present-but-nil invreq_payer_id passes the presence
			// check but has no key to verify against.
			name: "nil invreq_payer_id",
			mutate: func(t *testing.T, ir *InvoiceRequest) {
				ir.InvreqPayerID = tlv.SomeRecordT(
					tlv.NewPrimitiveRecord[tlv.TlvType88](
						(*btcec.PublicKey)(nil),
					),
				)
			},
			wantErr:      ErrNilPublicKey,
			wantContains: "invreq_payer_id",
		},
		{
			name: "missing signature",
			mutate: func(t *testing.T, ir *InvoiceRequest) {
				ir.Signature = tlv.OptionalRecordT[
					tlv.TlvType240, [64]byte,
				]{}
			},
			wantErr: ErrMissingSignature,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ir := validInvoiceRequest(t)
			ir.InvreqPayerID = tlv.SomeRecordT(
				tlv.NewPrimitiveRecord[tlv.TlvType88](pub),
			)
			tc.mutate(t, ir)

			err := VerifyInvoiceRequest(ir)
			require.ErrorIs(t, err, tc.wantErr)
			if tc.wantContains != "" {
				require.Contains(
					t, err.Error(), tc.wantContains,
				)
			}
		})
	}
}
