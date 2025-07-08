package hop

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tlv"
)

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

	// EncrypterTypeIntroduction is used to identify a sphinx onion error
	// encrypter where we are the introduction node in a blinded route. It
	// has the same functionality as EncrypterTypeSphinx, but is used to
	// mark our special-case error handling.
	EncrypterTypeIntroduction = 3

	// EncrypterTypeRelaying is used to identify a sphinx onion error
	// encryper where we are a relaying node in a blinded route. It has
	// the same functionality as a EncrypterTypeSphinx, but is used to mark
	// our special-case error handling.
	EncrypterTypeRelaying = 4

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
var (
	AttrErrorStruct = sphinx.NewAttrErrorStructure(20, 4, 4)
)

// IsBlinded returns a boolean indicating whether the error encrypter belongs
// to a blinded route.
func (e EncrypterType) IsBlinded() bool {
	return e == EncrypterTypeIntroduction || e == EncrypterTypeRelaying
}

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
	EncryptFirstHop(lnwire.FailureMessage) (lnwire.OpaqueReason,
		[]byte, error)

	// EncryptMalformedError is similar to EncryptFirstHop (it adds the
	// MAC), but it accepts an opaque failure reason rather than a failure
	// message. This method is used when we receive an
	// UpdateFailMalformedHTLC from the remote peer and then need to
	// convert that into a proper error from only the raw bytes.
	EncryptMalformedError(lnwire.OpaqueReason) (lnwire.OpaqueReason, []byte,
		error)

	// IntermediateEncrypt wraps an already encrypted opaque reason error
	// in an additional layer of onion encryption. This process repeats
	// until the error arrives at the source of the payment.
	IntermediateEncrypt(lnwire.OpaqueReason, []byte) (lnwire.OpaqueReason,
		[]byte, error)

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
	*sphinx.OnionErrorEncrypter

	EphemeralKey *btcec.PublicKey
	CreatedAt    time.Time
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
	sharedSecret sphinx.Hash256) *SphinxErrorEncrypter {

	encrypter := &SphinxErrorEncrypter{
		EphemeralKey: ephemeralKey,
	}

	// Set creation time rounded to nanosecond to avoid differences after
	// serialization.
	encrypter.CreatedAt = time.Now().Truncate(time.Nanosecond)

	encrypter.initialize(sharedSecret)

	return encrypter
}

// getHoldTime returns the hold time in decaseconds since the first
// instantiation of this sphinx error encrypter.
func (s *SphinxErrorEncrypter) getHoldTime() uint32 {
	return uint32(time.Since(s.CreatedAt).Milliseconds() / 100)
}

// encrypt is a thin wrapper around the main encryption method, mainly used to
// automatically derive the hold time to encode in the attribution structure.
func (s *SphinxErrorEncrypter) encrypt(initial bool,
	data, attrData []byte) (lnwire.OpaqueReason, []byte, error) {

	holdTime := s.getHoldTime()

	return s.EncryptError(initial, data, attrData, holdTime)
}

// initialize creates the underlying instance of the sphinx error encrypter.
func (s *SphinxErrorEncrypter) initialize(sharedSecret sphinx.Hash256) {
	s.OnionErrorEncrypter = sphinx.NewOnionErrorEncrypter(
		sharedSecret, AttrErrorStruct,
	)
}

// EncryptFirstHop transforms a concrete failure message into an encrypted
// opaque failure reason. This method will be used at the source that the error
// occurs. It differs from BackwardObfuscate slightly, in that it computes a
// proper MAC over the error.
//
// NOTE: Part of the ErrorEncrypter interface.
func (s *SphinxErrorEncrypter) EncryptFirstHop(
	failure lnwire.FailureMessage) (lnwire.OpaqueReason, []byte, error) {

	var b bytes.Buffer
	if err := lnwire.EncodeFailure(&b, failure, 0); err != nil {
		return nil, nil, err
	}

	return s.encrypt(true, b.Bytes(), nil)
}

// EncryptMalformedError is similar to EncryptFirstHop (it adds the MAC), but
// it accepts an opaque failure reason rather than a failure message. This
// method is used when we receive an UpdateFailMalformedHTLC from the remote
// peer and then need to convert that into an proper error from only the raw
// bytes.
//
// NOTE: Part of the ErrorEncrypter interface.
func (s *SphinxErrorEncrypter) EncryptMalformedError(
	reason lnwire.OpaqueReason) (lnwire.OpaqueReason, []byte, error) {

	return s.encrypt(true, reason, nil)
}

// IntermediateEncrypt wraps an already encrypted opaque reason error in an
// additional layer of onion encryption. This process repeats until the error
// arrives at the source of the payment. We re-encrypt the message on the
// backwards path to ensure that the error is indistinguishable from any other
// error seen.
//
// NOTE: Part of the ErrorEncrypter interface.
func (s *SphinxErrorEncrypter) IntermediateEncrypt(
	reason lnwire.OpaqueReason, attrData []byte) (lnwire.OpaqueReason,
	[]byte, error) {

	encrypted, attrData, err := s.encrypt(false, reason, attrData)

	switch {
	// If the structure of the error received from downstream is invalid,
	// then generate a new attribution structure so that the sender is able
	// to penalize the offending node.
	case errors.Is(err, sphinx.ErrInvalidAttrStructure):
		// Preserve the error message and initialize fresh attribution
		// data.
		return s.encrypt(true, reason, nil)

	case err != nil:
		return lnwire.OpaqueReason{}, nil, err
	}

	return encrypted, attrData, nil
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

	// Set attributable error creation time.
	s.CreatedAt = time.Unix(0, int64(creationTime))

	return nil
}

// Reextract rederives the error encrypter from the currently held EphemeralKey.
// This intended to be used shortly after Decode, to fully initialize a
// SphinxErrorEncrypter.
func (s *SphinxErrorEncrypter) Reextract(extract SharedSecretGenerator) error {
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

// A compile time check to ensure that IntroductionErrorEncrypter implements
// the ErrorEncrypter interface.
var _ ErrorEncrypter = (*IntroductionErrorEncrypter)(nil)

// IntroductionErrorEncrypter is a wrapper type on SphinxErrorEncrypter which
// is used to signal that we have special HTLC error handling for this hop.
type IntroductionErrorEncrypter struct {
	// ErrorEncrypter is the underlying error encrypter, embedded
	// directly in the struct so that we don't have to re-implement the
	// ErrorEncrypter interface.
	ErrorEncrypter
}

// NewIntroductionErrorEncrypter returns a blank IntroductionErrorEncrypter.
func NewIntroductionErrorEncrypter(ephemeralKey *btcec.PublicKey,
	sharedSecret sphinx.Hash256) *IntroductionErrorEncrypter {

	return &IntroductionErrorEncrypter{
		ErrorEncrypter: NewSphinxErrorEncrypter(
			ephemeralKey, sharedSecret,
		),
	}
}

// NewIntroductionErrorEncrypter returns a blank IntroductionErrorEncrypter.
// Since the actual encrypter is not stored in plaintext
// while at rest, reconstructing the error encrypter requires:
//  1. Decode: to deserialize the ephemeral public key.
//  2. Reextract: to "unlock" the actual error encrypter using an active
//     OnionProcessor.
func NewIntroductionErrorEncrypterUninitialized() *IntroductionErrorEncrypter {
	return &IntroductionErrorEncrypter{
		ErrorEncrypter: NewSphinxErrorEncrypterUninitialized(),
	}
}

// Type returns the identifier for an introduction error encrypter.
func (i *IntroductionErrorEncrypter) Type() EncrypterType {
	return EncrypterTypeIntroduction
}

// Reextract rederives the error encrypter from the currently held EphemeralKey,
// relying on the logic in the underlying SphinxErrorEncrypter.
func (i *IntroductionErrorEncrypter) Reextract(
	extract SharedSecretGenerator) error {

	return i.ErrorEncrypter.Reextract(extract)
}

// A compile time check to ensure that RelayingErrorEncrypte implements
// the ErrorEncrypter interface.
var _ ErrorEncrypter = (*RelayingErrorEncrypter)(nil)

// RelayingErrorEncrypter is a wrapper type on SphinxErrorEncrypter which
// is used to signal that we have special HTLC error handling for this hop.
type RelayingErrorEncrypter struct {
	ErrorEncrypter
}

// NewRelayingErrorEncrypter returns a blank RelayingErrorEncrypter with
// an underlying SphinxErrorEncrypter.
func NewRelayingErrorEncrypter(ephemeralKey *btcec.PublicKey,
	sharedSecret sphinx.Hash256) *RelayingErrorEncrypter {

	return &RelayingErrorEncrypter{
		ErrorEncrypter: NewSphinxErrorEncrypter(
			ephemeralKey, sharedSecret,
		),
	}
}

// NewRelayingErrorEncrypterUninitialized returns a blank RelayingErrorEncrypter
// with an underlying SphinxErrorEncrypter.
// Since the actual encrypter is not stored in plaintext
// while at rest, reconstructing the error encrypter requires:
//  1. Decode: to deserialize the ephemeral public key.
//  2. Reextract: to "unlock" the actual error encrypter using an active
//     OnionProcessor.
func NewRelayingErrorEncrypterUninitialized() *RelayingErrorEncrypter {
	return &RelayingErrorEncrypter{
		ErrorEncrypter: NewSphinxErrorEncrypterUninitialized(),
	}
}

// Type returns the identifier for a relaying error encrypter.
func (r *RelayingErrorEncrypter) Type() EncrypterType {
	return EncrypterTypeRelaying
}

// Reextract rederives the error encrypter from the currently held EphemeralKey,
// relying on the logic in the underlying SphinxErrorEncrypter.
func (r *RelayingErrorEncrypter) Reextract(
	extract SharedSecretGenerator) error {

	return r.ErrorEncrypter.Reextract(extract)
}
