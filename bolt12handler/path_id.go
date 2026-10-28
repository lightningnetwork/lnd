package bolt12handler

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightningnetwork/lnd/lntypes"
)

const (
	// pathIDKeyTag tags the hash of the node secret that path ids are
	// computed with.
	pathIDKeyTag = "bolt12/path-id"

	// pathIDHalfLen is the byte length of each half of a path id.
	pathIDHalfLen = 16

	// pathIDLen is the byte length of a path id that this node creates.
	pathIDLen = 2 * pathIDHalfLen
)

// PathIDContext names the kind of blinded path that a path id belongs to. A
// path id of one kind never verifies as a path id of another kind.
type PathIDContext string

const (
	// PathIDOffer marks the offer_paths of an offer. The binding is the
	// offer hash without offer_paths.
	PathIDOffer PathIDContext = "offer"

	// PathIDReply marks the reply path of an invoice_request. The binding
	// is the invoice request hash.
	PathIDReply PathIDContext = "reply"

	// pathIDPayment marks the payment paths of an invoice. The binding is
	// the hash of the invoice envelope. Only the payment path id methods
	// use it.
	pathIDPayment PathIDContext = "payment"
)

// PathIDKey computes and checks the path ids that this node puts into the
// final hop of the blinded paths that lead to it. A path id is a nonce and a
// tag. The nonce comes from the message that the path belongs to. The tag
// comes from the nonce. The tag alone shows that this node made the path. The
// nonce shows which message the path belongs to. The node stores nothing for
// either check. A payment path id has a random nonce instead. See
// newPaymentPathID.
//
// The zero key is public, so it is unusable. No check accepts a path id under
// it, and newPaymentPathID returns an error for it. Only DerivePathIDKey gives
// a real key.
type PathIDKey struct {
	key [32]byte
}

// isZero reports whether k is the zero key, under which anybody can compute
// path ids.
func (k PathIDKey) isZero() bool {
	return k.key == [32]byte{}
}

// DerivePathIDKey derives the path id key from the node key. The node key
// stays behind ecdh, so this works with a remote signer.
func DerivePathIDKey(ecdh ECDHer) (PathIDKey, error) {
	// signrpc.DeriveSharedKey returns the same value for this point, so a
	// caller with signer:generate can derive the key. That permission can
	// already sign invoices with the node key, so the key adds no power.
	shared, err := ecdh.ECDH(nodeSecretPoint)
	if err != nil {
		return PathIDKey{}, fmt.Errorf("path id key ECDH: %w", err)
	}

	return PathIDKey{
		key: *chainhash.TaggedHash([]byte(pathIDKeyTag), shared[:]),
	}, nil
}

// New returns the path id for a path of kind ctx that belongs to the message
// with the given binding.
func (k PathIDKey) New(ctx PathIDContext, binding [32]byte) [pathIDLen]byte {
	var id [pathIDLen]byte
	copy(id[:pathIDHalfLen], k.mac('n', ctx, binding[:]))
	copy(id[pathIDHalfLen:], k.mac('t', ctx, id[:pathIDHalfLen]))

	return id
}

// isOurs reports whether id is a path id of kind ctx that this node made. It
// needs no binding, so it answers for any path of that kind.
func (k PathIDKey) isOurs(ctx PathIDContext, id []byte) bool {
	if k.isZero() || len(id) != pathIDLen {
		return false
	}

	tag := k.mac('t', ctx, id[:pathIDHalfLen])

	return hmac.Equal(tag, id[pathIDHalfLen:])
}

// matches reports whether id is the path id of kind ctx that this node made
// for the message with the given binding.
func (k PathIDKey) matches(ctx PathIDContext, id []byte,
	binding [32]byte) bool {

	if k.isZero() {
		return false
	}

	want := k.New(ctx, binding)

	return hmac.Equal(want[:], id)
}

// newPaymentPathID returns a new path id for a blinded payment path. The final
// hop of the path carries the invoice envelope with the given binding. It
// returns an error for the zero key.
//
// The tag covers the random nonce and the binding. The nonce feeds the
// preimage, and the path id is shown as the payment address. The nonce is
// random, so two equal invoice requests get two different preimages. Offer and
// reply paths derive their nonce from their message instead.
func (k PathIDKey) newPaymentPathID(binding [32]byte) ([pathIDLen]byte,
	error) {

	var id [pathIDLen]byte
	if k.isZero() {
		return id, errors.New("zero path id key")
	}

	if _, err := rand.Read(id[:pathIDHalfLen]); err != nil {
		return id, fmt.Errorf("payment path id nonce: %w", err)
	}

	tag := k.mac('t', pathIDPayment, paymentInput(id[:], binding))
	copy(id[pathIDHalfLen:], tag)

	return id, nil
}

// checkPaymentPathID reports whether id is a payment path id that this node
// made for the given binding. Only then does it return the preimage of the
// payment.
func (k PathIDKey) checkPaymentPathID(id []byte,
	binding [32]byte) (lntypes.Preimage, bool) {

	if k.isZero() || len(id) != pathIDLen {
		return lntypes.Preimage{}, false
	}

	input := paymentInput(id, binding)
	tag := k.mac('t', pathIDPayment, input)
	if !hmac.Equal(tag, id[pathIDHalfLen:]) {
		return lntypes.Preimage{}, false
	}

	return lntypes.Preimage(k.sum('p', pathIDPayment, input)), true
}

// paymentInput returns the nonce of the payment path id followed by the
// binding.
func paymentInput(id []byte, binding [32]byte) []byte {
	input := make([]byte, 0, pathIDHalfLen+len(binding))
	input = append(input, id[:pathIDHalfLen]...)

	return append(input, binding[:]...)
}

// mac returns the first half of the HMAC of the label, the context and data.
func (k PathIDKey) mac(label byte, ctx PathIDContext, data []byte) []byte {
	return k.sum(label, ctx, data)[:pathIDHalfLen]
}

// sum returns the HMAC of the label, the context and data. The context
// carries its length, so two contexts never give the same input.
func (k PathIDKey) sum(label byte, ctx PathIDContext, data []byte) []byte {
	h := hmac.New(sha256.New, k.key[:])
	h.Write([]byte{label, byte(len(ctx))})
	h.Write([]byte(ctx))
	h.Write(data)

	return h.Sum(nil)
}
