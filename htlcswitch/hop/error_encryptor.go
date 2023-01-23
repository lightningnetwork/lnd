package hop

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tlv"
)

const (
	// A set of tlv type definitions used to serialize the encrypter to the
	// database.
	//
	// NOTE: A migration should be added whenever this list changes. This
	// prevents against the database being rolled back to an older
	// format where the surrounding logic might assume a different set of
	// fields are known.
	creationTimeType tlv.Type = 0
)

// AttrErrorStruct defines the message structure for an attributable error. Use
// a maximum route length of 20, a fixed payload length of 4 bytes to
// accommodate the a 32-bit hold time in milliseconds and use 4 byte hmacs.
// Total size including a 256 byte message from the error source works out to
// 1200 bytes.
var AttrErrorStruct = sphinx.NewAttrErrorStructure(20, 4, 4)

// EncrypterType establishes an enum used in serialization to indicate how to
// decode a concrete instance of the ErrorEncrypter interface.
type EncrypterType byte

const (
	// EncrypterTypeNone signals that no error encyrpter is present, this
	// can happen if the htlc is originates in the switch.
	EncrypterTypeNone EncrypterType = 0

	// EncrypterTypeSphinx is used to identify a sphinx onion error
	// encrypter instance.
	EncrypterTypeSphinx = 1

	// EncrypterTypeMock is used to identify a mock obfuscator instance.
	EncrypterTypeMock = 2
)

var byteOrder = binary.BigEndian

// SharedSecretGenerator defines a function signature that extracts a shared
// secret from an sphinx OnionPacket.
type SharedSecretGenerator func(*btcec.PublicKey) (sphinx.Hash256,
	lnwire.FailCode)

// ErrorEncrypter is an interface that is used to encrypt HTLC related errors
// at the source of the error, and also at each intermediate hop all the way
// back to the source of the payment.
type ErrorEncrypter interface {
	// EncryptFirstHop transforms a concrete failure message into an
	// encrypted opaque failure reason. This method will be used at the
	// source that the error occurs. It differs from IntermediateEncrypt
	// slightly, in that it computes a proper MAC over the error.
	EncryptFirstHop(lnwire.FailureMessage) (lnwire.OpaqueReason, error)

	// EncryptMalformedError is similar to EncryptFirstHop (it adds the
	// MAC), but it accepts an opaque failure reason rather than a failure
	// message. This method is used when we receive an
	// UpdateFailMalformedHTLC from the remote peer and then need to
	// convert that into a proper error from only the raw bytes.
	EncryptMalformedError(lnwire.OpaqueReason) (lnwire.OpaqueReason, error)

	// IntermediateEncrypt wraps an already encrypted opaque reason error
	// in an additional layer of onion encryption. This process repeats
	// until the error arrives at the source of the payment.
	IntermediateEncrypt(lnwire.OpaqueReason) (lnwire.OpaqueReason, error)

	// Type returns an enum indicating the underlying concrete instance
	// backing this interface.
	Type() EncrypterType

	// Encode serializes the encrypter's ephemeral public key to the given
	// io.Writer.
	Encode(io.Writer) error

	// Decode deserializes the encrypter' ephemeral public key from the
	// given io.Reader.
	Decode(io.Reader) error

	// Reextract rederives the encrypter using the shared secret generator,
	// performing an ECDH with the sphinx router's key and the ephemeral
	// public key.
	//
	// NOTE: This should be called shortly after Decode to properly
	// reinitialize the error encrypter.
	Reextract(SharedSecretGenerator) error
}

// SphinxErrorEncrypter is a concrete implementation of both the ErrorEncrypter
// interface backed by an implementation of the Sphinx packet format. As a
// result, all errors handled are themselves wrapped in layers of onion
// encryption and must be treated as such accordingly.
type SphinxErrorEncrypter struct {
	encrypter interface{}

	EphemeralKey *btcec.PublicKey
	CreatedAt    time.Time

	attrError bool
}

// NewSphinxErrorEncrypterUninitialized initializes a blank sphinx error
// encrypter, that should be used to deserialize an encoded
// SphinxErrorEncrypter. Since the actual encrypter is not stored in plaintext
// while at rest, reconstructing the error encrypter requires:
//  1. Decode: to deserialize the ephemeral public key.
//  2. Reextract: to "unlock" the actual error encrypter using an active
//     OnionProcessor.
func NewSphinxErrorEncrypterUninitialized() *SphinxErrorEncrypter {
	return &SphinxErrorEncrypter{
		EphemeralKey: &btcec.PublicKey{},
	}
}

// NewSphinxErrorEncrypter creates a new instance of a SphinxErrorEncrypter,
// initialized with the provided shared secret. To deserialize an encoded
// SphinxErrorEncrypter, use the NewSphinxErrorEncrypterUninitialized
// constructor.
func NewSphinxErrorEncrypter(ephemeralKey *btcec.PublicKey,
	sharedSecret sphinx.Hash256,
	attrError bool) *SphinxErrorEncrypter {

	encrypter := &SphinxErrorEncrypter{
		EphemeralKey: ephemeralKey,
		attrError:    attrError,
	}

	if attrError {
		// Set creation time rounded to nanosecond to avoid differences
		// after serialization.
		encrypter.CreatedAt = time.Now().Truncate(time.Nanosecond)
	}

	encrypter.initialize(sharedSecret)

	return encrypter
}

func (s *SphinxErrorEncrypter) initialize(sharedSecret sphinx.Hash256) {
	if s.attrError {
		s.encrypter = sphinx.NewOnionAttrErrorEncrypter(
			sharedSecret, AttrErrorStruct,
		)
	} else {
		s.encrypter = sphinx.NewOnionErrorEncrypter(
			sharedSecret,
		)
	}
}

func (s *SphinxErrorEncrypter) getHoldTimeMs() uint32 {
	return uint32(time.Since(s.CreatedAt).Milliseconds())
}

func (s *SphinxErrorEncrypter) encrypt(initial bool,
	data []byte) (lnwire.OpaqueReason, error) {

	switch encrypter := s.encrypter.(type) {
	case *sphinx.OnionErrorEncrypter:
		return encrypter.EncryptError(initial, data), nil

	case *sphinx.OnionAttrErrorEncrypter:
		// Pass hold time as the payload.
		holdTimeMs := s.getHoldTimeMs()

		var payload [4]byte
		byteOrder.PutUint32(payload[:], holdTimeMs)

		return encrypter.EncryptError(initial, data, payload[:])

	default:
		panic("unexpected encrypter type")
	}
}

// EncryptFirstHop transforms a concrete failure message into an encrypted
// opaque failure reason. This method will be used at the source that the error
// occurs. It differs from BackwardObfuscate slightly, in that it computes a
// proper MAC over the error.
//
// NOTE: Part of the ErrorEncrypter interface.
func (s *SphinxErrorEncrypter) EncryptFirstHop(
	failure lnwire.FailureMessage) (lnwire.OpaqueReason, error) {

	var b bytes.Buffer
	if err := lnwire.EncodeFailure(&b, failure, 0); err != nil {
		return nil, err
	}

	return s.encrypt(true, b.Bytes())
}

// EncryptMalformedError is similar to EncryptFirstHop (it adds the MAC), but
// it accepts an opaque failure reason rather than a failure message. This
// method is used when we receive an UpdateFailMalformedHTLC from the remote
// peer and then need to convert that into an proper error from only the raw
// bytes.
//
// NOTE: Part of the ErrorEncrypter interface.
func (s *SphinxErrorEncrypter) EncryptMalformedError(
	reason lnwire.OpaqueReason) (lnwire.OpaqueReason, error) {

	return s.encrypt(true, reason)
}

// IntermediateEncrypt wraps an already encrypted opaque reason error in an
// additional layer of onion encryption. This process repeats until the error
// arrives at the source of the payment. We re-encrypt the message on the
// backwards path to ensure that the error is indistinguishable from any other
// error seen.
//
// NOTE: Part of the ErrorEncrypter interface.
func (s *SphinxErrorEncrypter) IntermediateEncrypt(
	reason lnwire.OpaqueReason) (lnwire.OpaqueReason, error) {

	encrypted, err := s.encrypt(false, reason)
	switch {
	// If the structure of the error received from downstream is invalid,
	// then generate a new failure message with a valid structure so that
	// the sender is able to penalize the offending node.
	case errors.Is(err, sphinx.ErrInvalidStructure):
		// Use an all-zeroes failure message. This is not a defined
		// message, but the sender will at least know where the error
		// occurred.
		reason = make([]byte, lnwire.FailureMessageLength+2+2)

		return s.encrypt(true, reason)

	case err != nil:
		return lnwire.OpaqueReason{}, err
	}

	return encrypted, nil
}

// Type returns the identifier for a sphinx error encrypter.
func (s *SphinxErrorEncrypter) Type() EncrypterType {
	return EncrypterTypeSphinx
}

// Encode serializes the error encrypter' ephemeral public key to the provided
// io.Writer.
func (s *SphinxErrorEncrypter) Encode(w io.Writer) error {
	ephemeral := s.EphemeralKey.SerializeCompressed()
	_, err := w.Write(ephemeral)
	if err != nil {
		return err
	}

	// Stop here for legacy errors.
	if !s.attrError {
		return nil
	}

	var creationTime = uint64(s.CreatedAt.UnixNano())

	tlvStream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(creationTimeType, &creationTime),
	)
	if err != nil {
		return err
	}

	return tlvStream.Encode(w)
}

// Decode reconstructs the error encrypter's ephemeral public key from the
// provided io.Reader.
func (s *SphinxErrorEncrypter) Decode(r io.Reader) error {
	var ephemeral [33]byte
	if _, err := io.ReadFull(r, ephemeral[:]); err != nil {
		return err
	}

	var err error
	s.EphemeralKey, err = btcec.ParsePubKey(ephemeral[:])
	if err != nil {
		return err
	}

	// Try decode attributable error structure.
	var creationTime uint64

	tlvStream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(creationTimeType, &creationTime),
	)
	if err != nil {
		return err
	}

	typeMap, err := tlvStream.DecodeWithParsedTypes(r)
	if err != nil {
		return err
	}

	// Return early if this encrypter is not for attributable errors.
	if len(typeMap) == 0 {
		return nil
	}

	// Set attributable error flag and creation time.
	s.attrError = true
	s.CreatedAt = time.Unix(0, int64(creationTime))

	return nil
}

// Reextract rederives the error encrypter from the currently held EphemeralKey.
// This intended to be used shortly after Decode, to fully initialize a
// SphinxErrorEncrypter.
func (s *SphinxErrorEncrypter) Reextract(
	extract SharedSecretGenerator) error {

	sharedSecret, failcode := extract(s.EphemeralKey)
	if failcode != lnwire.CodeNone {
		// This should never happen, since we already validated that
		// this obfuscator can be extracted when it was received in the
		// link.
		return fmt.Errorf("unable to reconstruct onion "+
			"obfuscator, got failcode: %d", failcode)
	}

	s.initialize(sharedSecret)

	return nil
}

// A compile time check to ensure SphinxErrorEncrypter implements the
// ErrorEncrypter interface.
var _ ErrorEncrypter = (*SphinxErrorEncrypter)(nil)
