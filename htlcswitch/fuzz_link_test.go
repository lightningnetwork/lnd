package htlcswitch

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"math"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/htlcswitch/hop"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// fuzzScalar returns a 32-byte scalar derived from sigHash with three
// invariants that guarantee a clean round-trip through ecdsa.ParseDERSignature
// and the lnwire.Sig 64-byte compact encoding:
//
//  1. s[0] != 0x00 — extractCanonicalPadding always keeps all 32 bytes,
//     so the DER layout is fixed: 0x30 ?? 02 01 01 02 20 [32 bytes].
//  2. s[0] < 0x80 — no DER sign-extension 0x00 prefix needed.
//  3. s < 2^254 << N/2 — ParseDERSignature never normalizes s to N-s.
//
// Achieved by: clear the top two bits of s[0] and set bit 0.
// Result: s[0] ∈ {0x01,0x03,…,0x3f}, no secp256k1 arithmetic needed.
func fuzzScalar(sigHash []byte) [32]byte {
	s := sha256.Sum256(sigHash)
	s[0] = s[0]&0x3f | 0x01
	return s
}

// fuzzDERSig builds a minimal DER-encoded ECDSA signature with r=1 and
// s=fuzzScalar(sigHash). Both r and s are small positives so no sign-extension
// padding is needed. ecdsa.ParseDERSignature accepts the result unchanged.
func fuzzDERSig(sigHash []byte) []byte {
	s := fuzzScalar(sigHash)
	var inner []byte
	inner = append(inner, 0x02, 0x01, 0x01) // r = 1
	inner = append(inner, 0x02, 0x20)       // s tag + 32-byte length
	inner = append(inner, s[:]...)          // s value

	return append([]byte{0x30, byte(len(inner))}, inner...)
}

// fuzzSigner embeds MockSigner to satisfy input.Signer (MuSig2 methods,
// ComputeInputScript) but overrides SignOutputRaw with a trivial scheme:
// r=1, s=fuzzScalar(sigHash). Zero secp256k1 point-multiplication.
// Returns a real *ecdsa.Signature so lnwire.NewSigFromSignature accepts it.
type fuzzSigner struct {
	*input.MockSigner
}

func (f *fuzzSigner) SignOutputRaw(tx *wire.MsgTx,
	signDesc *input.SignDescriptor) (input.Signature, error) {

	sigHash, err := txscript.CalcWitnessSigHash(
		signDesc.WitnessScript, signDesc.SigHashes, signDesc.HashType,
		tx, signDesc.InputIndex, signDesc.Output.Value,
	)
	if err != nil {
		return nil, err
	}

	return ecdsa.ParseDERSignature(fuzzDERSig(sigHash))
}

// fuzzSigVerifier is the paired verifier for fuzzSigner. It extracts s from
// the DER-serialized signature (preserved through the lnwire round-trip) and
// checks s == fuzzScalar(sigHash).
func fuzzSigVerifier(sig input.Signature, sigHash []byte,
	_ *btcec.PublicKey) bool {

	expected := fuzzScalar(sigHash)

	// DER layout after round-trip: 0x30 [len] 0x02 0x01 0x01 0x02 0x20
	// [32 bytes s] fuzzScalar guarantees s[0] < 0x40, so no DER
	// sign-extension byte is ever added and the s field is always exactly
	// 32 bytes.
	der := sig.Serialize()
	if len(der) < 7+32 {
		return false
	}
	sBytes := der[7 : 7+32]

	return bytes.Equal(sBytes, expected[:])
}

type Event uint8

const (
	EvAliceSendAddHtlc Event = iota
	EvBobSendAddHtlc
	EvAliceSendCommit
	EvBobSendCommit
	EvAliceSettleHtlc
	EvBobSettleHtlc
	EvAliceInvalidHtlcSettlement
	EvBobInvalidHtlcSettlement
	EvAliceFailHtlc
	EvBobFailHtlc
	EvAliceFailNonExistentHtlc
	EvBobFailNonExistentHtlc
	EvAliceSendUpdateFee
	EvBobSendUpdateFee
	EvAliceInitQuiescence
	EvBobInitQuiescence
	EvResumeQuiescence
	EvAliceRestartLink
	EvBobRestartLink
	EvAliceSendCommitNoWindow
	EvBobSendCommitNoWindow
	EvAliceSendWarning
	EvBobSendWarning
	EvAliceSendBadOnion
	EvBobSendBadOnion

	NumEvents
)

const MaxEventsPerRun = 500

type fuzzFSM struct {
	t          *testing.T
	alice, bob *testLightningChannel

	// terminated is set to true when a link fails for an expected protocol
	// reason (e.g. channel reserve exceeded). Once set, no further events
	// should be applied and the test run ends cleanly.
	terminated bool

	aliceLink *channelLink
	bobLink   *channelLink

	// alicePeer captures messages that Alice's link sends to Bob.
	// bobPeer captures messages that Bob's link sends to Alice.
	alicePeer *mockPeer
	bobPeer   *mockPeer

	// Registries and circuit maps used by sendHTLC. Alice's link uses
	// aliceRegistry (for incoming HTLCs from Bob); Bob's link uses
	// bobRegistry (for incoming HTLCs from Alice).
	aliceRegistry *mockInvoiceRegistry
	bobRegistry   *mockInvoiceRegistry
	aliceCircuits *mockCircuitMap
	bobCircuits   *mockCircuitMap

	// Fields required to reconstruct a link on restart.
	hopNet       *hopNetwork
	aliceDecoder *mockIteratorDecoder
	bobDecoder   *mockIteratorDecoder
	alicePCache  *mockPreimageCache
	bobPCache    *mockPreimageCache
	bestHeight   func() uint32

	// Preimages for created HTLCs
	alicePreimages map[uint64]lntypes.Preimage
	bobPreimages   map[uint64]lntypes.Preimage

	// HTLC
	bobNextHTLCID   uint64
	aliceNextHTLCID uint64
	htlcRef         uint64

	// Monotonically-increasing attempt counters used to derive unique
	// preimageSeed.
	aliceHTLCAttempts uint64
	bobHTLCAttempts   uint64

	// restartSyncHeight, when non-zero, overrides NextLocalCommitHeight in
	// the remote's ChannelReestablish during restartLink.
	restartSyncHeight uint64

	// maxFeeExposure is the per-link MaxFeeExposure threshold passed to
	// newFuzzLink for both Alice and Bob (kept on the FSM so restartLink
	// can rebuild a link with the same value).
	maxFeeExposure lnwire.MilliSatoshi

	// maxFeeAllocation is the per-link MaxFeeAllocation fraction (0..1]
	// of channel balance allowed for the commitment fee. Kept on the FSM so
	// restartLink can rebuild a link with the same value.
	maxFeeAllocation float64

	// Height regression detection
	aliceLocalHeight  uint64
	aliceRemoteHeight uint64
	bobLocalHeight    uint64
	bobRemoteHeight   uint64
	heightsInit       bool

	// Shadow balances. Updated immediately on every confirmed settle so the
	// strong invariant in assertInvariants can detect misallocation between
	// Alice and Bob.
	expectedAliceMSat lnwire.MilliSatoshi
	expectedBobMSat   lnwire.MilliSatoshi

	// Settle round-trip tracking. After settleHTLC is called the HTLC stays
	// in LocalCommitment.Htlcs until the next commit round completes on
	// both sides; during that window the strong invariant needs to know
	// which still-committed HTLCs have already been claimed. Keys are the
	// sender's HtlcIndex (same value lnwallet stores in channeldb.HTLC).
	//   aliceSettlesPending: B→A HTLCs Alice has settled.
	//   bobSettlesPending:   A→B HTLCs Bob has settled.
	aliceSettlesPending map[uint64]struct{}
	bobSettlesPending   map[uint64]struct{}
}

// newFuzzFSM initializes and returns a new fuzz finite state machine (FSM)
// instance with the specified channel size and configuration parameters.
func newFuzzFSM(t *testing.T, channelSize, aliceShareGen,
	maxFeeExposureGen, maxFeeAllocationGen uint64) *fuzzFSM {
	// Redirect all t.TempDir() calls to /dev/shm (tmpfs) so that the
	// channeldb bbolt files are kept in RAM rather than written to disk.
	// This mitigates the disk I/O bottleneck during fuzzing.
	if runtime.GOOS != "linux" {
		t.Skipf("Skipping fuzz/scenario test on non-Linux OS: %s",
			runtime.GOOS)
	}
	t.Setenv("TMPDIR", "/dev/shm")

	// Maximum and minimum limits on channel capacity currently enforced by
	// LND. Not considering Wumbo channels here.
	chanCapacity := channelSize
	maxCapacity := uint64(1<<24) - 1
	minCapacity := uint64(20000)

	if channelSize < minCapacity {
		chanCapacity = minCapacity
	} else if channelSize > maxCapacity {
		chanCapacity = maxCapacity
	}

	// 20-79% of the channel capacity
	aliceShare := 20 + aliceShareGen%60

	_, SchanID := genID()
	aliceAmount := btcutil.Amount(chanCapacity * aliceShare / 100)
	bobAmount := btcutil.Amount(chanCapacity) - aliceAmount

	// The maximum limit on channel reserves is set to be 10% of the channel
	// capacity.
	aliceReserve := btcutil.Amount(chanCapacity / 10)
	bobReserve := btcutil.Amount(chanCapacity / 10)

	blockHeight := 100

	// Create lightning channels using the trivial fuzz signer so that
	// secp256k1 ECDSA is never called during fuzzing (big CPU win).
	mkFuzzSigner := func(k *btcec.PrivateKey) input.Signer {
		return &fuzzSigner{
			MockSigner: input.NewMockSigner(
				[]*btcec.PrivateKey{k}, nil,
			),
		}
	}
	alice, bob, err := createTestChannel(t, alicePrivKey, bobPrivKey,
		aliceAmount, bobAmount, aliceReserve, bobReserve, SchanID,
		withTestSignerFactory(mkFuzzSigner),
		withTestChanOpts(
			lnwallet.WithSigVerifier(fuzzSigVerifier),
		),
	)
	require.NoError(t, err)

	alicePeer := &mockPeer{
		sentMsgs: make(chan lnwire.Message, 100),
		quit:     make(chan struct{}),
	}
	bobPeer := &mockPeer{
		sentMsgs: make(chan lnwire.Message, 100),
		quit:     make(chan struct{}),
	}

	// Map maxFeeExposureGen to a per-link MaxFeeExposure threshold:
	//   gen == 0 → DefaultMaxFeeExposure (current harness behaviour)
	//   gen != 0 → [10_000, 750_000_000) mSAT, covering tight values
	//              that frequently trigger "fee threshold exceeded" up to
	//              loose values close to the default.
	maxFeeExposure := DefaultMaxFeeExposure
	if maxFeeExposureGen != 0 {
		maxFeeExposure = lnwire.MilliSatoshi(
			10_000 + maxFeeExposureGen%(750_000_000-10_000),
		)
	}

	// Map maxFeeAllocationGen to a per-link MaxFeeAllocation in (0, 1]:
	//   gen == 0 → DefaultMaxLinkFeeAllocation (current harness behaviour)
	//   gen != 0 → ((gen % 100) + 1) / 100.0 ∈ {0.01, …, 1.00}
	maxFeeAllocation := DefaultMaxLinkFeeAllocation
	if maxFeeAllocationGen != 0 {
		maxFeeAllocation = float64(maxFeeAllocationGen%100+1) / 100.0
	}

	hopNet := newHopNetwork()

	// Each side gets its own registry, preimage cache, and circuit map.
	// These are plain in-memory mocks with no background goroutines, so
	// there is nothing to race with the test goroutine.
	aliceRegistry := newMockRegistry(t)
	bobRegistry := newMockRegistry(t)
	alicePCache := newMockPreimageCache()
	bobPCache := newMockPreimageCache()
	aliceCircuits := &mockCircuitMap{lookup: make(chan *PaymentCircuit)}
	bobCircuits := &mockCircuitMap{lookup: make(chan *PaymentCircuit)}

	blockHeightVal := uint32(blockHeight)
	bestHeight := func() uint32 { return blockHeightVal }

	aliceDecoder := newMockIteratorDecoder()
	bobDecoder := newMockIteratorDecoder()

	// Create both links without starting the htlcManager goroutine and
	// without a Switch. newFuzzLink sets link.upstream directly so we can
	// drive reestablishment synchronously below.
	aliceLink, aliceUpstream := hopNet.newFuzzLink(
		t, alicePeer, alice.channel, aliceDecoder,
		aliceRegistry, alicePCache, aliceCircuits, bestHeight,
		maxFeeExposure, maxFeeAllocation,
	)
	bobLink, bobUpstream := hopNet.newFuzzLink(
		t, bobPeer, bob.channel, bobDecoder,
		bobRegistry, bobPCache, bobCircuits, bestHeight,
		maxFeeExposure, maxFeeAllocation,
	)

	// Generate the ChannelReestablish messages that each side needs to
	// receive in order to complete the sync handshake.
	aliceSyncMsg, err := alice.channel.State().ChanSyncMsg()
	require.NoError(t, err)
	bobSyncMsg, err := bob.channel.State().ChanSyncMsg()
	require.NoError(t, err)

	// Cross-inject: Alice's link reads from aliceUpstream (gets Bob's msg),
	// Bob's link reads from bobUpstream (gets Alice's msg).
	aliceUpstream <- bobSyncMsg
	bobUpstream <- aliceSyncMsg

	// resumeLink runs syncChanStates synchronously — no goroutine spawned.
	require.NoError(t, aliceLink.resumeLink(t.Context()))
	require.NoError(t, bobLink.resumeLink(t.Context()))

	return &fuzzFSM{
		t:                   t,
		alice:               alice,
		bob:                 bob,
		aliceLink:           aliceLink,
		bobLink:             bobLink,
		aliceRegistry:       aliceRegistry,
		bobRegistry:         bobRegistry,
		aliceCircuits:       aliceCircuits,
		bobCircuits:         bobCircuits,
		alicePeer:           alicePeer,
		bobPeer:             bobPeer,
		alicePreimages:      make(map[uint64]lntypes.Preimage),
		bobPreimages:        make(map[uint64]lntypes.Preimage),
		hopNet:              hopNet,
		aliceDecoder:        aliceDecoder,
		bobDecoder:          bobDecoder,
		alicePCache:         alicePCache,
		bobPCache:           bobPCache,
		bestHeight:          bestHeight,
		maxFeeExposure:      maxFeeExposure,
		maxFeeAllocation:    maxFeeAllocation,
		expectedAliceMSat:   lnwire.NewMSatFromSatoshis(aliceAmount),
		expectedBobMSat:     lnwire.NewMSatFromSatoshis(bobAmount),
		aliceSettlesPending: make(map[uint64]struct{}),
		bobSettlesPending:   make(map[uint64]struct{}),
	}
}

// assertInvariants verifies, after every driven event, that both channels'
// commit heights never regress, stay mirrored (Alice local == Bob remote, and
// vice versa, within a lag of 1), and that each party's claimable funds match
// the expected settled balance — catching any silent fund misallocation.
func (f *fuzzFSM) assertInvariants() {
	aliceChanState := f.alice.channel.State()
	aliceLocal := aliceChanState.LocalCommitment.CommitHeight
	aliceRemote := aliceChanState.RemoteCommitment.CommitHeight

	bobChanState := f.bob.channel.State()
	bobLocal := bobChanState.LocalCommitment.CommitHeight
	bobRemote := bobChanState.RemoteCommitment.CommitHeight

	if !f.heightsInit {
		f.aliceLocalHeight = aliceLocal
		f.aliceRemoteHeight = aliceRemote
		f.bobLocalHeight = bobLocal
		f.bobRemoteHeight = bobRemote
		f.heightsInit = true

		return
	}

	// Monotonic
	if aliceLocal < f.aliceLocalHeight ||
		aliceRemote < f.aliceRemoteHeight {

		f.t.Fatalf("height regression: aliceLocal=%d "+
			"lastLocalHeight=%d aliceRemote=%d"+
			"lastRemoteHeight=%d",
			aliceLocal, f.aliceLocalHeight, aliceRemote,
			f.aliceRemoteHeight)
	}

	if bobLocal < f.bobLocalHeight || bobRemote < f.bobRemoteHeight {
		f.t.Fatalf("height regression: bobLocal=%d "+
			"lastLocalHeight=%d bobRemote=%d lastRemoteHeight=%d",
			bobLocal, f.bobLocalHeight, bobRemote,
			f.bobRemoteHeight)
	}

	f.aliceLocalHeight = aliceLocal
	f.aliceRemoteHeight = aliceRemote
	f.bobLocalHeight = bobLocal
	f.bobRemoteHeight = bobRemote

	// They should be "mirrored"
	// We allow a lag of 1 due to transient protocol state.
	diff := func(a, b uint64) uint64 {
		if a > b {
			return a - b
		}

		return b - a
	}

	if diff(aliceLocal, bobRemote) > 1 {
		f.t.Fatalf("commit mismatch: aliceLocal=%d bobRemote=%d",
			aliceLocal, bobRemote)
	}

	if diff(aliceRemote, bobLocal) > 1 {
		f.t.Fatalf("commit mismatch: aliceRemote=%d bobLocal=%d",
			aliceRemote, bobLocal)
	}

	// Strong invariant: detect silent fund misallocation between Alice and
	// Bob.
	//
	// Each party's "claim" at any moment is the sum of:
	//   - LocalBalance on their local commitment.
	//   - CommitFee on their local commitment, only if they are the
	//     initiator (the initiator pays the on-chain fee, so those funds
	//     still belong to them).
	//   - Every HTLC in their local commitment whose funds *would still
	//     return to them* on resolution:
	//       * Incoming HTLC they have already settled → funds are theirs
	//         even though the commit round hasn't lifted the HTLC yet.
	//       * Outgoing HTLC the peer has NOT settled → refund possible.
	//     Incoming HTLCs they have not settled, and outgoing HTLCs the
	//     peer has already settled, are skipped: the funds belong to the
	//     other side.
	//
	// Compared against expectedAliceMSat / expectedBobMSat, which track the
	// running "final settled balance" assuming every observed settle goes
	// through. Any mismatch means funds were silently reassigned by the
	// link.

	// Build presence sets so we can both look up direction membership and
	// recognise when a pending settle has fully propagated.
	aliceIncoming := make(map[uint64]struct{})
	aliceOutgoing := make(map[uint64]struct{})
	for _, h := range aliceChanState.LocalCommitment.Htlcs {
		if h.Incoming {
			aliceIncoming[h.HtlcIndex] = struct{}{}
		} else {
			aliceOutgoing[h.HtlcIndex] = struct{}{}
		}
	}
	bobIncoming := make(map[uint64]struct{})
	bobOutgoing := make(map[uint64]struct{})
	for _, h := range bobChanState.LocalCommitment.Htlcs {
		if h.Incoming {
			bobIncoming[h.HtlcIndex] = struct{}{}
		} else {
			bobOutgoing[h.HtlcIndex] = struct{}{}
		}
	}

	// A pending settle is final once the HTLC has been dropped from both
	// sides' LocalCommitment (the second commit round has landed). The
	// new LocalBalance already reflects the settle, so we can stop carrying
	// the pending entry.
	for id := range f.aliceSettlesPending {
		_, inAlice := aliceIncoming[id]
		_, inBob := bobOutgoing[id]
		if !inAlice && !inBob {
			delete(f.aliceSettlesPending, id)
		}
	}
	for id := range f.bobSettlesPending {
		_, inBob := bobIncoming[id]
		_, inAlice := aliceOutgoing[id]
		if !inBob && !inAlice {
			delete(f.bobSettlesPending, id)
		}
	}

	aliceClaim := aliceChanState.LocalCommitment.LocalBalance
	if aliceChanState.IsInitiator {
		aliceClaim += lnwire.NewMSatFromSatoshis(
			aliceChanState.LocalCommitment.CommitFee,
		)
	}
	for _, h := range aliceChanState.LocalCommitment.Htlcs {
		if h.Incoming {
			// B→A HTLC: Alice's only if she has settled it.
			if _, ok := f.aliceSettlesPending[h.HtlcIndex]; ok {
				aliceClaim += h.Amt
			}
		} else {
			// A→B HTLC: still Alice's unless Bob has settled.
			if _, ok := f.bobSettlesPending[h.HtlcIndex]; !ok {
				aliceClaim += h.Amt
			}
		}
	}
	require.Equal(f.t, f.expectedAliceMSat, aliceClaim,
		"alice balance mismatch: expected=%v actual=%v",
		f.expectedAliceMSat, aliceClaim)

	bobClaim := bobChanState.LocalCommitment.LocalBalance
	if bobChanState.IsInitiator {
		bobClaim += lnwire.NewMSatFromSatoshis(
			bobChanState.LocalCommitment.CommitFee,
		)
	}
	for _, h := range bobChanState.LocalCommitment.Htlcs {
		if h.Incoming {
			// A→B HTLC: Bob's only if he has settled it.
			if _, ok := f.bobSettlesPending[h.HtlcIndex]; ok {
				bobClaim += h.Amt
			}
		} else {
			// B→A HTLC: still Bob's unless Alice has settled.
			if _, ok := f.aliceSettlesPending[h.HtlcIndex]; !ok {
				bobClaim += h.Amt
			}
		}
	}
	require.Equal(f.t, f.expectedBobMSat, bobClaim,
		"bob balance mismatch: expected=%v actual=%v",
		f.expectedBobMSat, bobClaim)
}

// htlcMsgStr returns a human-readable string for an lnwire.Message,
// including the HTLC ID for add/settle/fail messages and number of htlcs
// signed for commit msgs.
func htlcMsgStr(msg lnwire.Message) string {
	switch m := msg.(type) {
	case *lnwire.UpdateAddHTLC:
		return fmt.Sprintf("UpdateAddHTLC(id=%d, amount=%v)", m.ID,
			m.Amount)

	case *lnwire.UpdateFulfillHTLC:
		return fmt.Sprintf("UpdateFulfillHTLC(id=%d)", m.ID)
	case *lnwire.UpdateFailHTLC:
		return fmt.Sprintf("UpdateFailHTLC(id=%d)", m.ID)
	case *lnwire.CommitSig:
		return fmt.Sprintf("CommitSig(htlc_sigs=%d)", len(m.HtlcSigs))
	default:
		return msg.MsgType().String()
	}
}

// isExpectedLinkFailure returns true if the link failure reason is a known
// protocol boundary condition — i.e., a case where the protocol itself
// requires the link to be torn down rather than a bug in the commit logic.
// Failing links in these cases is correct behaviour; the test only fails if
// an unexpected reason is produced.
func isExpectedLinkFailure(reason string) bool {
	expected := []string{
		// Commitment fee pushes one party below their channel reserve.
		"below chan reserve",
		// Fee-exposure limit exceeded (too many dust HTLCs at this fee
		// rate).
		"fee threshold exceeded",
		// An HTLC update (add/settle/fail) arrived after the peer sent
		// stfu, entering quiescence.
		"update received after stfu",
		// The remote's NextLocalCommitHeight is behind what we have
		// already ACKed — the remote likely lost state.
		"possible remote commitment state data loss",
		// The remote's NextLocalCommitHeight is too far ahead — we
		// cannot safely sync commit chains.
		"unable to sync commit chains",
	}
	for _, substr := range expected {
		if strings.Contains(reason, substr) {
			return true
		}
	}

	return false
}

// drainMessages processes all pending messages.
// alicePeer.sentMsgs holds messages Alice sent to Bob → deliver to Bob's link.
// bobPeer.sentMsgs holds messages Bob sent to Alice → deliver to Alice's link.
func (f *fuzzFSM) drainMessages() {
	for {
		select {
		case msg := <-f.alicePeer.sentMsgs:
			// Alice sent this message → deliver to Bob's link.
			f.t.Logf("Alice→Bob: %v", htlcMsgStr(msg))

			f.bobLink.handleUpstreamMsg(
				f.t.Context(), msg,
			)
			if f.bobLink.failed {
				reason := f.bobLink.failReason
				if isExpectedLinkFailure(reason) {
					f.t.Logf("Bob's link correctly "+
						"terminated (expected "+
						"protocol boundary) after %v:"+
						" %v", htlcMsgStr(msg), reason)
					f.terminated = true

					return
				}
				f.t.Fatalf("Bob's link failed "+
					"unexpectedly after handling %v: %v",
					htlcMsgStr(msg), reason)
			}

		case msg := <-f.bobPeer.sentMsgs:
			// Bob sent this message → deliver to Alice's link.
			f.t.Logf("Bob→Alice: %v", htlcMsgStr(msg))
			f.aliceLink.handleUpstreamMsg(
				f.t.Context(), msg,
			)
			if f.aliceLink.failed {
				reason := f.aliceLink.failReason
				if isExpectedLinkFailure(reason) {
					f.t.Logf("Alice's link correctly "+
						"terminated (expected "+
						"protocol boundary) after %v:"+
						" %v", htlcMsgStr(msg), reason)
					f.terminated = true

					return
				}
				f.t.Fatalf("Alice's link failed "+
					"unexpectedly after handling %v: %v",
					htlcMsgStr(msg), reason)
			}

		default:
			return
		}
	}
}

// pickHTLCID selects an HTLC ID from preimages using htlcRef as an index,
// giving the fuzzer control over which pending HTLC gets resolved.
func (f *fuzzFSM) pickHTLCID(preimages map[uint64]lntypes.Preimage) uint64 {
	ids := make([]uint64, 0, len(preimages))
	for id := range preimages {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	return ids[f.htlcRef%uint64(len(ids))]
}

// sendHTLC initiates an outgoing HTLC from sender by registering a hodl
// invoice on the receiver's registry, committing the payment circuit on the
// sender's Switch, and injecting the UpdateAddHTLC directly into the link via
// handleDownstreamUpdateAdd. Returns the preimage and true on success, or an
// empty preimage and false if the channel is full (circuit and invoice are
// cleaned up in that case).
func (f *fuzzFSM) sendHTLC(sender *channelLink, htlcID uint64) (
	lntypes.Preimage, bool) {

	var senderCircuits *mockCircuitMap
	var invoiceRegistry *mockInvoiceRegistry
	// preimageSeed derives a unique preimage per attempt.
	var preimageSeed uint64
	switch sender {
	case f.aliceLink:
		senderCircuits = f.aliceCircuits
		invoiceRegistry = f.bobRegistry
		preimageSeed = MaxEventsPerRun + f.aliceHTLCAttempts + f.htlcRef
		f.aliceHTLCAttempts++
	case f.bobLink:
		senderCircuits = f.bobCircuits
		invoiceRegistry = f.aliceRegistry
		preimageSeed = MaxEventsPerRun*2 + f.bobHTLCAttempts + f.htlcRef
		f.bobHTLCAttempts++
	default:
		f.t.Fatal("HTLC sender does not exist")
	}

	// HTLC amount is derived from the htlcRef fuzz input and bounded
	// by the channel capacity.
	maxHTLC := lnwire.MilliSatoshi(sender.channel.Capacity * 1000)
	htlcAmt := lnwire.MilliSatoshi(f.htlcRef) % maxHTLC

	// HTLC preimage is derived from the htlcRef.
	htlc, preimage, err := generateSingleHopHtlc(
		f.t, htlcID, htlcAmt, preimageSeed,
	)
	if err != nil {
		f.t.Fatalf("failed to generate htlc: %v", err)
	}
	hodlInvoice := invoices.Invoice{
		CreationDate: time.Now(),
		HodlInvoice:  true,
		Terms: invoices.ContractTerm{
			FinalCltvDelta: testInvoiceCltvExpiry,
			Value:          htlc.Amount,
			Features: lnwire.NewFeatureVector(
				nil, lnwire.Features,
			),
			PaymentPreimage: &preimage,
		},
	}
	if err := invoiceRegistry.AddInvoice(
		context.Background(), hodlInvoice, htlc.PaymentHash,
	); err != nil {
		f.t.Fatalf("AddInvoice (hodl) failed: %v", err)
	}
	packet := &htlcPacket{
		// hop.Source marks this as a locally-initiated payment.
		incomingChanID: hop.Source,
		incomingHTLCID: htlcID,
		outgoingChanID: sender.ShortChanID(),
		htlc:           htlc,
		amount:         htlc.Amount,
	}
	circuit := newPaymentCircuit(&htlc.PaymentHash, packet)

	_, err = senderCircuits.CommitCircuits(circuit)
	if err != nil {
		f.t.Fatalf("CommitCircuits failed: %v", err)
	}
	packet.circuit = circuit
	err = sender.handleDownstreamUpdateAdd(f.t.Context(), packet)
	if err != nil {
		// Channel may be full. Clean up resources already allocated:
		// remove the circuit from the map and cancel the hold invoice.
		f.t.Logf("sendHTLC skipped: %v", err)
		_ = senderCircuits.DeleteCircuits(circuit.Incoming)
		_ = invoiceRegistry.CancelInvoice(
			context.Background(), htlc.PaymentHash,
		)

		return lntypes.Preimage{}, false
	}

	return preimage, true
}

// sendBadOnionHTLC sends an HTLC from sender after arming the receiver's
// onion decoder with a one-shot failure. The failure mode is picked from
// htlcRef so the fuzz corpus drives which branch of processRemoteAdds is
// exercised:
//   - htlcRef%3 == 0 → onionFailDecode  (DecodeHopIterator returns failcode)
//   - htlcRef%3 == 1 → onionFailPayload (HopPayload returns ErrInvalidPayload)
//   - htlcRef%3 == 2 → onionFailExtract (ExtractErrorEncrypter returns
//     failcode)
//
// The HTLC is failed-back by the receiver rather than settled, so the preimage
// is not tracked in alice/bobPreimages.
func (f *fuzzFSM) sendBadOnionHTLC(sender *channelLink,
	receiverDec *mockIteratorDecoder) {

	var (
		nextID *uint64
		who    string
	)
	switch sender {
	case f.aliceLink:
		nextID = &f.aliceNextHTLCID
		who = "Alice"
	case f.bobLink:
		nextID = &f.bobNextHTLCID
		who = "Bob"
	default:
		f.t.Fatal("HTLC sender does not exist")
	}

	// Bad-onion HTLCs are sent normally and then failed back by the
	// receiver, so they must be caped also by maxInflightHtlcs.
	if active := len(sender.channel.ActiveHtlcs()); active >=
		maxInflightHtlcs {

		f.t.Logf("%s Bad Onion Skipped: active HTLCs %d >= %d", who,
			active, maxInflightHtlcs)
		return
	}

	mode := onionFailMode(f.htlcRef%3) + 1

	receiverDec.mu.Lock()
	receiverDec.nextOnionFailMode = mode
	receiverDec.mu.Unlock()

	htlcID := *nextID
	_, ok := f.sendHTLC(sender, htlcID)
	if !ok {
		// sendHTLC didn't go through — clear the armed flag so it
		// doesn't fire on an unrelated future decode.
		receiverDec.mu.Lock()
		receiverDec.nextOnionFailMode = onionFailNone
		receiverDec.mu.Unlock()
		f.t.Logf("%s Bad Onion Skipped: channel full", who)

		return
	}
	*nextID++
	f.t.Logf("EV %s Send Bad Onion ID:%v mode:%v", who, htlcID, mode)
}

// sendCommitSig triggers a commitment signature from sender if there are
// pending local or remote updates to commit. It calls updateCommitTx directly,
// bypassing the link's internal event loop. Returns the number of pending
// updates and true if a CommitSig was sent, or 0 and false if there was
// nothing to commit.
func (f *fuzzFSM) sendCommitSig(sender *channelLink) (uint64, bool) {
	if f.terminated {
		return 0, false
	}

	// Send the commit_sig message only if there are pending commitment
	// update messages on the sender side, or if the sender is the remote
	// node.
	pending := sender.channel.NumPendingUpdates(
		lntypes.Local, lntypes.Remote,
	)

	err := sender.updateCommitTx(f.t.Context())
	if err != nil {
		if isExpectedLinkFailure(err.Error()) {
			f.t.Logf("sendCommitSig correctly failed "+
				"(expected protocol boundary): %v", err)
			f.terminated = true

			return 0, false
		}
		f.t.Fatalf("failed CommitSig %v", err)
	}

	return pending, true
}

// findLockedInAdd walks the settler/failer's fwdPkgs and returns the
// matching Add together with its AddRef. Returns ok=false when no fwdPkg
// contains the Add yet (i.e. the lock-in revoke round hasn't completed) —
// that is the correct signal that the HTLC isn't ready to be resolved.
func (f *fuzzFSM) findLockedInAdd(link *channelLink,
	htlcID uint64) (*lnwire.UpdateAddHTLC, channeldb.AddRef, bool) {

	fwdPkgs, err := link.channel.LoadFwdPkgs()
	if err != nil {
		f.t.Fatalf("LoadFwdPkgs failed: %v", err)
	}
	for _, pkg := range fwdPkgs {
		for i, lu := range pkg.Adds {
			add, ok := lu.UpdateMsg.(*lnwire.UpdateAddHTLC)
			if !ok {
				continue
			}
			if add.ID == htlcID {
				return add, channeldb.AddRef{
					Height: pkg.Height,
					Index:  uint16(i),
				}, true
			}
		}
	}

	return nil, channeldb.AddRef{}, false
}

// settleHTLC settles an incoming HTLC on the settler's link via
// channelLink.settleHTLC. This exercises the real link settle path
// (SettleHTLC + UpdateFulfillHTLC + HtlcNotifier) and feeds in a valid
// fwdPkg sourceRef so AckAddHtlcs bookkeeping runs on the next commit.
//
// The hodl invoice in the registry is left in ContractAccepted with a
// dangling subscription — that is intentional. Calling CancelInvoice would
// send a fail notification to the link's hodl subscriber, triggering an
// unwanted UpdateFailHTLC. Since htlcIDs are unique and the test is
// in-memory, the dangling entries cause no issues.
//
// Guard: the HTLC must be locked-in (in a fwdPkg) before we can settle it.
// On success the original Add amount is returned so the caller can update the
// shadow balance accounting consumed by assertInvariants.
func (f *fuzzFSM) settleHTLC(link *channelLink, htlcID uint64,
	preimage lntypes.Preimage) (lnwire.MilliSatoshi, bool) {

	add, sourceRef, ok := f.findLockedInAdd(link, htlcID)
	if !ok {
		f.t.Logf("settle skipped: HTLC %d not yet locked-in / no "+
			"fwdPkg", htlcID)
		return 0, false
	}

	if err := link.settleHTLC(preimage, htlcID, sourceRef); err != nil {
		f.t.Logf("settle skipped: %v", err)
		return 0, false
	}

	return add.Amount, true
}

// failHTLC fails an incoming locked-in HTLC on the failer's link via the
// real link fail path. The fail variant is picked from htlcRef so the fuzz
// corpus drives the choice:
//   - htlcRef%2 == 0 → channelLink.sendHTLCError (regular UpdateFailHTLC
//     with a TemporaryChannelFailure obfuscated by the mock encrypter).
//   - htlcRef%2 == 1 → channelLink.sendMalformedHTLCError
//     (UpdateFailMalformedHTLC with CodeInvalidOnionHmac).
//
// Both paths feed a real fwdPkg sourceRef into channel.FailHTLC /
// channel.MalformedFailHTLC so AckAddHtlcs bookkeeping runs on the next
// commit.
//
// Guard: the HTLC must be locked-in (present in a fwdPkg) before we can
// fail it.
func (f *fuzzFSM) failHTLC(link *channelLink, htlcID uint64) bool {
	add, sourceRef, ok := f.findLockedInAdd(link, htlcID)
	if !ok {
		f.t.Logf("fail skipped: HTLC %d not yet locked-in / no fwdPkg",
			htlcID)
		return false
	}

	if f.htlcRef%2 == 0 {
		// Regular failure path. The mock obfuscator wraps the
		// FailureMessage with a fake HMAC; the channel.FailHTLC call
		// inside sendHTLCError logs but does not return an error, so
		// we treat findLockedInAdd as the source of truth.
		failure := NewLinkError(lnwire.NewTemporaryChannelFailure(nil))
		link.sendHTLCError(
			*add, sourceRef, failure, NewMockObfuscator(), true,
		)
		f.t.Logf("fail HTLC %d via sendHTLCError", htlcID)

		return true
	}

	// Malformed failure path. The Add's onion blob from the fwdPkg
	// becomes the ShaOnionBlob the sender sees on the wire.
	link.sendMalformedHTLCError(
		htlcID, lnwire.CodeInvalidOnionHmac, add.OnionBlob, &sourceRef,
	)
	f.t.Logf("fail HTLC %d via sendMalformedHTLCError", htlcID)

	return true
}

// updateFee attempts to send a fee update on the given link.
func (f *fuzzFSM) updateFee(link *channelLink, newFee int) (error, bool) {
	// After STFU is sent the link must not emit any more update messages;
	// the receiving side would call stfuFailf and fail the link.
	if !link.quiescer.CanSendUpdates() {
		return nil, false
	}

	feePerKw := chainfee.SatPerKWeight(newFee)

	err := link.updateChannelFee(f.t.Context(), feePerKw)
	if err != nil {
		return err, false
	}

	return nil, true
}

// sendWarning emits an lnwire.Warning from sender's peer so that drainMessages
// delivers it to the other side's link. The payload alternates between
// printable ASCII and a binary blob (driven by htlcRef) to cover both branches
// of Warning.Warning().
func (f *fuzzFSM) sendWarning(sender *channelLink) {
	data := []byte(fmt.Sprintf("fuzz warning ref=%d", f.htlcRef))
	// Inject a binary blob every third call to cover the Warning.Warning()
	// branch that returns the raw data instead of a string.
	if f.htlcRef%3 == 0 {
		data = append(data, 0xff, 0x00, 0xfe)
	}

	err := sender.cfg.Peer.SendMessage(false, &lnwire.Warning{
		ChanID: sender.ChanID(),
		Data:   data,
	})
	if err != nil {
		f.t.Fatalf("failed to send Warning: %v", err)
	}
}

// initQuiescence initiates the quiescence handshake on the given link by
// sending a quiescence request.
func (f *fuzzFSM) initQuiescence(link *channelLink) error {
	req, _ := fn.NewReq[fn.Unit, fn.Result[lntypes.ChannelParty]](fn.Unit{})

	err := link.handleQuiescenceReq(req)
	if err != nil {
		return err
	}

	return nil
}

// resumeQuiescence resumes normal operation on both links after a quiescence
// session.
func (f *fuzzFSM) resumeQuiescence() error {
	aliceQ := f.aliceLink.quiescer.IsQuiescent()
	bobQ := f.bobLink.quiescer.IsQuiescent()
	if !aliceQ || !bobQ {
		return fmt.Errorf("Alice quiescenter state: %v, Bob quiescer "+
			"state: %v", aliceQ, bobQ,
		)
	}
	f.aliceLink.quiescer.Resume()
	f.bobLink.quiescer.Resume()

	return nil
}

// restartLink simulates a disconnect/reconnect for one side. The old link is
// stopped, any in-flight messages are discarded (lost during disconnect), and a
// fresh link is created over the same lnwallet.LightningChannel. The remote's
// current ChannelReestablish is injected into the new link's upstream so that
// resumeLink can complete the sync handshake. The local ChannelReestablish sent
// by the new link is then drained from the peer's sentMsgs — the still-running
// remote link doesn't participate in a second sync round.
func (f *fuzzFSM) restartLink(isAlice bool) {
	var (
		oldLink  *channelLink
		testChan *testLightningChannel
		remoteCh *testLightningChannel
		peer     *mockPeer
		registry *mockInvoiceRegistry
		pCache   *mockPreimageCache
		circuits *mockCircuitMap
	)
	if isAlice {
		oldLink = f.aliceLink
		testChan = f.alice
		remoteCh = f.bob
		peer = f.alicePeer
		registry = f.aliceRegistry
		pCache = f.alicePCache
		circuits = f.aliceCircuits
	} else {
		oldLink = f.bobLink
		testChan = f.bob
		remoteCh = f.alice
		peer = f.bobPeer
		registry = f.bobRegistry
		pCache = f.bobPCache
		circuits = f.bobCircuits
	}

	// Stop the old link to clean up its fwdPkgGarbager goroutine.
	oldLink.Stop()

	// Discard any messages that were in-flight when the link went down.
	for len(peer.sentMsgs) > 0 {
		<-peer.sentMsgs
	}

	// Snapshot the remote's current channel state for the sync handshake.
	remoteSyncMsg, err := remoteCh.channel.State().ChanSyncMsg()
	require.NoError(f.t, err)

	// When restartSyncHeight is non-zero, inject a mutated height so the
	// fuzzer can reach syncChanStates paths that are unreachable with
	// canonical messages.
	if f.restartSyncHeight != 0 {
		remoteSyncMsg.NextLocalCommitHeight = f.restartSyncHeight
	}

	// A real restart clears the Sphinx replay cache. Use a fresh decoder so
	// resolveFwdPkgs can re-decode onion blobs from scratch instead of
	// hitting stale, already-consumed iterator entries from the prior run.
	freshDecoder := newMockIteratorDecoder()

	newLink, newUpstream := f.hopNet.newFuzzLink(
		f.t, peer, testChan.channel, freshDecoder,
		registry, pCache, circuits, f.bestHeight,
		f.maxFeeExposure, f.maxFeeAllocation,
	)

	// Pre-load the remote's reestablish so resumeLink can read it
	// synchronously from upstream.
	newUpstream <- remoteSyncMsg

	err = newLink.resumeLink(f.t.Context())
	if err != nil {
		if f.restartSyncHeight == 0 {
			// Canonical sync message — any error is a real bug.
			require.NoError(f.t, err)
		}

		// Simulate the disconnect the real peer would perform: send an
		// Error to the remote so drainMessages can detect the failure
		// via processRemoteError → bobLink.failed.
		_ = peer.SendMessage(false, &lnwire.Error{
			Data: []byte(err.Error()),
		})

		return
	}

	// Disconnection cancels the in-progress STFU session on both sides.
	// Reset the remote link's quiescer unconditionally: Resume() clears
	// sent/received flags, cancels any timeout, and runs OnResume callbacks
	// that were deferred during quiescence (those callbacks may emit
	// messages that drainMessages will deliver to the new link below).
	if isAlice {
		f.aliceLink = newLink
		f.bobLink.quiescer.Resume()
	} else {
		f.bobLink = newLink
		f.aliceLink.quiescer.Resume()
	}

	// Drain the ChannelReestablish the new link sent out plus any messages
	// emitted by the remote's OnResume callbacks.
	f.drainMessages()
}

// applyEvent dispatches a single fuzz-generated event to the FSM for either
// Alice or Bob. Events that cannot be applied in the current state are silently
// skipped so the fuzzer can keep making progress without failing the test.
func (f *fuzzFSM) applyEvent(e Event) {
	if f.terminated {
		return
	}
	switch e {
	case EvAliceSendAddHtlc:
		if len(f.bobPreimages) >= maxInflightHtlcs {
			f.t.Logf("Alice Add HTLC Skipped: HTLCs pending > %v",
				maxInflightHtlcs)

			return
		}
		// Bob create the Hold Invoice, Alice send the HTLC.
		preimage, ok := f.sendHTLC(
			f.aliceLink, f.aliceNextHTLCID,
		)
		if !ok {
			f.t.Log("Alice Add HTLC Skipped: channel full")
			return
		}
		// bobPreimages are those Bob keep track to settle the hold
		// invoices.
		f.bobPreimages[f.aliceNextHTLCID] = preimage
		f.aliceNextHTLCID++
		f.t.Logf("EV Alice Send Add HTLC ID:%v", f.aliceNextHTLCID-1)
	case EvBobSendAddHtlc:
		if len(f.alicePreimages) >= maxInflightHtlcs {
			f.t.Logf("Bob Add HTLC Skipped: HTLCs pending > %v",
				maxInflightHtlcs)

			return
		}
		// Alice create the Hold Invoice, Bob send the HTLC.
		preimage, ok := f.sendHTLC(
			f.bobLink, f.bobNextHTLCID,
		)
		if !ok {
			f.t.Log("Bob Add HTLC Skipped: channel full")
			return
		}
		// alicePreimages are those Alice keep track to settle the hold
		// invoices.
		f.alicePreimages[f.bobNextHTLCID] = preimage
		f.bobNextHTLCID++
		f.t.Logf("EV Bob Send Add HTLC ID:%v", f.bobNextHTLCID-1)
	case EvAliceSendCommit:
		_, ok := f.sendCommitSig(f.aliceLink)
		if ok {
			f.t.Log("EV Alice Send Commit")
			return
		}
		f.t.Log("Alice skipped Commit")
	case EvBobSendCommit:
		_, ok := f.sendCommitSig(f.bobLink)
		if ok {
			f.t.Log("EV Bob Send Commit")
			return
		}
		f.t.Log("Bob skipped Commit")
	case EvAliceSettleHtlc:
		if len(f.alicePreimages) == 0 {
			f.t.Log("No Alice preimages to be settled")
			return
		}

		chosenID := f.pickHTLCID(f.alicePreimages)
		preimage := f.alicePreimages[chosenID]
		amt, ok := f.settleHTLC(
			f.aliceLink, chosenID, preimage,
		)
		if ok {
			// B→A settle: Alice claims amt, Bob loses it.
			f.expectedAliceMSat += amt
			f.expectedBobMSat -= amt
			f.aliceSettlesPending[chosenID] = struct{}{}
			delete(f.alicePreimages, chosenID)
			f.t.Logf("EV Alice Settle HTLC ID:%v amt:%v",
				chosenID, amt)

			return
		}
		f.t.Log("Alice Settle HTLC Skipped")
	case EvBobSettleHtlc:
		if len(f.bobPreimages) == 0 {
			f.t.Log("No Bob preimages to be settled")
			return
		}

		chosenID := f.pickHTLCID(f.bobPreimages)
		preimage := f.bobPreimages[chosenID]
		amt, ok := f.settleHTLC(f.bobLink, chosenID, preimage)
		if ok {
			// A→B settle: Bob claims amt, Alice loses it.
			f.expectedBobMSat += amt
			f.expectedAliceMSat -= amt
			f.bobSettlesPending[chosenID] = struct{}{}
			delete(f.bobPreimages, chosenID)
			f.t.Logf("EV Bob Settle HTLC ID:%v amt:%v",
				chosenID, amt)

			return
		}
		f.t.Log("Bob Settle HTLC Skipped")
	// Invalid settlement:
	// - if the number of tracked preimages is even, use both invalids
	//   preimage and HTLC ID.
	// - if it is odd, use an existing HTLC ID with an invalid preimage.
	case EvAliceInvalidHtlcSettlement:
		preimage := lntypes.Preimage{0x01}
		htlcID := uint64(MaxEventsPerRun)
		numPreimages := len(f.alicePreimages)
		if numPreimages%2 != 0 {
			for id := range f.alicePreimages {
				htlcID = id
				break
			}
		}
		err := f.aliceLink.channel.SettleHTLC(
			preimage, htlcID, nil, nil, nil,
		)
		require.Error(f.t, err)
		f.t.Logf("EV Alice Invalid HTLC Settlement: %v", err)
	case EvBobInvalidHtlcSettlement:
		preimage := lntypes.Preimage{0x01}
		htlcID := uint64(MaxEventsPerRun)
		numPreimages := len(f.bobPreimages)
		if numPreimages%2 != 0 {
			for id := range f.bobPreimages {
				htlcID = id
				break
			}
		}
		err := f.bobLink.channel.SettleHTLC(
			preimage, htlcID, nil, nil, nil,
		)
		require.Error(f.t, err)
		f.t.Logf("EV Bob Invalid HTLC Settlement: %v", err)
	case EvAliceFailHtlc:
		if len(f.alicePreimages) == 0 {
			f.t.Log("No Alice preimages to be failed")
			return
		}

		chosenID := f.pickHTLCID(f.alicePreimages)
		ok := f.failHTLC(f.aliceLink, chosenID)
		if ok {
			delete(f.alicePreimages, chosenID)
			f.t.Logf("EV Alice Fail HTLC ID:%v", chosenID)
			return
		}
		f.t.Log("Alice Fail HTLC Skipped")
	case EvBobFailHtlc:
		if len(f.bobPreimages) == 0 {
			f.t.Log("No Bob preimages to be failed")
			return
		}

		chosenID := f.pickHTLCID(f.bobPreimages)
		ok := f.failHTLC(f.bobLink, chosenID)
		if ok {
			delete(f.bobPreimages, chosenID)
			f.t.Logf("EV Bob Fail HTLC ID: %v", chosenID)
			return
		}
		f.t.Log("Bob Fail HTLC Skipped")
	case EvAliceFailNonExistentHtlc:
		htlcID := uint64(MaxEventsPerRun)
		reason := []byte("fuzz test")
		err := f.aliceLink.channel.FailHTLC(
			htlcID, reason, nil, nil, nil,
		)
		require.Error(f.t, err)
		f.t.Logf("EV Alice Invalid HTLC Failure: %v", err)
	case EvBobFailNonExistentHtlc:
		htlcID := uint64(MaxEventsPerRun)
		reason := []byte("fuzz test")
		err := f.bobLink.channel.FailHTLC(htlcID, reason, nil, nil, nil)
		require.Error(f.t, err)
		f.t.Logf("EV Bob Invalid HTLC Failure: %v", err)
	case EvAliceSendUpdateFee:
		newFee := ((len(f.aliceLink.channel.ActiveHtlcs()))+
			int(f.htlcRef))*100 + 1000

		err, ok := f.updateFee(f.aliceLink, newFee)
		if ok {
			f.t.Log("EV Alice Send Update Fee")
			return
		}
		f.t.Logf("Alice skipped Update Fee: %s", err)
	case EvBobSendUpdateFee:
		newFee := ((len(f.bobLink.channel.ActiveHtlcs()))+
			int(f.htlcRef))*100 + 1000

		err, ok := f.updateFee(f.bobLink, newFee)
		if ok {
			f.t.Log("EV Bob Send Update Fee")
			return
		}
		f.t.Logf("Bob skipped Update Fee: %s", err)
	case EvAliceInitQuiescence:
		err := f.initQuiescence(f.aliceLink)
		if err != nil {
			f.t.Logf("Alice skipped Init Quiescence: %s", err)
			return
		}
		f.t.Log("EV Alice Init Quiescence")
	case EvBobInitQuiescence:
		err := f.initQuiescence(f.bobLink)
		if err != nil {
			f.t.Logf("Bob skipped Init Quiescence: %s", err)
			return
		}
		f.t.Log("EV Bob Init Quiescence")
	case EvResumeQuiescence:
		err := f.resumeQuiescence()
		if err != nil {
			f.t.Logf("skipped Resume Quiescence: %s", err)
			return
		}
		f.t.Log("EV Resume Quiescence")
	case EvAliceRestartLink:
		f.restartLink(true)
		f.t.Log("EV Alice Restart Link")
	case EvBobRestartLink:
		f.restartLink(false)
		f.t.Log("EV Bob Restart Link")
	// Two back-to-back commits without draining Bob's revoke_and_ack
	// exercise the ErrNoWindow.
	case EvAliceSendCommitNoWindow:
		p1, _ := f.sendCommitSig(f.aliceLink)
		p2, _ := f.sendCommitSig(f.aliceLink)
		f.t.Logf("EV Alice Send Commit NoWindow pending1=%d "+
			"pending2=%d", p1, p2)
	case EvBobSendCommitNoWindow:
		p1, _ := f.sendCommitSig(f.bobLink)
		p2, _ := f.sendCommitSig(f.bobLink)
		f.t.Logf("EV Bob Send Commit NoWindow pending1=%d pending2=%d",
			p1, p2)
	// BOLT #1 lets a peer signal a non-fatal protocol issue via Warning.
	case EvAliceSendWarning:
		f.sendWarning(f.aliceLink)
		f.t.Log("EV Alice Send Warning")
	case EvBobSendWarning:
		f.sendWarning(f.bobLink)
		f.t.Log("EV Bob Send Warning")
	// Send an HTLC whose onion will fail to decode on the receiver side,
	// exercising the three error branches in processRemoteAdds. The mode
	// (decode / payload / extract) is picked from the fuzz corpus via
	// htlcRef so the fuzzer explores all three paths.
	case EvAliceSendBadOnion:
		f.sendBadOnionHTLC(f.aliceLink, f.bobDecoder)
	case EvBobSendBadOnion:
		f.sendBadOnionHTLC(f.bobLink, f.aliceDecoder)
	}
}

// TestChannelLinkFSMScenarios runs deterministic event sequences through the
// fuzz harness to validate each event type before enabling the full fuzzer.
func TestChannelLinkFSMScenarios(t *testing.T) {
	run := func(t *testing.T, events []Event) {
		t.Helper()

		f := newFuzzFSM(
			t, uint64(1_000_000), uint64(50), uint64(0), uint64(0),
		)

		f.htlcRef = uint64(10_000_000)

		for _, evt := range events {
			f.applyEvent(evt)
			f.drainMessages()
			f.assertInvariants()
		}
	}

	// runWithSyncHeight is like run but overrides NextLocalCommitHeight in
	// the remote ChannelReestablish on every restart event, so that
	// syncChanStates error paths are exercised deterministically.
	runWithSyncHeight := func(t *testing.T, syncHeight uint64,
		events []Event) {

		t.Helper()

		f := newFuzzFSM(
			t, uint64(1_000_000), uint64(50), uint64(0), uint64(0),
		)
		f.htlcRef = uint64(10_000_000)
		f.restartSyncHeight = syncHeight

		for _, evt := range events {
			f.applyEvent(evt)
			f.drainMessages()
			f.assertInvariants()
		}

		require.True(t, f.terminated,
			"expected link failure due to invalid sync height")
	}
	// No-op smoke test: all events that should silently skip on a clean
	// channel with no pending HTLCs.
	t.Run("noop_on_clean_channel", func(t *testing.T) {
		run(t, []Event{
			EvAliceSendCommit,
			EvBobSendCommit,
			EvAliceSettleHtlc,
			EvBobSettleHtlc,
			EvAliceFailHtlc,
			EvBobFailHtlc,
			EvBobSendUpdateFee,
			EvAliceSendCommit,
			EvBobSendCommit,
		})
	})

	// Alice adds an HTLC and both parties commit it.
	t.Run("alice_add_commit", func(t *testing.T) {
		run(t, []Event{
			EvAliceSendAddHtlc,
			EvAliceSendCommit,
		})
	})

	// Bob adds an HTLC and both parties commit it.
	t.Run("bob_add_commit", func(t *testing.T) {
		run(t, []Event{
			EvBobSendAddHtlc,
			EvBobSendCommit,
		})
	})

	// Multiple HTLCs in both directions, committed in one round.
	t.Run("multiple_htlcs_both_directions", func(t *testing.T) {
		run(t, []Event{
			EvAliceSendAddHtlc,
			EvAliceSendAddHtlc,
			EvBobSendAddHtlc,
			EvAliceSendCommit,
		})
	})

	// Alice adds an HTLC, both commit, Bob settlesl.
	t.Run("alice_add_bob_settle", func(t *testing.T) {
		run(t, []Event{
			EvAliceSendAddHtlc,
			EvAliceSendCommit,
			EvBobSettleHtlc,
			EvBobSendCommit,
		})
	})

	// Alice adds an HTLC, both commit, Bob fails. Partial: same numHtlcs
	// constraint applies to the final EvAliceSendCommit.
	t.Run("alice_add_bob_fail", func(t *testing.T) {
		run(t, []Event{
			EvAliceSendAddHtlc,
			EvAliceSendCommit,
			EvBobFailHtlc,
			EvBobSendCommit,
		})
	})
	// Alice restarts mid-session, then reconnects and settles an in-flight
	// HTLC and both parties commit the resolution.
	t.Run("alice_restart_link", func(t *testing.T) {
		run(t, []Event{
			EvAliceSendAddHtlc,
			EvAliceSendCommit,
			EvAliceRestartLink,
			EvBobSettleHtlc,
			EvBobSendCommit,
		})
	})

	// Bob restarts mid-session, then reconnects and settles an in-flight
	// HTLC and both parties commit the resolution.
	t.Run("bob_restart_link", func(t *testing.T) {
		run(t, []Event{
			EvBobSendAddHtlc,
			EvBobSendCommit,
			EvBobRestartLink,
			EvAliceSettleHtlc,
			EvAliceSendCommit,
		})
	})

	// Alice initiates quiescence while an HTLC is pending but not yet
	// committed.
	t.Run("alice_quiescence_link", func(t *testing.T) {
		run(t, []Event{
			EvAliceSendAddHtlc,
			EvAliceInitQuiescence,
			EvAliceSendCommit,
			EvResumeQuiescence,
		})
	})

	// Alice restarts while in  quiescence.
	t.Run("alice_restart_during_quiescence_link", func(t *testing.T) {
		run(t, []Event{
			EvAliceSendAddHtlc,
			EvAliceInitQuiescence,
			EvAliceRestartLink,
			EvAliceSendCommit,
			EvResumeQuiescence,
		})
	})

	// Alice restarts with a sync height below the remote tail — triggers
	// ErrCommitSyncRemoteDataLoss in syncChanStates.
	t.Run("alice_restart_sync_height_too_low", func(t *testing.T) {
		runWithSyncHeight(t, 1, []Event{
			EvAliceSendAddHtlc,
			EvAliceSendCommit,
			EvAliceRestartLink,
		})
	})

	// Alice restarts with a sync height far above the remote tip — triggers
	// ErrCannotSyncCommitChains in syncChanStates.
	t.Run("alice_restart_sync_height_too_high", func(t *testing.T) {
		runWithSyncHeight(t, math.MaxUint64, []Event{
			EvAliceSendAddHtlc,
			EvAliceSendCommit,
			EvAliceRestartLink,
		})
	})

	// Bob initiates quiescence while an HTLC is pending but not yet
	// committed.
	t.Run("bob_quiescence_link", func(t *testing.T) {
		run(t, []Event{
			EvBobSendAddHtlc,
			EvBobInitQuiescence,
			EvBobSendCommit,
			EvResumeQuiescence,
		})
	})

	// Alice signs two commitments back-to-back without delivering Bob's
	// revoke_and_ack in between. The second SignNextCommitment hits the
	// ErrNoWindow path.
	t.Run("alice_commit_no_window", func(t *testing.T) {
		run(t, []Event{
			EvAliceSendAddHtlc,
			EvAliceSendCommitNoWindow,
			EvBobSettleHtlc,
			EvBobSendCommit,
		})
	})

	// Same as above for Bob.
	t.Run("bob_commit_no_window", func(t *testing.T) {
		run(t, []Event{
			EvBobSendAddHtlc,
			EvBobSendCommitNoWindow,
			EvAliceSettleHtlc,
			EvAliceSendCommit,
		})
	})

	// Warnings are non-fatal per BOLT #1. The link logs and keeps going.
	t.Run("alice_warning_interleaved", func(t *testing.T) {
		run(t, []Event{
			EvAliceSendAddHtlc,
			EvAliceSendWarning,
			EvAliceSendCommit,
			EvBobSendWarning,
			EvBobSettleHtlc,
			EvBobSendCommit,
		})
	})

	// Direct assertion that the ErrNoWindow path is reachable: after one
	// SignNextCommitment the remote chain is unacked, so a second call
	// must return ErrNoWindow. This complements the scenarios above by
	// pinning the precondition the new events rely on.
	t.Run("sign_next_commitment_no_window", func(t *testing.T) {
		f := newFuzzFSM(
			t, uint64(1_000_000), uint64(50), uint64(0), uint64(0),
		)
		f.htlcRef = uint64(10_000_000)

		f.applyEvent(EvAliceSendAddHtlc)

		_, err := f.aliceLink.channel.SignNextCommitment(t.Context())
		require.NoError(t, err)

		_, err = f.aliceLink.channel.SignNextCommitment(t.Context())
		require.ErrorIs(t, err, lnwallet.ErrNoWindow)
	})

	t.Run("all_events", func(t *testing.T) {
		run(t, []Event{
			// Warm-up: warnings and traffic.
			EvAliceSendWarning,
			EvBobSendWarning,
			EvAliceSendAddHtlc,
			EvAliceSendAddHtlc,
			EvAliceSendAddHtlc,
			EvAliceRestartLink,
			EvBobSendAddHtlc,
			EvBobSendAddHtlc,
			EvBobRestartLink,
			EvBobSendUpdateFee,
			EvAliceSendUpdateFee,
			EvBobSendAddHtlc,
			EvAliceSendCommitNoWindow,
			EvBobSendCommit,
			EvAliceInvalidHtlcSettlement,
			EvBobInvalidHtlcSettlement,
			EvAliceFailNonExistentHtlc,
			EvBobFailNonExistentHtlc,
			EvBobSendCommitNoWindow,
			EvBobInitQuiescence,
			EvBobSendCommit,
			EvResumeQuiescence,
			EvAliceInitQuiescence,
			EvAliceFailHtlc,
			EvBobFailHtlc,
			EvAliceSettleHtlc,
			EvBobSettleHtlc,
			EvResumeQuiescence,
			EvAliceSendCommit,
			EvAliceSendAddHtlc,
			EvBobSendAddHtlc,
			EvAliceSendCommit,
			EvAliceFailHtlc,
		})
	})

	// Bad-onion scenarios — each htlcRef value selects one of the three
	// failure branches in processRemoteAdds (decode / payload / extract).
	// Driving Alice→Bob and Bob→Alice for each mode runs the bad HTLC
	// through a full add/commit/revoke cycle, so the receiver hits the
	// targeted branch and fails the HTLC back via UpdateFailHTLC.
	badOnionEvents := []Event{
		EvAliceSendBadOnion,
		EvAliceSendCommit,
		EvBobSendBadOnion,
		EvBobSendCommit,
	}
	runWithHtlcRef := func(t *testing.T, htlcRef uint64, events []Event) {
		t.Helper()
		f := newFuzzFSM(
			t, uint64(1_000_000), uint64(50), uint64(0), uint64(0),
		)
		f.htlcRef = htlcRef
		for _, evt := range events {
			f.applyEvent(evt)
			f.drainMessages()
			f.assertInvariants()
		}
	}
	t.Run("bad_onion_decode", func(t *testing.T) {
		// htlcRef%3 == 0 → onionFailDecode.
		runWithHtlcRef(t, uint64(3_000_000), badOnionEvents)
	})
	t.Run("bad_onion_payload", func(t *testing.T) {
		// htlcRef%3 == 1 → onionFailPayload.
		runWithHtlcRef(t, uint64(3_000_001), badOnionEvents)
	})
	t.Run("bad_onion_extract", func(t *testing.T) {
		// htlcRef%3 == 2 → onionFailExtract.
		runWithHtlcRef(t, uint64(3_000_002), badOnionEvents)
	})
}

// FuzzChannelLinkFSM is a coverage-guided fuzz test for the two-party
// commitment protocol between Alice and Bob. Each byte of the corpus is
// interpreted as one of the NumEvents protocol actions for either peer. After
// every event the pending messages are drained and assertInvariants verifies
// that both sides remain in a consistent state (matching commitment heights,
// balanced totals). The fuzzer explores arbitrary interleavings of these
// actions to find protocol violations that deterministic scenarios might miss.
func FuzzChannelLinkFSM(f *testing.F) {
	// seed input
	// restartSyncHeight=0 seeds the canonical case (no height mutation).
	// maxFeeExposureGen=0  → DefaultMaxFeeExposure (no override).
	// maxFeeAllocationGen=0 → DefaultMaxLinkFeeAllocation (no override).
	f.Add(uint64(1_000_000), uint64(10_000_000), uint64(50), uint64(0),
		uint64(0), uint64(0),
		[]byte{byte(EvAliceSendAddHtlc), byte(EvAliceSendCommit),
			byte(EvBobSendAddHtlc), byte(EvBobSendCommit),
			byte(EvAliceSettleHtlc), byte(EvBobSettleHtlc),
			byte(EvAliceSendUpdateFee), byte(EvAliceSendAddHtlc),
			byte(EvAliceSendWarning), byte(EvBobRestartLink),
			byte(EvAliceSendCommit), byte(EvBobSendAddHtlc),
			byte(EvBobSendCommit), byte(EvAliceFailHtlc),
			byte(EvBobSendWarning), byte(EvBobFailNonExistentHtlc),
			byte(EvBobFailHtlc), byte(EvBobSendUpdateFee),
			byte(EvAliceSendAddHtlc), byte(EvAliceSendCommit),
			byte(EvBobSendAddHtlc), byte(EvBobSendCommit),
			byte(EvBobInvalidHtlcSettlement),
			byte(EvBobSendCommitNoWindow),
			byte(EvAliceSendBadOnion),
			byte(EvAliceFailNonExistentHtlc),
			byte(EvAliceFailHtlc), byte(EvAliceRestartLink),
			byte(EvBobFailHtlc), byte(EvBobInitQuiescence),
			byte(EvBobSendUpdateFee), byte(EvAliceSendAddHtlc),
			byte(EvAliceSendCommit), byte(EvResumeQuiescence),
			byte(EvAliceSendCommitNoWindow),
			byte(EvBobSendBadOnion),
			byte(EvBobSettleHtlc), byte(EvBobSendAddHtlc),
			byte(EvAliceInvalidHtlcSettlement),
			byte(EvBobSendCommit), byte(EvAliceFailHtlc),
			byte(EvResumeQuiescence), byte(EvAliceRestartLink),
			byte(EvAliceInitQuiescence)},
	)
	f.Fuzz(func(t *testing.T, channelSize, htlcRef uint64,
		aliceShareGen, restartSyncHeight, maxFeeExposureGen,
		maxFeeAllocationGen uint64, data []byte) {

		fuzzFSM := newFuzzFSM(
			t, channelSize, aliceShareGen, maxFeeExposureGen,
			maxFeeAllocationGen,
		)

		fuzzFSM.htlcRef = htlcRef
		fuzzFSM.restartSyncHeight = restartSyncHeight

		// Guard against excessively long inputs that would make the
		// test run too long.
		if len(data) > MaxEventsPerRun {
			return
		}

		for _, b := range data {
			evt := Event(b % uint8(NumEvents))
			fuzzFSM.applyEvent(evt)
			fuzzFSM.drainMessages()
			if fuzzFSM.terminated {
				return
			}
			fuzzFSM.assertInvariants()
		}
	})
}
