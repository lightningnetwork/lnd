package lnwire

import (
	"github.com/lightningnetwork/lnd/tlv"
)

const (
	// DeliveryAddrType is the TLV record type for delivery addreses within
	// the name space of the OpenChannel and AcceptChannel messages.
	DeliveryAddrType = 0

	// deliveryAddressMaxSize is the maximum size in bytes of a
	// DeliveryAddress. BOLT 2 allows the following forms, sizes in bytes:
	// - pay to witness pubkey hash: 22
	// - pay to witness script hash: 34
	// - with option_shutdown_anysegwit, a witness program of version 1
	//   through 16 (OP_1 through OP_16 followed by a single push of 2 to
	//   40 bytes), which includes pay to taproot: 4 to 42.
	// The legacy pay to pubkey hash (25) and pay to script hash (23)
	// forms also fit within this limit.
	deliveryAddressMaxSize = 42
)

// DeliveryAddress is used to communicate the address to which funds from a
// closed channel should be sent. See deliveryAddressMaxSize for the forms it
// can take.
type DeliveryAddress []byte

// Record returns a TLV record that can be used to encode the delivery
// address within the ExtraData TLV stream. This was introduced in order to
// allow the OpenChannel/AcceptChannel messages to properly be extended with
// TLV types.
func (d *DeliveryAddress) Record() tlv.Record {
	addrBytes := (*[]byte)(d)

	return tlv.MakeDynamicRecord(
		DeliveryAddrType, addrBytes,
		func() uint64 {
			return uint64(len(*addrBytes))
		},
		tlv.EVarBytes, tlv.DVarBytes,
	)
}
