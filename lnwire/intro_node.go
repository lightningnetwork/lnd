package lnwire

import (
	"bytes"
	"fmt"
	"io"

	"github.com/btcsuite/btcd/btcec/v2"
)

// IntroductionNode is the sealed sum-type for a blinded path's introduction
// node. {0x02, 0x03} → PubkeyIntro; {0x00, 0x01} → SciddirIntro. The unexported
// method seals the variant set so foreign packages cannot satisfy the interface
// with an unrecognised wire form.
type IntroductionNode interface {
	isIntroductionNode()

	encodedLen() uint64

	encode(w io.Writer) error

	// validate checks that the discriminator byte is valid for the variant.
	validate() error

	// Bytes returns the wire-format encoding of the introduction node for
	// callers that need it outside an io.Writer (RPC surfaces).
	Bytes() []byte
}

// PubkeyIntro is the 33-byte compressed-pubkey variant. The SEC1 parity byte
// (0x02 or 0x03) doubles as the wire discriminator. Use the constructor to
// ensure the non-nil, on-curve invariant is upheld.
type PubkeyIntro struct {
	Pubkey *btcec.PublicKey
}

// NewPubkeyIntro constructs the pubkey introduction-node variant, establishing
// the non-nil, on-curve invariant at construction time so callers receive an
// error up front rather than relying on every method to re-guard a nil key.
func NewPubkeyIntro(pubkey *btcec.PublicKey) (PubkeyIntro, error) {
	p := PubkeyIntro{Pubkey: pubkey}
	if err := p.validate(); err != nil {
		return PubkeyIntro{}, err
	}

	return p, nil
}

// SciddirIntro is the 9-byte sciddir variant. Direction is the wire
// discriminator; SCID is the 8-byte short channel ID. Use the constructor to
// ensure the direction is valid at construction time.
type SciddirIntro struct {
	Direction byte
	SCID      [scidLen]byte
}

// NewSciddirIntro constructs the sciddir introduction-node variant, rejecting
// an invalid direction discriminator at construction time.
func NewSciddirIntro(direction byte, scid [scidLen]byte) (SciddirIntro, error) {
	s := SciddirIntro{Direction: direction, SCID: scid}
	if err := s.validate(); err != nil {
		return SciddirIntro{}, err
	}

	return s, nil
}

var (
	_ IntroductionNode = PubkeyIntro{}
	_ IntroductionNode = SciddirIntro{}
)

// decodeIntroductionNode reads the discriminator byte and dispatches to the
// matching variant.
func decodeIntroductionNode(r io.Reader,
	buf *[8]byte) (IntroductionNode, error) {

	if _, err := io.ReadFull(r, buf[:1]); err != nil {
		return nil, fmt.Errorf("read intro node type: %w", err)
	}

	disc := buf[0]
	switch disc {
	case 0x00, 0x01:
		var scid [scidLen]byte
		if _, err := io.ReadFull(r, scid[:]); err != nil {
			return nil, fmt.Errorf("read sciddir: %w", err)
		}

		return NewSciddirIntro(disc, scid)

	case 0x02, 0x03:
		var b [pubKeyLen]byte
		b[0] = disc
		if _, err := io.ReadFull(r, b[1:]); err != nil {
			return nil, fmt.Errorf("read intro pubkey: %w", err)
		}
		pub, err := btcec.ParsePubKey(b[:])
		if err != nil {
			return nil, fmt.Errorf("%w: %w",
				ErrInvalidIntroNode, err)
		}

		return NewPubkeyIntro(pub)

	default:
		return nil, fmt.Errorf("%w: 0x%02x", ErrInvalidIntroNode, disc)
	}
}

func (PubkeyIntro) isIntroductionNode() {}

func (p PubkeyIntro) encodedLen() uint64 { return pubKeyLen }

func (p PubkeyIntro) encode(w io.Writer) error {
	if p.Pubkey == nil {
		return fmt.Errorf("nil intro pubkey")
	}
	_, err := w.Write(p.Pubkey.SerializeCompressed())

	return err
}

func (p PubkeyIntro) validate() error {
	if p.Pubkey == nil {
		return fmt.Errorf("%w: nil pubkey", ErrInvalidIntroNode)
	}

	if !p.Pubkey.IsOnCurve() {
		return fmt.Errorf("%w: pubkey not on curve",
			ErrInvalidIntroNode)
	}

	return nil
}

// Bytes returns the wire-format encoding of the pubkey variant.
func (p PubkeyIntro) Bytes() []byte {
	var buf bytes.Buffer
	buf.Grow(pubKeyLen)

	// We ignore errors because we have validated that the pubkey is non nil
	// at construction time.
	_ = p.encode(&buf)

	return buf.Bytes()
}

func (SciddirIntro) isIntroductionNode() {}

func (s SciddirIntro) encodedLen() uint64 { return sciddirLen }

func (s SciddirIntro) encode(w io.Writer) error {
	if _, err := w.Write([]byte{s.Direction}); err != nil {
		return err
	}
	_, err := w.Write(s.SCID[:])

	return err
}

func (s SciddirIntro) validate() error {
	switch s.Direction {
	case 0x00, 0x01:
		return nil
	}

	return fmt.Errorf("%w: 0x%02x", ErrInvalidIntroNode, s.Direction)
}

// Bytes returns the wire-format encoding of the sciddir variant.
func (s SciddirIntro) Bytes() []byte {
	var buf bytes.Buffer
	buf.Grow(sciddirLen)

	// We ignore the error because encode only writes the fixed-size
	// direction and SCID to an in-memory buffer, which cannot fail.
	_ = s.encode(&buf)

	return buf.Bytes()
}
