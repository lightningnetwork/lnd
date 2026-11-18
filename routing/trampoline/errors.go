package trampoline

import (
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/aead/chacha20"
	"github.com/btcsuite/btcd/btcec/v2"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/lnwire"
)

const (
	// FailurePacketSize is the fixed lnd BOLT 4 failure packet plus its
	// authentication tag.
	FailurePacketSize = sha256.Size + lnwire.FailureMessageLength + 4
)

var (
	// ErrInvalidFailurePacketSize is returned when a nested failure does
	// not have the fixed BOLT 4 wire size used by lnd.
	ErrInvalidFailurePacketSize = errors.New(
		"invalid trampoline failure packet size",
	)

	// ErrNoErrorPath is returned when shared-secret derivation is requested
	// for an empty path.
	ErrNoErrorPath = errors.New("empty trampoline error path")

	// ErrNestedErrorUndecryptable is returned when neither the outer nor
	// trampoline shared-secret sequence authenticates an error.
	ErrNestedErrorUndecryptable = errors.New(
		"unable to decrypt nested trampoline failure",
	)
)

// ErrorDomain identifies which onion layer authenticated an error.
type ErrorDomain uint8

const (
	// ErrorDomainOuter is an error authenticated by the ordinary payment
	// onion path.
	ErrorDomainOuter ErrorDomain = iota

	// ErrorDomainTrampoline is an error authenticated by the trampoline
	// onion path.
	ErrorDomainTrampoline
)

// DecryptedError is a nested failure together with its authenticated origin.
type DecryptedError struct {
	// Domain identifies the shared-secret sequence that authenticated the
	// failure.
	Domain ErrorDomain

	// SenderIndex is the zero-based sender position within Domain's path.
	SenderIndex int

	// Message is the encoded, fixed-size BOLT 4 failure message.
	Message []byte
}

// GenerateSharedSecrets derives the BOLT 4 per-hop secrets for an onion
// circuit. This is exposed locally because nested error decryption must peel
// every outer layer before attempting the trampoline secret sequence.
func GenerateSharedSecrets(sessionKey *btcec.PrivateKey,
	path []*btcec.PublicKey) ([][sha256.Size]byte, error) {

	if sessionKey == nil {
		return nil, fmt.Errorf("nil error-path session key")
	}
	if len(path) == 0 {
		return nil, ErrNoErrorPath
	}
	if len(path) > sphinx.NumMaxHops {
		return nil, fmt.Errorf("error path has %d hops, max is %d",
			len(path), sphinx.NumMaxHops)
	}

	secrets := make([][sha256.Size]byte, len(path))
	ephemeralKey := sessionKey

	for i, nodeID := range path {
		if nodeID == nil || !nodeID.IsOnCurve() {
			return nil, fmt.Errorf("invalid error-path node %d", i)
		}

		ecdh := &sphinx.PrivKeyECDH{PrivKey: ephemeralKey}
		secret, err := ecdh.ECDH(nodeID)
		if err != nil {
			return nil, fmt.Errorf("derive shared secret %d: %w",
				i, err)
		}
		secrets[i] = secret

		if i == len(path)-1 {
			continue
		}

		blindingHash := sha256.New()
		_, _ = blindingHash.Write(
			ephemeralKey.PubKey().SerializeCompressed(),
		)
		_, _ = blindingHash.Write(secret[:])

		var blinding btcec.ModNScalar
		if overflow := blinding.SetByteSlice(
			blindingHash.Sum(nil),
		); overflow || blinding.IsZero() {
			return nil, fmt.Errorf(
				"invalid blinding factor at hop %d", i,
			)
		}

		var next btcec.ModNScalar
		next.Set(&ephemeralKey.Key)
		next.Mul(&blinding)
		if next.IsZero() {
			return nil, fmt.Errorf(
				"zero ephemeral key at hop %d", i,
			)
		}

		ephemeralKey = btcec.PrivKeyFromScalar(&next)
	}

	return secrets, nil
}

// EncryptNestedFailure creates a fresh failure authenticated by the current
// trampoline hop, then wraps it for the current outer onion hop.
func EncryptNestedFailure(failureMessage []byte, trampolineSecret,
	outerSecret [sha256.Size]byte) ([]byte, error) {

	if len(failureMessage) != lnwire.FailureMessageLength+4 {
		return nil, fmt.Errorf("%w: failure=%d expected=%d",
			ErrInvalidFailurePacketSize, len(failureMessage),
			lnwire.FailureMessageLength+4)
	}

	encrypted, err := encryptInitialError(
		trampolineSecret, failureMessage,
	)
	if err != nil {
		return nil, err
	}

	return cryptError(outerSecret, encrypted)
}

// WrapNestedFailure applies an intermediate trampoline layer followed by its
// corresponding outer onion layer.
func WrapNestedFailure(reason []byte, trampolineSecret,
	outerSecret [sha256.Size]byte) ([]byte, error) {

	if len(reason) != FailurePacketSize {
		return nil, fmt.Errorf("%w: reason=%d expected=%d",
			ErrInvalidFailurePacketSize, len(reason),
			FailurePacketSize)
	}

	wrapped, err := cryptError(trampolineSecret, reason)
	if err != nil {
		return nil, err
	}

	return cryptError(outerSecret, wrapped)
}

// WrapOuterFailure applies only an outer onion layer. This form addresses the
// failure to the previous trampoline node instead of the original sender.
func WrapOuterFailure(reason []byte,
	outerSecret [sha256.Size]byte) ([]byte, error) {

	if len(reason) != FailurePacketSize {
		return nil, fmt.Errorf("%w: reason=%d expected=%d",
			ErrInvalidFailurePacketSize, len(reason),
			FailurePacketSize)
	}

	return cryptError(outerSecret, reason)
}

// PeelErrorPath removes one complete shared-secret sequence. If a layer
// authenticates, its failure message and sender are returned. Otherwise the
// fully peeled bytes can be passed to the trampoline secret sequence.
func PeelErrorPath(reason []byte,
	secrets [][sha256.Size]byte) ([]byte, *DecryptedError, error) {

	if len(reason) != FailurePacketSize {
		return nil, nil, fmt.Errorf("%w: reason=%d expected=%d",
			ErrInvalidFailurePacketSize, len(reason),
			FailurePacketSize)
	}
	if len(secrets) == 0 {
		return nil, nil, ErrNoErrorPath
	}
	if len(secrets) > sphinx.NumMaxHops {
		return nil, nil, fmt.Errorf(
			"error path has %d secrets, max is %d",
			len(secrets), sphinx.NumMaxHops,
		)
	}

	peeled := append([]byte(nil), reason...)
	for i, secret := range secrets {
		var err error
		peeled, err = cryptError(secret, peeled)
		if err != nil {
			return nil, nil, err
		}

		tag := peeled[:sha256.Size]
		message := peeled[sha256.Size:]
		umKey := errorKey("um", secret)
		mac := hmac.New(sha256.New, umKey[:])
		_, _ = mac.Write(message)
		if hmac.Equal(tag, mac.Sum(nil)) {
			return peeled, &DecryptedError{
				SenderIndex: i,
				Message: append(
					[]byte(nil), message...,
				),
			}, nil
		}
	}

	return peeled, nil, nil
}

// DecryptNestedError first exhausts the outer onion secret sequence and then,
// only if it did not authenticate, continues with the trampoline sequence.
func DecryptNestedError(reason []byte, outerSecrets,
	trampolineSecrets [][sha256.Size]byte) (*DecryptedError, error) {

	peeled, decrypted, err := PeelErrorPath(reason, outerSecrets)
	if err != nil {
		return nil, err
	}
	if decrypted != nil {
		decrypted.Domain = ErrorDomainOuter
		return decrypted, nil
	}

	_, decrypted, err = PeelErrorPath(peeled, trampolineSecrets)
	if err != nil {
		return nil, err
	}
	if decrypted == nil {
		return nil, ErrNestedErrorUndecryptable
	}

	decrypted.Domain = ErrorDomainTrampoline

	return decrypted, nil
}

func encryptInitialError(secret [sha256.Size]byte,
	message []byte) ([]byte, error) {

	umKey := errorKey("um", secret)
	mac := hmac.New(sha256.New, umKey[:])
	_, _ = mac.Write(message)

	authenticated := make([]byte, 0, FailurePacketSize)
	authenticated = append(authenticated, mac.Sum(nil)...)
	authenticated = append(authenticated, message...)

	return cryptError(secret, authenticated)
}

func cryptError(secret [sha256.Size]byte, data []byte) ([]byte, error) {
	ammagKey := errorKey("ammag", secret)
	var nonce [8]byte
	stream, err := chacha20.NewCipher(nonce[:], ammagKey[:])
	if err != nil {
		return nil, err
	}

	result := make([]byte, len(data))
	stream.XORKeyStream(result, data)

	return result, nil
}

func errorKey(label string,
	secret [sha256.Size]byte) [sha256.Size]byte {

	mac := hmac.New(sha256.New, []byte(label))
	_, _ = mac.Write(secret[:])

	var key [sha256.Size]byte
	copy(key[:], mac.Sum(nil))

	return key
}
