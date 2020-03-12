package lnwire

import (
	"io"

	"github.com/lightningnetwork/lnd/tlv"
)

const (
	// DeliveryAddrType is the TLV record type for delivery addreses within
	// the name space of the OpenChannel and AcceptChannel messages.
	DeliveryAddrType = 0
)

// TypedDeliveryAddress is similar to the DeliveryAddrType type, but it's
// encoded using a mini TLV stream. This tyupe was intorudced in order to allow
// the OpenChannel/AcceptChannel messages to properly be extended with TLV types.
type TypedDeliveryAddress []byte

// Encode encodes the target TypedDeliveryAddress into the target io.Writer
// using a TLV stream.
func (t *TypedDeliveryAddress) Encode(w io.Writer) error {
	addrBytes := []byte((*t)[:])

	records := []tlv.Record{
		tlv.MakeDynamicRecord(
			DeliveryAddrType, &addrBytes,
			func() uint64 {
				return uint64(len(addrBytes))
			},
			tlv.EVarBytes, tlv.DVarBytes,
		),
	}
	tlvStream, err := tlv.NewStream(records...)
	if err != nil {
		return err
	}

	return tlvStream.Encode(w)
}

// Decode decodes a set of bytes from the targer io.Reader into the target
// TypedDeliveryAddress.
func (t *TypedDeliveryAddress) Decode(r io.Reader) error {
	addrBytes := []byte((*t)[:])

	records := []tlv.Record{
		tlv.MakeDynamicRecord(
			DeliveryAddrType, &addrBytes, nil,
			tlv.EVarBytes, tlv.DVarBytes,
		),
	}

	tlvStream, err := tlv.NewStream(records...)
	if err != nil {
		return err
	}
	if err := tlvStream.Decode(r); err != nil {
		return err
	}

	*t = addrBytes

	return nil
}
