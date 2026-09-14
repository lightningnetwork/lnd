package lnwire

import (
	"encoding/binary"
	"fmt"
	"io"

	"github.com/lightningnetwork/lnd/tlv"
)

// SciddirLen is the wire length of a Sciddir: one direction byte followed by
// the 8-byte short_channel_id.
const SciddirLen = sciddirLen

// Sciddir is the `sciddir` form of BOLT 1's `sciddir_or_pubkey` type: a
// channel-with-direction identifier carried on the wire as
// `<dirbyte><short_channel_id>` for 9 bytes total. The leading byte must be
// `0` (refers to node_id_1 of the corresponding channel_announcement_2) or
// `1` (refers to node_id_2). The `pubkey` form of `sciddir_or_pubkey` is
// rejected wherever a Sciddir is used.
type Sciddir struct {
	// Direction is the direction byte. `0` means the message comes from
	// (or refers to) `node_id_1` of the channel announcement; `1` means
	// `node_id_2`.
	Direction byte

	// ID is the 8-byte short_channel_id portion.
	ID ShortChannelID
}

// NewSciddir builds a Sciddir from a short_channel_id and an `isSecondPeer`
// flag — true if the message comes from `node_id_2`.
func NewSciddir(scid ShortChannelID, isSecondPeer bool) Sciddir {
	var dir byte
	if isSecondPeer {
		dir = 1
	}

	return Sciddir{
		Direction: dir,
		ID:        scid,
	}
}

// IsNode1 reports whether the direction byte refers to `node_id_1` (i.e.
// `dirbyte == 0`).
func (s *Sciddir) IsNode1() bool {
	return s.Direction == 0
}

// Record returns the tlv record used to encode a Sciddir on the wire. The
// wrapping RecordT supplies the actual TLV type number; the zero passed here
// is overridden.
func (s *Sciddir) Record() tlv.Record {
	return tlv.MakeStaticRecord(
		0, s, SciddirLen, sciddirEncoder, sciddirDecoder,
	)
}

// sciddirEncoder reuses the introduction-node codec for the shared wire form.
func sciddirEncoder(w io.Writer, val interface{}, _ *[8]byte) error {
	v, ok := val.(*Sciddir)
	if !ok {
		return tlv.NewTypeForEncodingErr(val, "lnwire.Sciddir")
	}

	var scid [scidLen]byte
	binary.BigEndian.PutUint64(scid[:], v.ID.ToUint64())
	intro, err := NewSciddirIntro(v.Direction, scid)
	if err != nil {
		return err
	}

	return intro.encode(w)
}

// sciddirDecoder accepts only the SCID variant of the introduction-node codec.
func sciddirDecoder(r io.Reader, val interface{}, buf *[8]byte,
	l uint64) error {

	v, ok := val.(*Sciddir)
	if !ok || l != SciddirLen {
		return tlv.NewTypeForDecodingErr(val, "lnwire.Sciddir", l,
			SciddirLen)
	}

	intro, err := decodeIntroductionNode(io.LimitReader(r, int64(l)), buf)
	if err != nil {
		return err
	}

	sciddir, ok := intro.(SciddirIntro)
	if !ok {
		return fmt.Errorf("expected sciddir introduction node, got %T",
			intro)
	}
	v.Direction = sciddir.Direction
	v.ID = NewShortChanIDFromInt(binary.BigEndian.Uint64(sciddir.SCID[:]))

	return nil
}
