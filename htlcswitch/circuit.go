package htlcswitch

import (
	"encoding/binary"
	"fmt"
	"io"

	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lnd/lnwire"
)

// ErrInvalidCircuitKeyLen signals that a circuit key could not be decoded
// because the byte slice is of an invalid length.
var ErrInvalidCircuitKeyLen = errors.New("length of serialized circuit " +
	"key must be 16 bytes")

// EmptyKeystone is a default value returned when a circuit's keystone has not
// been set. Note that this value is invalid for use as a keystone, since the
// outgoing link must not be equal to sourceHop.
var EmptyKeystone CircuitKey

// CircuitKey is a tuple of channel ID and HTLC ID, used to uniquely identify
// HTLCs in a circuit. Circuits are identified primarily by the circuit key of
// the incoming HTLC. However, a circuit may also be referenced by its outgoing
// circuit key (keystone) after the HTLC has been forwarded via the outgoing
// link.
type CircuitKey struct {
	ChanID lnwire.ShortChannelID
	HtlcID uint64
}

// SetBytes deserializes the given bytes into this CircuitKey.
func (k *CircuitKey) SetBytes(bs []byte) error {
	if len(bs) != 16 {
		return ErrInvalidCircuitKeyLen
	}

	k.ChanID = lnwire.NewShortChanIDFromInt(
		binary.BigEndian.Uint64(bs[:8]))
	k.HtlcID = binary.BigEndian.Uint64(bs[8:])

	return nil
}

// Bytes returns the serialized bytes for this circuit key.
func (k CircuitKey) Bytes() []byte {
	var bs = make([]byte, 16)
	binary.BigEndian.PutUint64(bs[:8], k.ChanID.ToUint64())
	binary.BigEndian.PutUint64(bs[8:], k.HtlcID)
	return bs
}

// Encode writes a CircuitKey to the provided io.Writer.
func (k *CircuitKey) Encode(w io.Writer) error {
	var scratch [16]byte
	binary.BigEndian.PutUint64(scratch[:8], k.ChanID.ToUint64())
	binary.BigEndian.PutUint64(scratch[8:], k.HtlcID)

	_, err := w.Write(scratch[:])
	return err
}

// Decode reads a CircuitKey from the provided io.Reader.
func (k *CircuitKey) Decode(r io.Reader) error {
	var scratch [16]byte

	if _, err := io.ReadFull(r, scratch[:]); err != nil {
		return err
	}
	k.ChanID = lnwire.NewShortChanIDFromInt(
		binary.BigEndian.Uint64(scratch[:8]))
	k.HtlcID = binary.BigEndian.Uint64(scratch[8:])

	return nil
}

// String returns a string representation of the CircuitKey.
func (k CircuitKey) String() string {
	return fmt.Sprintf("(Chan ID=%s, HTLC ID=%d)", k.ChanID, k.HtlcID)
}

// PaymentCircuit is used by the switch as placeholder between when the
// switch makes a forwarding decision and the outgoing link determines the
// proper HTLC ID for the local log. After the outgoing HTLC ID has been
// determined, the half circuit will be converted into a full PaymentCircuit.
type PaymentCircuit struct {
	// Incoming is the circuit key identifying the incoming channel and htlc
	// index from which this ADD originates.
	Incoming CircuitKey

	// Outgoing is the circuit key identifying the outgoing channel, and the
	// HTLC index that was used to forward the ADD. It will be nil if this
	// circuit's keystone has not been set.
	Outgoing *CircuitKey

	// PaymentHash used as unique identifier of payment.
	PaymentHash [32]byte

	// IncomingAmount is the value of the HTLC from the incoming link.
	IncomingAmount lnwire.MilliSatoshi

	// OutgoingAmount specifies the value of the HTLC leaving the switch,
	// either as a payment or forwarded amount.
	OutgoingAmount lnwire.MilliSatoshi

	// ErrorEncrypter is used to re-encrypt the onion failure before
	// sending it back to the originator of the payment.
	ErrorEncrypter ErrorEncrypter
}

// HasKeystone returns true if an outgoing link has assigned an this circuit's
// outgoing circuit key.
func (c *PaymentCircuit) HasKeystone() bool {
	return c.Outgoing != nil
}

// newPaymentCircuit initalizes apayment circuit on the heap using the payment
// hash and an in-memory htlc packet.
func newPaymentCircuit(hash *[32]byte, pkt *htlcPacket) *PaymentCircuit {
	return &PaymentCircuit{
		Incoming: CircuitKey{
			ChanID: pkt.incomingChanID,
			HtlcID: pkt.incomingHTLCID,
		},
		PaymentHash:    *hash,
		IncomingAmount: pkt.incomingAmount,
		OutgoingAmount: pkt.amount,
		ErrorEncrypter: pkt.obfuscator,
	}
}

// makePaymentCircuit initalizes a payment circuit on the stack using the
// payment hash and an in-memory htlc packet.
func makePaymentCircuit(hash *[32]byte, pkt *htlcPacket) PaymentCircuit {
	return PaymentCircuit{
		Incoming: CircuitKey{
			ChanID: pkt.incomingChanID,
			HtlcID: pkt.incomingHTLCID,
		},
		PaymentHash:    *hash,
		IncomingAmount: pkt.incomingAmount,
		OutgoingAmount: pkt.amount,
		ErrorEncrypter: pkt.obfuscator,
	}
}

// Encode writes a PaymentCircuit to the provided io.Writer.
func (c *PaymentCircuit) Encode(w io.Writer) error {
	if err := c.Incoming.Encode(w); err != nil {
		return err
	}

	if _, err := w.Write(c.PaymentHash[:]); err != nil {
		return err
	}

	var scratch [8]byte

	binary.BigEndian.PutUint64(scratch[:], uint64(c.IncomingAmount))
	if _, err := w.Write(scratch[:]); err != nil {
		return err
	}

	binary.BigEndian.PutUint64(scratch[:], uint64(c.OutgoingAmount))
	if _, err := w.Write(scratch[:]); err != nil {
		return err
	}

	// Defaults to EncrypterTypeNone.
	var encrypterType EncrypterType
	if c.ErrorEncrypter != nil {
		encrypterType = c.ErrorEncrypter.Type()
	}

	err := binary.Write(w, binary.BigEndian, encrypterType)
	if err != nil {
		return err
	}

	// Skip encoding of error encrypter if this half add does not have one.
	if encrypterType == EncrypterTypeNone {
		return nil
	}

	return c.ErrorEncrypter.Encode(w)
}

// Decode reads a PaymentCircuit from the provided io.Reader.
func (c *PaymentCircuit) Decode(r io.Reader) error {
	if err := c.Incoming.Decode(r); err != nil {
		return err
	}

	if _, err := io.ReadFull(r, c.PaymentHash[:]); err != nil {
		return err
	}

	var scratch [8]byte

	if _, err := io.ReadFull(r, scratch[:]); err != nil {
		return err
	}
	c.IncomingAmount = lnwire.MilliSatoshi(
		binary.BigEndian.Uint64(scratch[:]))

	if _, err := io.ReadFull(r, scratch[:]); err != nil {
		return err
	}
	c.OutgoingAmount = lnwire.MilliSatoshi(
		binary.BigEndian.Uint64(scratch[:]))

	// Read the encrypter type used for this circuit.
	var encrypterType EncrypterType
	err := binary.Read(r, binary.BigEndian, &encrypterType)
	if err != nil {
		return err
	}

	switch encrypterType {
	case EncrypterTypeNone:
		// No encrypter was provided, such as when the payment is
		// locally initiated.
		return nil

	case EncrypterTypeSphinx:
		// Sphinx encrypter was used as this is a forwarded HTLC.
		c.ErrorEncrypter = NewSphinxErrorEncrypter()

	case EncrypterTypeMock:
		// Test encrypter.
		c.ErrorEncrypter = NewMockObfuscator()

	default:
		return UnknownEncrypterType(encrypterType)
	}

	return c.ErrorEncrypter.Decode(r)
}

// InKey returns the primary identifier for the circuit corresponding to the
// incoming HTLC.
func (c *PaymentCircuit) InKey() CircuitKey {
	return c.Incoming
}

// OutKey returns the keystone identifying the outgoing link and HTLC ID. If the
// circuit hasn't been completed, this method returns an EmptyKeystone, which is
// an invalid outgoing circuit key. Only call this method if HasKeystone returns
// true.
func (c *PaymentCircuit) OutKey() CircuitKey {
	if c.Outgoing != nil {
		return *c.Outgoing
	}

	return EmptyKeystone
}
