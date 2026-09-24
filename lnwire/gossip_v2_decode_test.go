package lnwire

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// gossipV2Message pairs a gossip v2 message with the records of a well-formed
// instance of it, so a test can drop one record at a time.
type gossipV2Message struct {
	name string
	new  func() Message

	// records is a valid record set for the message.
	records map[uint64][]byte

	// compulsory lists the types that the reader requires. Every entry is
	// also present in records.
	compulsory []uint64
}

// gossipV2Messages returns the four pure-TLV gossip v2 messages that enforce
// compulsory-field presence, each with a well-formed record set.
func gossipV2Messages() []gossipV2Message {
	return []gossipV2Message{
		{
			name: "channel_update_2",
			new:  func() Message { return &ChannelUpdate2{} },
			records: map[uint64][]byte{
				2:   make([]byte, sciddirLen),
				4:   make([]byte, 4),
				240: make([]byte, 64),
			},
			compulsory: []uint64{2, 4, 240},
		},
		{
			name: "channel_announcement_2",
			new:  func() Message { return &ChannelAnnouncement2{} },
			records: map[uint64][]byte{
				4:   make([]byte, 8),
				6:   {1},
				8:   make([]byte, 33),
				10:  make([]byte, 33),
				18:  make([]byte, 34),
				240: make([]byte, 64),
			},
			compulsory: []uint64{4, 6, 8, 10, 18, 240},
		},
		{
			name: "node_announcement_2",
			new:  func() Message { return &NodeAnnouncement2{} },
			records: map[uint64][]byte{
				0:   {},
				2:   make([]byte, 4),
				4:   make([]byte, 33),
				240: make([]byte, 64),
			},
			compulsory: []uint64{0, 2, 4, 240},
		},
		{
			name: "announcement_signatures_2",
			new: func() Message {
				return &AnnounceSignatures2{}
			},
			records: map[uint64][]byte{
				0: make([]byte, 32),
				2: make([]byte, 8),
				4: make([]byte, AnnouncementSigPairLen),
				6: make([]byte, 32),
			},
			compulsory: []uint64{0, 2, 4, 6},
		},
	}
}

// encodeRecordMap serialises a type-to-value map as a TLV stream.
func encodeRecordMap(t *testing.T, fields map[uint64][]byte) []byte {
	t.Helper()

	raw, err := EncodeRecords(tlv.MapToRecords(fields))
	require.NoError(t, err)

	return raw
}

// TestGossipV2CompulsoryFields tests that the gossip v2 readers reject a
// message that omits any compulsory TLV, and accept the full record set.
func TestGossipV2CompulsoryFields(t *testing.T) {
	t.Parallel()

	for _, msg := range gossipV2Messages() {
		t.Run(msg.name, func(t *testing.T) {
			t.Parallel()

			// The unmodified record set must decode, so a later
			// rejection is attributable to the dropped type.
			raw := encodeRecordMap(t, msg.records)
			require.NoError(
				t, msg.new().Decode(bytes.NewReader(raw), 0),
			)

			for _, dropped := range msg.compulsory {
				name := fmt.Sprintf("without %d", dropped)
				t.Run(name, func(t *testing.T) {
					t.Parallel()

					fields := make(map[uint64][]byte)
					for typ, value := range msg.records {
						if typ == dropped {
							continue
						}
						fields[typ] = value
					}

					raw := encodeRecordMap(t, fields)
					err := msg.new().Decode(
						bytes.NewReader(raw), 0,
					)
					require.ErrorContains(
						t, err, fmt.Sprintf(
							"required TLV type "+
								"%d missing",
							dropped,
						),
					)
				})
			}
		})
	}
}

// TestAnnouncementSigPairRejectsOverflow tests that a partial signature at or
// above the curve order is rejected rather than silently reduced mod n.
func TestAnnouncementSigPairRejectsOverflow(t *testing.T) {
	t.Parallel()

	// A scalar of all 0xff is above the secp256k1 group order.
	overflow := bytes.Repeat([]byte{0xff}, 32)
	valid := make([]byte, 32)
	valid[31] = 1

	tests := []struct {
		name string
		raw  []byte
	}{
		{
			name: "node sig overflows",
			raw:  append(append([]byte{}, overflow...), valid...),
		},
		{
			name: "bitcoin sig overflows",
			raw:  append(append([]byte{}, valid...), overflow...),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			records := map[uint64][]byte{
				0: make([]byte, 32),
				2: make([]byte, 8),
				4: test.raw,
				6: make([]byte, 32),
			}

			raw := encodeRecordMap(t, records)

			var msg AnnounceSignatures2
			err := msg.Decode(bytes.NewReader(raw), 0)
			require.ErrorIs(t, err, musig2.ErrPartialSigInvalid)
		})
	}

	// A pair that is below the order still decodes.
	records := map[uint64][]byte{
		0: make([]byte, 32),
		2: make([]byte, 8),
		4: append(append([]byte{}, valid...), valid...),
		6: make([]byte, 32),
	}

	var msg AnnounceSignatures2
	require.NoError(t, msg.Decode(
		bytes.NewReader(encodeRecordMap(t, records)), 0,
	))
}
