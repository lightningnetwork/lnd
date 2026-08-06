package bolt12

import (
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
)

// signatureTagPrefix is the literal prefix for all BOLT 12 signature tags.
const signatureTagPrefix = "lightning"

// ErrInvalidSignature is returned by VerifyInvoice and VerifyInvoiceRequest
// when the BIP-340 Schnorr signature does not validate against the message's
// Merkle root and signing key.
var ErrInvalidSignature = errors.New("BOLT 12 signature is invalid")

// ErrNilPrivateKey is returned by the sign entry paths when the signing key is
// nil.
var ErrNilPrivateKey = errors.New("BOLT 12 signing key is nil")

// signMessage creates a BIP-340 Schnorr signature over the Merkle root of a
// BOLT 12 message. The tag is "lightning" || messageName || fieldName.
func signMessage(messageName, fieldName string, root [32]byte,
	privKey *btcec.PrivateKey) ([64]byte, error) {

	if privKey == nil {
		return [64]byte{}, ErrNilPrivateKey
	}

	tag := signatureTagPrefix + messageName + fieldName
	digest := taggedHash(tag, root[:])

	sig, err := schnorr.Sign(privKey, digest[:])
	if err != nil {
		return [64]byte{}, fmt.Errorf("sign: %w", err)
	}

	var result [64]byte
	copy(result[:], sig.Serialize())

	return result, nil
}

// verifySignature verifies a BIP-340 Schnorr signature over the Merkle root of
// a BOLT 12 message.
func verifySignature(messageName, fieldName string, root [32]byte, sig [64]byte,
	pubKey *btcec.PublicKey) error {

	if pubKey == nil {
		return ErrNilPublicKey
	}

	tag := signatureTagPrefix + messageName + fieldName
	digest := taggedHash(tag, root[:])

	parsedSig, err := schnorr.ParseSignature(sig[:])
	if err != nil {
		return fmt.Errorf("parse signature: %w", err)
	}

	if !parsedSig.Verify(digest[:], pubKey) {
		return ErrInvalidSignature
	}

	return nil
}

// SignInvoiceRequest computes the Merkle root of an invoice request and
// generates a Schnorr signature using the provided private key. The root is
// computed over the signable subset of AllRecords().
func SignInvoiceRequest(ir *InvoiceRequest, privKey *btcec.PrivateKey) (
	[64]byte, error) {

	if privKey == nil {
		return [64]byte{}, ErrNilPrivateKey
	}

	root, err := merkleRoot(signableTLVs(ir.AllRecords()))
	if err != nil {
		return [64]byte{}, err
	}

	return signMessage(
		"invoice_request", "signature", root, privKey,
	)
}

// VerifyInvoiceRequest verifies the signature on an invoice request using its
// invreq_payer_id public key.
func VerifyInvoiceRequest(ir *InvoiceRequest) error {
	pubKey, err := ir.InvreqPayerID.UnwrapOrErrV(ErrMissingPayerID)
	if err != nil {
		return err
	}
	if pubKey == nil {
		return fmt.Errorf("%w: invreq_payer_id", ErrNilPublicKey)
	}

	sig, err := ir.Signature.UnwrapOrErrV(ErrMissingSignature)
	if err != nil {
		return err
	}

	root, err := merkleRoot(signableTLVs(ir.AllRecords()))
	if err != nil {
		return err
	}

	return verifySignature(
		"invoice_request", "signature", root, sig, pubKey,
	)
}

// SignInvoice computes the Merkle root of an invoice and generates a Schnorr
// signature using the provided private key. The root is computed over the
// signable subset of AllRecords().
func SignInvoice(inv *Invoice, privKey *btcec.PrivateKey) ([64]byte, error) {
	if privKey == nil {
		return [64]byte{}, ErrNilPrivateKey
	}

	root, err := merkleRoot(signableTLVs(inv.AllRecords()))
	if err != nil {
		return [64]byte{}, err
	}

	return signMessage("invoice", "signature", root, privKey)
}

// VerifyInvoice verifies the signature on an invoice using its invoice_node_id
// public key.
func VerifyInvoice(inv *Invoice) error {
	pubKey, err := inv.InvoiceNodeID.UnwrapOrErrV(ErrMissingNodeID)
	if err != nil {
		return err
	}
	if pubKey == nil {
		return fmt.Errorf("%w: invoice_node_id", ErrNilPublicKey)
	}

	sig, err := inv.Signature.UnwrapOrErrV(ErrMissingSignature)
	if err != nil {
		return err
	}

	root, err := merkleRoot(signableTLVs(inv.AllRecords()))
	if err != nil {
		return err
	}

	return verifySignature("invoice", "signature", root, sig, pubKey)
}
