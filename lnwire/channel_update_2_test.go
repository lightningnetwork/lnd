package lnwire

import (
	"bytes"
	"fmt"
	"math"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// TestChanUpdate2FeeEncoding checks each fee against canonical tu32 vectors
// and rejects non-minimal or oversized values received from peers.
func TestChanUpdate2FeeEncoding(t *testing.T) {
	t.Parallel()

	// defaults holds the value each fee field takes when its TLV is
	// absent.
	defaults := map[uint64]uint32{
		16: defaultFeeBaseMsat,
		18: defaultFeeProportionalMillionths,
		20: defaultInboundFeeBaseMsat,
		22: defaultInboundFeeProportionalMillionths,
	}

	tests := []struct {
		name    string
		raw     []byte
		value   uint32
		wantErr bool
	}{
		{name: "zero"},
		{name: "one byte", raw: []byte{0xff}, value: 255},
		{name: "two bytes", raw: []byte{1, 0}, value: 256},
		{name: "three bytes", raw: []byte{1, 0, 0}, value: 65536},
		{
			name:  "four bytes",
			raw:   []byte{0xff, 0xff, 0xff, 0xff},
			value: math.MaxUint32,
		},
		{name: "non-minimal zero", raw: []byte{0}, wantErr: true},
		{name: "leading zero", raw: []byte{0, 1}, wantErr: true},
		{
			name:    "oversized",
			raw:     []byte{1, 0, 0, 0, 0},
			wantErr: true,
		},
	}
	for _, typ := range []uint64{16, 18, 20, 22} {
		for _, test := range tests {
			name := fmt.Sprintf("%d/%s", typ, test.name)
			t.Run(name, func(t *testing.T) {
				t.Parallel()

				data, err := EncodeRecords(tlv.MapToRecords(
					map[uint64][]byte{
						2:   make([]byte, sciddirLen),
						4:   make([]byte, 4),
						240: make([]byte, 64),
						typ: test.raw,
					},
				))
				require.NoError(t, err)

				var msg ChannelUpdate2
				err = msg.Decode(bytes.NewReader(data), 0)
				if test.wantErr {
					require.Error(t, err)

					return
				}
				require.NoError(t, err)

				// The TLV under test fills its own field. The
				// other fee fields read as their defaults.
				policy := msg.ForwardingPolicy()
				inboundBase, inboundRate := msg.InboundFee()
				fields := map[uint64]uint32{
					16: uint32(policy.BaseFee),
					18: uint32(policy.FeeRate),
					20: inboundBase,
					22: inboundRate,
				}
				for fieldType, got := range fields {
					want := defaults[fieldType]
					if fieldType == typ {
						want = test.value
					}
					require.Equalf(
						t, want, got, "fee TLV %d",
						fieldType,
					)
				}

				var found bool
				for _, record := range msg.AllRecords() {
					if uint64(record.Type()) != typ {
						continue
					}

					found = true
					var encoded bytes.Buffer
					require.NoError(
						t, record.Encode(&encoded),
					)
					require.Equal(
						t, test.raw, encoded.Bytes(),
					)
				}

				// The record is re-encoded because it was
				// present, even when it holds the default.
				require.True(t, found)
			})
		}
	}
}

// TestChanUpdate2EncodeDecode tests the encoding and decoding of the
// ChannelUpdate2 message using hardcoded byte slices.
func TestChanUpdate2EncodeDecode(t *testing.T) {
	t.Parallel()

	// We'll create a raw byte stream that represents a valid ChannelUpdate2
	// message. This includes the signature and a TLV stream with both known
	// and unknown records.
	rawBytes := []byte{
		// ChainHash record (optional, not mainnet).
		0x0,  // type.
		0x20, // length.
		0x1, 0x1, 0x1, 0x1, 0x1, 0x1, 0x1, 0x1, 0x1, 0x1, 0x1, 0x1,
		0x1, 0x1, 0x1, 0x1, 0x1, 0x1, 0x1, 0x1, 0x1, 0x1, 0x1, 0x1,
		0x1, 0x1, 0x1, 0x1, 0x1, 0x1, 0x1, 0x1,

		// ShortChannelID record (sciddir form: dir byte + 8-byte scid).
		0x2,                                    // type.
		0x9,                                    // length.
		0x1,                                    // dir byte: node_id_2.
		0x0, 0x0, 0x1, 0x0, 0x0, 0x2, 0x0, 0x3, // scid value.

		// BlockHeight record.
		0x4,                // type.
		0x4,                // length.
		0x0, 0x0, 0x1, 0x0, // value.

		// DisabledFlags record.
		0x6, // type.
		0x1, // length.
		0x1, // value.

		// Unknown odd-type TLV record.
		0x9,        // type.
		0x2,        // length.
		0xab, 0xcd, // value.

		// CLTVExpiryDelta record.
		0xa,       // type.
		0x2,       // length.
		0x0, 0x10, // value.

		// HTLCMinimumMsat record.
		0xc,             // type.
		0x3,             // length.
		0xf, 0x42, 0x40, // value (tu64: 1_000_000).

		// HTLCMaximumMsat record.
		0xe,             // type.
		0x3,             // length.
		0xf, 0x42, 0x40, // value (tu64: 1_000_000).

		// FeeBaseMsat record.
		0x10,     // type.
		0x2,      // length.
		0x1, 0x0, // value.

		// FeeProportionalMillionths record.
		0x12,     // type.
		0x2,      // length.
		0x1, 0x0, // value.

		// InboundFeeBaseMsat record.
		0x14, // type.
		0x1,  // length.
		0x5,  // value (5).

		// InboundFeeProportionalMillionths record.
		0x16, // type.
		0x1,  // length.
		0x3,  // value (3).

		// Extra Opaque Data - Unknown Record.
		0x18,       // type.
		0x2,        // length.
		0x79, 0x79, // value.

		// Signature.
		0xf0, // type.
		0x40, // length.
		0x0, 0x1, 0x2, 0x3, 0x4, 0x5, 0x6, 0x7, 0x8, 0x9, 0xa, 0xb,
		0xc, 0xd, 0xe, 0xf, 0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16,
		0x17, 0x18, 0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f, 0x20,
		0x21, 0x22, 0x23, 0x24, 0x25, 0x26, 0x27, 0x28, 0x29, 0x2a,
		0x2b, 0x2c, 0x2d, 0x2e, 0x2f, 0x30, 0x31, 0x32, 0x33, 0x34,
		0x35, 0x36, 0x37, 0x38, 0x39, 0x3a, 0x3b, 0x3c, 0x3d, 0x3e,
		0x3f, // value
	}

	secondSignedRangeType := new(bytes.Buffer)
	var buf [8]byte
	err := tlv.WriteVarInt(
		secondSignedRangeType, pureTLVSignedSecondRangeStart+1, &buf,
	)
	require.NoError(t, err)
	rawBytes = append(rawBytes, secondSignedRangeType.Bytes()...) // type.
	rawBytes = append(rawBytes, []byte{
		0x02,       // length.
		0x79, 0x79, // value.
	}...)

	// Now, create a new empty message and decode the raw bytes into it.
	msg := &ChannelUpdate2{}
	r := bytes.NewReader(rawBytes)
	err = msg.Decode(r, 0)
	require.NoError(t, err)

	// Next, encode the message back into a new byte buffer.
	var b bytes.Buffer
	err = msg.Encode(&b, 0)
	require.NoError(t, err)

	// The re-encoded bytes should be exactly the same as the original raw
	// bytes.
	require.Equal(t, rawBytes, b.Bytes())
}

// TestGossipV2ExplicitDefaults tests that a gossip v2 message re-encodes to the
// bytes that the sender signed, both when the sender omits a field that has a
// default and when it encodes the default value explicitly.
func TestGossipV2ExplicitDefaults(t *testing.T) {
	t.Parallel()

	mainnet := chaincfg.MainNetParams.GenesisHash[:]
	newAnnouncement := func() Message { return &ChannelAnnouncement2{} }

	// The compulsory records of each message, which every case includes.
	update := map[uint64][]byte{
		2:   make([]byte, sciddirLen),
		4:   make([]byte, 4),
		240: make([]byte, 64),
	}
	announcement := map[uint64][]byte{
		4:   make([]byte, 8),
		6:   {1},
		8:   make([]byte, 33),
		10:  make([]byte, 33),
		18:  make([]byte, 34),
		240: make([]byte, 64),
	}

	tests := []struct {
		name    string
		msg     func() Message
		records map[uint64][]byte
		typ     uint64
		value   []byte
	}{
		{name: "update/all absent", typ: 1},
		{name: "update/chain_hash", typ: 0, value: mainnet},
		{name: "update/disable_flags", typ: 6, value: []byte{0}},
		{
			name:  "update/cltv_expiry_delta",
			typ:   10,
			value: []byte{0, 80},
		},
		{name: "update/htlc_minimum_msat", typ: 12, value: []byte{1}},
		{name: "update/fee_base_msat", typ: 16, value: []byte{3, 0xe8}},
		{name: "update/fee_proportional", typ: 18, value: []byte{1}},
		{name: "update/inbound_fee_base", typ: 20, value: []byte{}},
		{name: "update/inbound_fee_rate", typ: 22, value: []byte{}},
		{
			name:    "announcement/all absent",
			msg:     newAnnouncement,
			records: announcement,
			typ:     1,
		},
		{
			name:    "announcement/chain_hash",
			msg:     newAnnouncement,
			records: announcement,
			typ:     0,
			value:   mainnet,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			records := update
			newMsg := func() Message { return &ChannelUpdate2{} }
			if test.msg != nil {
				records, newMsg = test.records, test.msg
			}

			fields := make(map[uint64][]byte, len(records)+1)
			for typ, value := range records {
				fields[typ] = value
			}

			// A nil value leaves every defaulted field absent.
			if test.value != nil {
				fields[test.typ] = test.value
			}
			raw, err := EncodeRecords(tlv.MapToRecords(fields))
			require.NoError(t, err)

			msg := newMsg()
			require.NoError(t, msg.Decode(bytes.NewReader(raw), 0))

			var b bytes.Buffer
			require.NoError(t, msg.Encode(&b, 0))
			require.Equal(t, raw, b.Bytes())
		})
	}
}
