package hop

import (
	"encoding/binary"
	"fmt"
	"io"

	"github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/tlv"
)

// ErrInvalidPayload is an error returned when a parsed onion payload either
// included or omitted incorrect records for a particular hop type.
type ErrInvalidPayload struct {
	// Type the record's type that cause the violation.
	Type tlv.Type

	// Ommitted if true, signals that the sender did not include the record.
	// Otherwise, the sender included the record when it shouldn't have.
	Omitted bool

	// FinalHop if true, indicates that the violation is for the final hop
	// in the route (identified by next hop id), otherwise the violation is
	// for an intermediate hop.
	FinalHop bool
}

// Error returns a human-readable description of the invalid payload error.
func (e ErrInvalidPayload) Error() string {
	hopType := "intermediate"
	if e.FinalHop {
		hopType = "final"
	}
	violation := "included"
	if e.Omitted {
		violation = "omitted"
	}

	return fmt.Sprintf("onion payload for %s hop %s record with type %d",
		hopType, violation, e.Type)
}

// Payload encapsulates all information delivered to a hop in an onion payload.
// A Hop can represent either a TLV or legacy payload. The primary forwarding
// instruction can be accessed via ForwardingInfo, and additional records can be
// accessed by other member functions.
type Payload struct {
	// FwdInfo holds the basic parameters required for HTLC forwarding, e.g.
	// amount, cltv, and next hop.
	FwdInfo ForwardingInfo
}

// NewLegacyPayload builds a Payload from the amount, cltv, and next hop
// parameters provided by leegacy onion payloads.
func NewLegacyPayload(f *sphinx.HopData) *Payload {
	nextHop := binary.BigEndian.Uint64(f.NextAddress[:])

	return &Payload{
		FwdInfo: ForwardingInfo{
			Network:         BitcoinNetwork,
			NextHop:         lnwire.NewShortChanIDFromInt(nextHop),
			AmountToForward: lnwire.MilliSatoshi(f.ForwardAmount),
			OutgoingCTLV:    f.OutgoingCltv,
		},
	}
}

// NewPayloadFromReader builds a new Hop from the passed io.Reader. The reader
// should correspond to the bytes encapsulated in a TLV onion payload.
func NewPayloadFromReader(r io.Reader) (*Payload, error) {
	var (
		cid  uint64
		amt  uint64
		cltv uint32
	)

	tlvStream, err := tlv.NewStream(
		record.NewAmtToFwdRecord(&amt),
		record.NewLockTimeRecord(&cltv),
		record.NewNextHopIDRecord(&cid),
	)
	if err != nil {
		return nil, err
	}

	parsedTypes, err := tlvStream.DecodeWithParsedTypes(r)
	if err != nil {
		return nil, err
	}

	nextHop := lnwire.NewShortChanIDFromInt(cid)

	// Validate whether the sender properly included or omitted tlv records
	// in accordance with BOLT 04.
	err = ValidateParsedPayloadTypes(parsedTypes, nextHop)
	if err != nil {
		return nil, err
	}

	return &Payload{
		FwdInfo: ForwardingInfo{
			Network:         BitcoinNetwork,
			NextHop:         nextHop,
			AmountToForward: lnwire.MilliSatoshi(amt),
			OutgoingCTLV:    cltv,
		},
	}, nil
}

// ForwardingInfo returns the basic parameters required for HTLC forwarding,
// e.g. amount, cltv, and next hop.
func (h *Payload) ForwardingInfo() ForwardingInfo {
	return h.FwdInfo
}

// ValidateParsedPayloadTypes checks the types parsed from a hop payload to
// ensure that the proper fields are either included or omitted. The finalHop
// boolean should be true if the payload was parsed for an exit hop. The
// requirements for this method are described in BOLT 04.
func ValidateParsedPayloadTypes(parsedTypes tlv.TypeSet,
	nextHop lnwire.ShortChannelID) error {

	isFinalHop := nextHop == Exit

	_, hasAmt := parsedTypes[record.AmtOnionType]
	_, hasLockTime := parsedTypes[record.LockTimeOnionType]
	_, hasNextHop := parsedTypes[record.NextHopOnionType]

	switch {

	// All hops must include an amount to forward.
	case !hasAmt:
		return ErrInvalidPayload{
			Type:     record.AmtOnionType,
			Omitted:  true,
			FinalHop: isFinalHop,
		}

	// All hops must include a cltv expiry.
	case !hasLockTime:
		return ErrInvalidPayload{
			Type:     record.LockTimeOnionType,
			Omitted:  true,
			FinalHop: isFinalHop,
		}

	// The exit hop should omit the next hop id. If nextHop != Exit, the
	// sender must have included a record, so we don't need to test for its
	// inclusion at intermediate hops directly.
	case isFinalHop && hasNextHop:
		return ErrInvalidPayload{
			Type:     record.NextHopOnionType,
			Omitted:  false,
			FinalHop: true,
		}
	}

	return nil
}
