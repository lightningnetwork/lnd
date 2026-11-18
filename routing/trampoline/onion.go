package trampoline

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/tlv"
)

const (
	// OnionPacketType is the BOLT 4 TLV type for an embedded trampoline
	// onion packet.
	OnionPacketType tlv.Type = 20

	// onionPacketOverhead is the encoded version byte, ephemeral public
	// key, and header MAC outside of the variable routing information.
	onionPacketOverhead = 1 + 33 + sphinx.HMACSize

	// MinOnionPacketSize is the smallest structurally valid Sphinx packet.
	MinOnionPacketSize = onionPacketOverhead

	// MaxOnionPacketSize bounds untrusted embedded onions to the normal
	// BOLT 4 payment-onion ceiling.
	MaxOnionPacketSize = onionPacketOverhead +
		sphinx.MaxRoutingPayloadSize
)

var (
	// ErrOnionPacketTooSmall is returned when an embedded onion cannot
	// contain the fixed Sphinx fields.
	ErrOnionPacketTooSmall = errors.New("trampoline onion packet too small")

	// ErrOnionPacketTooLarge is returned when an embedded onion exceeds the
	// local payment-onion resource bound.
	ErrOnionPacketTooLarge = errors.New("trampoline onion packet too large")
)

// EncodeOnionPacket serializes a variable-sized trampoline onion packet while
// enforcing the local resource bound.
func EncodeOnionPacket(packet *sphinx.OnionPacket) ([]byte, error) {
	if packet == nil {
		return nil, fmt.Errorf("nil trampoline onion packet")
	}

	size := onionPacketOverhead + len(packet.RoutingInfo)
	if size > MaxOnionPacketSize {
		return nil, fmt.Errorf("%w: size=%d max=%d",
			ErrOnionPacketTooLarge, size, MaxOnionPacketSize)
	}

	var b bytes.Buffer
	b.Grow(size)
	if err := packet.Encode(&b); err != nil {
		return nil, fmt.Errorf("encode trampoline onion: %w", err)
	}

	return b.Bytes(), nil
}

// DecodeOnionPacket parses a complete variable-sized trampoline onion packet
// and rejects inputs outside of the payment-onion resource bound.
func DecodeOnionPacket(data []byte) (*sphinx.OnionPacket, error) {
	switch {
	case len(data) < MinOnionPacketSize:
		return nil, fmt.Errorf("%w: size=%d min=%d",
			ErrOnionPacketTooSmall, len(data), MinOnionPacketSize)

	case len(data) > MaxOnionPacketSize:
		return nil, fmt.Errorf("%w: size=%d max=%d",
			ErrOnionPacketTooLarge, len(data), MaxOnionPacketSize)
	}

	var packet sphinx.OnionPacket
	if err := packet.Decode(bytes.NewReader(data)); err != nil {
		return nil, fmt.Errorf("decode trampoline onion: %w", err)
	}

	return &packet, nil
}

// newOnionPacketRecord creates the type 20 codec used inside a hop TLV stream.
func newOnionPacketRecord(packet **sphinx.OnionPacket) tlv.Record {
	size := func() uint64 {
		if packet == nil || *packet == nil {
			return 0
		}

		return uint64(onionPacketOverhead + len((*packet).RoutingInfo))
	}

	encoder := func(w io.Writer, val interface{}, _ *[8]byte) error {
		target, ok := val.(**sphinx.OnionPacket)
		if !ok || target == nil || *target == nil {
			return tlv.NewTypeForEncodingErr(
				val, "**sphinx.OnionPacket",
			)
		}

		encoded, err := EncodeOnionPacket(*target)
		if err != nil {
			return err
		}

		_, err = w.Write(encoded)

		return err
	}

	decoder := func(r io.Reader, val interface{}, _ *[8]byte,
		l uint64) error {

		target, ok := val.(**sphinx.OnionPacket)
		if !ok || target == nil {
			return tlv.NewTypeForDecodingErr(
				val, "**sphinx.OnionPacket", l,
				MaxOnionPacketSize,
			)
		}

		if l < MinOnionPacketSize {
			return fmt.Errorf("%w: size=%d min=%d",
				ErrOnionPacketTooSmall, l, MinOnionPacketSize)
		}
		if l > MaxOnionPacketSize {
			return fmt.Errorf("%w: size=%d max=%d",
				ErrOnionPacketTooLarge, l, MaxOnionPacketSize)
		}

		data := make([]byte, l)
		if _, err := io.ReadFull(r, data); err != nil {
			return err
		}

		packet, err := DecodeOnionPacket(data)
		if err != nil {
			return err
		}

		*target = packet

		return nil
	}

	return tlv.MakeDynamicRecord(
		OnionPacketType, packet, size, encoder, decoder,
	)
}

// NewOnionPacketRecord creates the outer-hop type 20 record used to transport
// a trampoline onion through the ordinary payment-onion parser.
func NewOnionPacketRecord(packet **sphinx.OnionPacket) tlv.Record {
	return newOnionPacketRecord(packet)
}
