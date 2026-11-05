package htlcswitch

import (
	"bytes"
	"fmt"
	"math"
	"slices"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightningnetwork/lnd/lnwire"
)

// tamperKind enumerates the ways Mallory, one of the two peers, corrupts the
// channel protocol. Mallory runs the same honest link as the victim; the
// harness rewrites or injects messages on the wire between them. Any link
// failure after a tamper must carry one of the LinkFailureError codes the kind
// can legitimately cause (see expectedCodes). Beyond that, a kind is either:
//   - immediate: the corrupted message itself is invalid, so the victim must
//     fail the link while handling it (see tamper.mustReject);
//   - deferred: the message is well formed and only its effect on the next
//     signed commitment is wrong. Mallory may simply never sign again,
//     stalling the channel as any peer can, so the oracle is safety: the
//     corruption must never reach a commitment both sides hold.
type tamperKind uint8

const (
	// tamperCommitSig corrupts the commitment signature of the next
	// commit_sig: the ECDSA signature, or on a taproot channel, where that
	// field is empty, the scalar of the MuSig2 partial signature.
	tamperCommitSig tamperKind = iota

	// tamperHtlcSig corrupts one HTLC signature of the next commit_sig
	// that carries any.
	tamperHtlcSig

	// tamperHtlcSigCount drops the last HTLC signature of the next
	// commit_sig, or adds a spurious one when it carries none.
	tamperHtlcSigCount

	// tamperRevocation corrupts the revealed per-commitment secret of the
	// next revoke_and_ack.
	tamperRevocation

	// tamperAddID moves the next update_add_htlc ahead in the HTLC ID
	// sequence.
	tamperAddID

	// tamperAddAmount changes the amount of the next update_add_htlc by
	// whole satoshis. Commitment signatures only bind outputs rounded down
	// to the satoshi, so a sub-satoshi change can leave both commitment
	// transactions byte-identical: the peers would sign the same state and
	// merely disagree on msat bookkeeping, which no protocol check can
	// see. That is inherent to the protocol, not a detection gap.
	tamperAddAmount

	// tamperAddHash changes the payment hash of the next update_add_htlc.
	tamperAddHash

	// tamperFulfillPreimage corrupts the preimage of the next
	// update_fulfill_htlc.
	tamperFulfillPreimage

	// tamperResolveID points the next fulfill, fail or malformed fail at
	// an HTLC ID that was never offered.
	tamperResolveID

	// tamperFeeRate rewrites the rate of the next update_fee to a value
	// below the floor or far above what the initiator can pay.
	tamperFeeRate

	// tamperInjectFee injects an update_fee Mallory's own state does not
	// contain: a protocol violation outright from the non-initiator, and a
	// commitment mismatch waiting to happen from the initiator.
	tamperInjectFee

	// tamperReplay delivers the next protocol message twice.
	tamperReplay

	numTamperKinds
)

// String returns the name of the tamper kind for logs.
func (k tamperKind) String() string {
	names := [...]string{
		"CommitSig", "HtlcSig", "HtlcSigCount", "Revocation", "AddID",
		"AddAmount", "AddHash", "FulfillPreimage", "ResolveID",
		"FeeRate", "InjectFee", "Replay",
	}
	if int(k) < len(names) {
		return names[k]
	}

	return fmt.Sprintf("tamperKind(%d)", uint8(k))
}

// tamper is the corruption armed on one direction of the connection. A run
// carries at most one applied tamper: once the protocol is broken the rest of
// the run only matters up to the point the peers notice.
type tamper struct {
	kind  tamperKind
	param uint8

	// fromAlice selects the direction: Mallory is Alice (true) or Bob.
	fromAlice bool

	// applied is set once the tamper has hit a message.
	applied bool

	// hit is the type of message the tamper was applied to.
	hit lnwire.MessageType

	// mustReject is the corrupted message instance the victim has to
	// fail the link on, for immediate kinds; nil for deferred ones.
	mustReject lnwire.Message
}

// String describes the tamper for logs.
func (t *tamper) String() string {
	mallory := "Bob"
	if t.fromAlice {
		mallory = "Alice"
	}

	return fmt.Sprintf("%s tamper by %s (param %d, hit %v)", t.kind,
		mallory, t.param, t.hit)
}

// expectedCodes returns the LinkFailureError codes a peer may legitimately
// fail with once this tamper has been applied. Corruption that is only
// visible once both sides sign (a rewritten add or fee) surfaces as a
// commitment signature mismatch on whichever side verifies first, or as the
// victim refusing to sign a state it cannot afford.
func (t *tamper) expectedCodes() []errorCode {
	switch t.kind {
	case tamperCommitSig, tamperHtlcSig, tamperHtlcSigCount:
		return []errorCode{ErrInvalidCommitment}

	case tamperRevocation:
		return []errorCode{ErrInvalidRevocation}

	case tamperAddID, tamperFulfillPreimage, tamperResolveID:
		return []errorCode{ErrInvalidUpdate}

	case tamperAddAmount, tamperAddHash:
		return []errorCode{ErrInvalidUpdate, ErrInvalidCommitment}

	case tamperFeeRate, tamperInjectFee:
		return []errorCode{
			ErrInvalidUpdate, ErrInvalidCommitment,
			ErrInternalError, ErrStfuViolation,
		}

	case tamperReplay:
		switch t.hit {
		case lnwire.MsgCommitSig:
			return []errorCode{ErrInvalidCommitment}
		case lnwire.MsgRevokeAndAck:
			return []errorCode{ErrInvalidRevocation}
		case lnwire.MsgStfu:
			return []errorCode{ErrStfuViolation}
		default:
			return []errorCode{ErrInvalidUpdate}
		}
	}

	return nil
}

// immediate reports whether the corrupted message is invalid on its own, so
// the victim must reject it on receipt. update_fee from the initiator is well
// formed whatever its rate; only the commitment it leads to is wrong.
func (t *tamper) immediate(malloryIsInitiator bool) bool {
	switch t.kind {
	case tamperAddAmount, tamperAddHash, tamperFeeRate:
		return false

	case tamperInjectFee:
		return !malloryIsInitiator

	default:
		return true
	}
}

// expects reports whether a link failing with code is a legitimate reaction
// to this tamper.
func (t *tamper) expects(code errorCode) bool {
	return slices.Contains(t.expectedCodes(), code)
}

// flipByte returns a copy of b with the byte selected by param XOR-ed with a
// fixed non-zero mask, so the value is guaranteed to change.
func flipByte(b []byte, param uint8) []byte {
	out := slices.Clone(b)
	out[int(param)%len(out)] ^= 0x55

	return out
}

// corruptSig returns sig with one byte of its raw r||s encoding flipped.
func corruptSig(sig lnwire.Sig, param uint8) lnwire.Sig {
	corrupted, err := lnwire.NewSigFromWireECDSA(
		flipByte(sig.RawBytes(), param),
	)
	if err != nil {
		// Unreachable for a 64-byte input; keep the message unchanged
		// rather than invent a signature.
		return sig
	}

	return corrupted
}

// corruptPartialSig returns a copy of a MuSig2 partial signature with one byte
// of its scalar flipped. The nonce is left intact, so the signature still
// parses and fails only on verification.
func corruptPartialSig(sig lnwire.PartialSigWithNonce,
	param uint8) *lnwire.PartialSigWithNonce {

	scalar := sig.Sig.Bytes()
	copy(scalar[:], flipByte(scalar[:], param))
	sig.Sig.SetBytes(&scalar)

	return &sig
}

// tamperMsg applies the tamper to msg if msg is the kind of message it
// targets. It returns the messages to deliver in place of msg (the replay
// tamper yields two) and whether the tamper hit. Messages are copied before
// being rewritten, since the sender may still hold the originals.
func (t *tamper) tamperMsg(msg lnwire.Message) ([]lnwire.Message, bool) {
	switch t.kind {
	case tamperCommitSig:
		m, ok := msg.(*lnwire.CommitSig)
		if !ok {
			return nil, false
		}
		c := *m
		c.CommitSig = corruptSig(m.CommitSig, t.param)
		m.PartialSig.WhenSome(func(r lnwire.PartialSigWithNonceTLV) {
			c.CommitSig = m.CommitSig
			c.PartialSig = lnwire.MaybePartialSigWithNonce(
				corruptPartialSig(r.Val, t.param),
			)
		})

		return []lnwire.Message{&c}, true

	case tamperHtlcSig:
		m, ok := msg.(*lnwire.CommitSig)
		if !ok || len(m.HtlcSigs) == 0 {
			return nil, false
		}
		c := *m
		c.HtlcSigs = slices.Clone(m.HtlcSigs)
		i := int(t.param) % len(c.HtlcSigs)
		c.HtlcSigs[i] = corruptSig(c.HtlcSigs[i], t.param)

		return []lnwire.Message{&c}, true

	case tamperHtlcSigCount:
		m, ok := msg.(*lnwire.CommitSig)
		if !ok {
			return nil, false
		}
		c := *m
		if len(m.HtlcSigs) > 0 {
			last := len(m.HtlcSigs) - 1
			c.HtlcSigs = slices.Clone(m.HtlcSigs[:last])
		} else {
			c.HtlcSigs = []lnwire.Sig{m.CommitSig}
		}

		return []lnwire.Message{&c}, true

	case tamperRevocation:
		m, ok := msg.(*lnwire.RevokeAndAck)
		if !ok {
			return nil, false
		}
		r := *m
		copy(r.Revocation[:], flipByte(m.Revocation[:], t.param))

		return []lnwire.Message{&r}, true

	case tamperAddID, tamperAddAmount, tamperAddHash:
		m, ok := msg.(*lnwire.UpdateAddHTLC)
		if !ok {
			return nil, false
		}
		a := *m
		switch t.kind {
		case tamperAddID:
			a.ID += 1 + uint64(t.param%4)
		case tamperAddAmount:
			a.Amount += lnwire.NewMSatFromSatoshis(
				1 + btcutil.Amount(t.param),
			)
		default:
			copy(a.PaymentHash[:],
				flipByte(m.PaymentHash[:], t.param))
		}

		return []lnwire.Message{&a}, true

	case tamperFulfillPreimage:
		m, ok := msg.(*lnwire.UpdateFulfillHTLC)
		if !ok {
			return nil, false
		}
		u := *m
		copy(u.PaymentPreimage[:], flipByte(m.PaymentPreimage[:],
			t.param))

		return []lnwire.Message{&u}, true

	case tamperResolveID:
		switch m := msg.(type) {
		case *lnwire.UpdateFulfillHTLC:
			u := *m
			u.ID = nonExistentHTLCID

			return []lnwire.Message{&u}, true

		case *lnwire.UpdateFailHTLC:
			u := *m
			u.ID = nonExistentHTLCID

			return []lnwire.Message{&u}, true

		case *lnwire.UpdateFailMalformedHTLC:
			u := *m
			u.ID = nonExistentHTLCID

			return []lnwire.Message{&u}, true
		}

		return nil, false

	case tamperFeeRate:
		m, ok := msg.(*lnwire.UpdateFee)
		if !ok {
			return nil, false
		}
		u := *m
		u.FeePerKw = distinctFeeRate(tamperedFeeRate(t.param),
			m.FeePerKw)

		return []lnwire.Message{&u}, true

	case tamperReplay:
		switch msg.(type) {
		case *lnwire.UpdateAddHTLC, *lnwire.UpdateFulfillHTLC,
			*lnwire.UpdateFailHTLC, *lnwire.UpdateFailMalformedHTLC,
			*lnwire.CommitSig, *lnwire.RevokeAndAck, *lnwire.Stfu:

			return []lnwire.Message{msg, cloneMsg(msg)}, true
		}

		return nil, false
	}

	return nil, false
}

// tamperedFeeRate maps param to a fee rate no honest initiator would propose:
// at most 251 sat/kw, below the 253 sat/kw floor, for even params, and a
// large rate (2^16 sat/kw and up) for odd ones.
func tamperedFeeRate(param uint8) uint32 {
	if param%2 == 0 {
		return uint32(param) % 252
	}

	return math.MaxUint32 >> (param % 16)
}

// distinctFeeRate returns rate moved at least 2 sat/kw away from the rate
// Mallory's own state holds. A commitment weighs over 700 wu, so that shifts
// the fee by at least a satoshi and the two commitment transactions really
// differ (see tamperAddAmount for why sub-satoshi changes cannot count).
func distinctFeeRate(rate, own uint32) uint32 {
	if rate+1 >= own && rate <= own+1 {
		return own + 2
	}

	return rate
}

// cloneMsg returns an independent copy of msg by round-tripping it through
// the wire encoding, so a replayed message is a distinct instance the oracle
// can single out.
func cloneMsg(msg lnwire.Message) lnwire.Message {
	var b bytes.Buffer
	if _, err := lnwire.WriteMessage(&b, msg, 0); err != nil {
		panic(fmt.Sprintf("encode %T: %v", msg, err))
	}
	clone, err := lnwire.ReadMessage(&b, 0)
	if err != nil {
		panic(fmt.Sprintf("decode %T: %v", msg, err))
	}

	return clone
}
