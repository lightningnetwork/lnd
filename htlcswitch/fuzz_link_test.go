package htlcswitch

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"maps"
	"math"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
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

// fuzzSigVerifier is the paired verifier for fuzzSigner. It accepts a
// signature only if its DER serialization (preserved through the lnwire
// round-trip) is exactly the one fuzzSigner produces for sigHash, so a
// corrupted r is rejected just like a corrupted s, as real ECDSA would.
func fuzzSigVerifier(sig input.Signature, sigHash []byte,
	_ *btcec.PublicKey) bool {

	return bytes.Equal(sig.Serialize(), fuzzDERSig(sigHash))
}

// fuzzCommitKeyDeriver is a trivial CommitKeyDeriverFunc for fuzz harnesses.
// It mirrors the local/remote base-point selection of DeriveCommitmentKeys but
// returns the raw base points without any secp256k1 scalar multiplication,
// eliminating the ~30% CPU overhead of TweakPubKey/DeriveRevocationPubkey on
// every commit round. Both Alice and Bob call this with mirrored arguments and
// arrive at the same underlying public keys, so commitment tx scripts remain
// consistent across both sides.
func fuzzCommitKeyDeriver(commitPoint *btcec.PublicKey,
	whoseCommit lntypes.ChannelParty, _ channeldb.ChannelType, localChanCfg,
	remoteChanCfg *channeldb.ChannelConfig) *lnwallet.CommitmentKeyRing {

	localBasePoint := localChanCfg.PaymentBasePoint
	if whoseCommit.IsLocal() {
		localBasePoint = localChanCfg.DelayBasePoint
	}

	var toLocalKey, toRemoteKey, revocationKey *btcec.PublicKey
	if whoseCommit.IsLocal() {
		toLocalKey = localChanCfg.DelayBasePoint.PubKey
		toRemoteKey = remoteChanCfg.PaymentBasePoint.PubKey
		revocationKey = remoteChanCfg.RevocationBasePoint.PubKey
	} else {
		toLocalKey = remoteChanCfg.DelayBasePoint.PubKey
		toRemoteKey = localChanCfg.PaymentBasePoint.PubKey
		revocationKey = localChanCfg.RevocationBasePoint.PubKey
	}

	return &lnwallet.CommitmentKeyRing{
		CommitPoint: commitPoint,
		// Tweaks are cheap (just SHA256), keep them accurate.
		LocalCommitKeyTweak: input.SingleTweakBytes(
			commitPoint, localBasePoint.PubKey,
		),
		LocalHtlcKeyTweak: input.SingleTweakBytes(
			commitPoint, localChanCfg.HtlcBasePoint.PubKey,
		),
		// Skip TweakPubKey/DeriveRevocationPubkey — return base points
		// directly to avoid secp256k1 scalar multiplications.
		LocalHtlcKey:  localChanCfg.HtlcBasePoint.PubKey,
		RemoteHtlcKey: remoteChanCfg.HtlcBasePoint.PubKey,
		ToLocalKey:    toLocalKey,
		ToRemoteKey:   toRemoteKey,
		RevocationKey: revocationKey,
	}
}

// fuzzFeeEstimator is a chainfee.Estimator whose rates the fuzz events set
// directly. Fee updates then reach the link through handleUpdateFee, the same
// path production uses, so a hostile network fee still goes through the real
// clamping to the relay floor and the fee allocation instead of being handed
// raw to updateChannelFee.
type fuzzFeeEstimator struct {
	feeRate  chainfee.SatPerKWeight
	relayFee chainfee.SatPerKWeight
}

func (e *fuzzFeeEstimator) EstimateFeePerKW(uint32) (chainfee.SatPerKWeight,
	error) {

	return e.feeRate, nil
}

func (e *fuzzFeeEstimator) RelayFeePerKW() chainfee.SatPerKWeight {
	return e.relayFee
}

func (e *fuzzFeeEstimator) Start() error { return nil }

func (e *fuzzFeeEstimator) Stop() error { return nil }

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
	EvAliceHoldOutbox
	EvBobHoldOutbox
	EvDeliverAliceToBob
	EvDeliverBobToAlice
	EvAliceTamper
	EvBobTamper
	EvReconnect
	EvArmDisconnect
	EvMineBlocks

	NumEvents
)

// fuzzInput is a read cursor over the fuzz byte stream. The fuzz loop reads
// each event byte from it and the event then reads its own arguments (HTLC
// amount, fee rate, which HTLC to resolve, ...) from the bytes that follow, so
// every action in a run gets independent, fuzzer-controlled parameters. Reads
// past the end return zero, which every event maps to its default behaviour.
type fuzzInput struct {
	data []byte
	pos  int
}

// done reports whether the stream has been fully consumed.
func (in *fuzzInput) done() bool {
	return in.pos >= len(in.data)
}

// u8 reads one byte, or returns 0 once the stream is exhausted.
func (in *fuzzInput) u8() uint8 {
	if in.done() {
		return 0
	}
	b := in.data[in.pos]
	in.pos++

	return b
}

// u16 reads a big-endian uint16, zero-padding past the end of the stream.
func (in *fuzzInput) u16() uint16 {
	return uint16(in.u8())<<8 | uint16(in.u8())
}

// u32 reads a big-endian uint32, zero-padding past the end of the stream.
func (in *fuzzInput) u32() uint32 {
	return uint32(in.u16())<<16 | uint32(in.u16())
}

// u64 reads a big-endian uint64, zero-padding past the end of the stream.
func (in *fuzzInput) u64() uint64 {
	return uint64(in.u32())<<32 | uint64(in.u32())
}

// outboxCapacity is the buffer of each peer's outbox. mockPeer.SendMessage
// blocks on a full outbox, and held messages (see EvAliceHoldOutbox) pile up
// for up to maxHoldEvents events, each of which can emit a fail per in-flight
// HTLC plus the commit dance, so the default of 100 could deadlock a run.
const outboxCapacity = 1024

// maxHoldEvents bounds how many events an outbox hold lasts.
const maxHoldEvents = 8

// maxFlushRounds bounds the number of commit rounds flushAndAssertConverged
// drives before declaring the channel stuck. A single commit_sig normally
// triggers the full dance (revoke, counter-commit, revoke), so two rounds
// already cover the case where both sides owe a commitment.
const maxFlushRounds = 4

// MaxWorkPerRun bounds the total deterministic work weight a single fuzz
// run may spend (see costOf). Unlike a per-event count cap it limits the
// real (adds × commit rounds) cost, so heavy events like the
// CommitNoWindow and BadOnion variants cannot make a run hang.
const MaxWorkPerRun = 2000

// nonExistentHTLCID is an HTLC index the channel will never allocate (IDs
// grow from 0), used to drive the invalid settle/fail error paths.
const nonExistentHTLCID = uint64(math.MaxUint64)

// failureLog records the LinkFailureError each side's link reports through
// OnChannelFailure, so the tamper oracle can check which validation fired.
// It outlives the links, which restartLink replaces.
type failureLog struct {
	alice, bob *LinkFailureError
}

// recorder returns the OnChannelFailure hook for one side's link.
func (l *failureLog) recorder(isAlice bool) func(LinkFailureError) {
	return func(linkErr LinkFailureError) {
		if isAlice {
			l.alice = &linkErr
		} else {
			l.bob = &linkErr
		}
	}
}

// resolvedHTLC remembers an incoming HTLC a side settled or failed until the
// resolution is locked in, so a reconnect that drops it can restore the HTLC
// to the tracking maps (and, for a settle, undo the shadow balance credit).
type resolvedHTLC struct {
	amt      lnwire.MilliSatoshi
	preimage lntypes.Preimage
}

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

	// height is the chain tip bestHeight reports.
	height *uint32

	// Preimages for created HTLCs
	alicePreimages map[uint64]lntypes.Preimage
	bobPreimages   map[uint64]lntypes.Preimage

	// Circuit keys for the HTLCs each side originates. The tracking maps
	// key HTLCs by the index the channel assigns instead, since that one
	// rolls back when a reconnect drops an uncommitted add, while circuit
	// keys must stay unique.
	aliceCircuitSeq uint64
	bobCircuitSeq   uint64
	htlcRef         uint64

	// Monotonically-increasing attempt counters used to derive unique
	// preimageSeed.
	aliceHTLCAttempts uint64
	bobHTLCAttempts   uint64

	// aliceHold and bobHold count the upcoming drains during which that
	// side's outbox stays queued instead of being delivered, letting the
	// fuzzer interleave the two directions (e.g. crossing commit_sigs).
	// Order within a direction is always preserved, as on a real
	// connection.
	aliceHold, bobHold int

	// cutAfter, when non-zero, makes the next step deliver only that many
	// messages of the exchange its event starts before the connection
	// drops (see EvArmDisconnect).
	cutAfter int

	// tamper is the protocol corruption Mallory has armed or applied (see
	// fuzz_link_mallory_test.go), nil in an honest run.
	tamper *tamper

	// failures records the failure codes the links report.
	failures *failureLog

	// in is the argument stream events read their parameters from. The
	// fuzz loop shares one stream between event bytes and arguments;
	// scenarios hand each event its own (possibly empty) stream.
	in *fuzzInput

	// maxFeeExposure is the per-link MaxFeeExposure threshold passed to
	// newFuzzLink for both Alice and Bob (kept on the FSM so restartLink
	// can rebuild a link with the same value).
	maxFeeExposure lnwire.MilliSatoshi

	// maxFeeAllocation is the per-link MaxFeeAllocation fraction (0..1]
	// of channel balance allowed for the commitment fee. Kept on the FSM so
	// restartLink can rebuild a link with the same value.
	maxFeeAllocation float64

	// feeEstimator is shared by both links; fee update events set its
	// network fee before calling handleUpdateFee.
	feeEstimator *fuzzFeeEstimator

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
	aliceSettlesPending map[uint64]resolvedHTLC
	bobSettlesPending   map[uint64]resolvedHTLC

	// Fail round-trip tracking, the counterpart of the settle maps: the
	// incoming HTLCs each side has failed whose removal has not landed on
	// both commitments yet. A reconnect can drop the fail, and the HTLC
	// then has to be tracked again (see reconcileAfterReconnect).
	aliceFailsPending map[uint64]resolvedHTLC
	bobFailsPending   map[uint64]resolvedHTLC

	// workSpent accumulates the deterministic work weight of every
	// applied event (see costOf). A run ends once it exceeds
	// MaxWorkPerRun, bounding the worst-case (adds × commit rounds) cost
	// that a flat per-event count cap cannot.
	workSpent int
}

// shapeBits reads fixed-width fields off a fuzz-provided word, low bits first.
type shapeBits uint32

// take returns the next n bits as an int.
func (b *shapeBits) take(n uint) int {
	v := int(uint32(*b) & (1<<n - 1))
	*b >>= n

	return v
}

// fuzzChanShape decodes the channel shape from shapeGen. Zero keeps
// createTestChannel's default shape, which every corpus entry recorded
// before shapes existed runs with. Any other value draws each field within
// the bounds lnd's VerifyConstraints enforces when a channel is opened, so
// the harness never runs a channel lnd would have refused:
//   - 1 bit:   tweakless, or anchors with zero-fee HTLC transactions;
//   - 8 bits:  initial fee rate, at most the 2500 sat/kw anchor cap (the
//     link's MaxAnchorsCommitFeeRate) for anchor channels;
//   - per side, 3 bits: dust limit in [354, 1061] sat, inside the
//     [maxWitnessLimit, 3*maxWitnessLimit] band;
//   - per side, 2 bits: reserve of 1, 2, 5 or 10% of the capacity, never
//     below that side's dust limit;
//   - per side, 2 bits: MinHTLC of 1, 1000, 10^4 or 10^5 msat;
//   - per side, 2 bits: MaxAcceptedHtlcs of 5, 15, 30 or maxInflightHtlcs.
//
// It returns the shape and the two reserves.
func fuzzChanShape(shapeGen uint32, capacity btcutil.Amount) (
	testChannelShape, btcutil.Amount, btcutil.Amount) {

	shape := defaultTestChannelShape()
	defaultReserve := capacity / 10
	if shapeGen == 0 {
		return shape, defaultReserve, defaultReserve
	}

	bits := shapeBits(shapeGen)
	feeSel := chainfee.SatPerKWeight(0)
	if bits.take(1) == 1 {
		shape.chanType = channeldb.SingleFunderTweaklessBit |
			channeldb.AnchorOutputsBit | channeldb.ZeroHtlcTxFeeBit

		// The top bit, otherwise unused, upgrades the anchor channel
		// to a production simple taproot channel.
		if shapeGen&(1<<31) != 0 {
			shape.chanType |= channeldb.SimpleTaprootFeatureBit |
				channeldb.TaprootFinalBit
		}
		feeSel = chainfee.SatPerKWeight(bits.take(8))
		shape.feePerKw = min(chainfee.FeePerKwFloor+feeSel*9, 2500)
	} else {
		feeSel = chainfee.SatPerKWeight(bits.take(8))
		shape.feePerKw = chainfee.FeePerKwFloor + feeSel*50
	}

	reservePct := []btcutil.Amount{1, 2, 5, 10}
	minHTLC := []lnwire.MilliSatoshi{1, 1000, 10_000, 100_000}
	maxAccepted := []uint16{5, 15, 30, maxInflightHtlcs}
	side := func(dust *btcutil.Amount, minHtlc *lnwire.MilliSatoshi,
		maxHtlcs *uint16) btcutil.Amount {

		*dust = 354 + btcutil.Amount(bits.take(3))*101
		reserve := max(capacity*reservePct[bits.take(2)]/100, *dust)
		*minHtlc = minHTLC[bits.take(2)]
		*maxHtlcs = maxAccepted[bits.take(2)]

		return reserve
	}
	aliceReserve := side(&shape.aliceDustLimit, &shape.aliceMinHTLC,
		&shape.aliceMaxAcceptedHtlcs)
	bobReserve := side(&shape.bobDustLimit, &shape.bobMinHTLC,
		&shape.bobMaxAcceptedHtlcs)

	return shape, aliceReserve, bobReserve
}

// newFuzzFSM initializes and returns a new fuzz finite state machine (FSM)
// instance with the specified channel size and configuration parameters.
// chanShape picks the channel type and parameters (see fuzzChanShape).
func newFuzzFSM(t *testing.T, channelSize, aliceShareGen uint64,
	chanShape uint32, maxFeeExposureGen,
	maxFeeAllocationGen uint64) *fuzzFSM {
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

	shape, aliceReserve, bobReserve := fuzzChanShape(
		chanShape, btcutil.Amount(chanCapacity),
	)

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
	// The trivial signer, verifier and key deriver only model ECDSA over
	// untweaked keys; taproot channels sign with MuSig2 and Schnorr, so
	// they run on real cryptography.
	chanOpts := []testChannelOpt{withTestChanShape(shape)}
	if !shape.chanType.IsTaproot() {
		chanOpts = append(chanOpts,
			withTestSignerFactory(mkFuzzSigner),
			withTestChanOpts(
				lnwallet.WithSigVerifier(fuzzSigVerifier),
				lnwallet.WithCommitKeyDeriver(
					fuzzCommitKeyDeriver,
				),
			),
		)
	}
	alice, bob, err := createTestChannel(t, alicePrivKey, bobPrivKey,
		aliceAmount, bobAmount, aliceReserve, bobReserve, SchanID,
		chanOpts...,
	)
	require.NoError(t, err)

	alicePeer := &mockPeer{
		sentMsgs: make(chan lnwire.Message, outboxCapacity),
		quit:     make(chan struct{}),
	}
	bobPeer := &mockPeer{
		sentMsgs: make(chan lnwire.Message, outboxCapacity),
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
	failures := &failureLog{}
	feeEstimator := &fuzzFeeEstimator{
		feeRate:  chainfee.FeePerKwFloor,
		relayFee: chainfee.FeePerKwFloor,
	}

	// Each side gets its own registry, preimage cache, and circuit map.
	// These are plain in-memory mocks with no background goroutines, so
	// there is nothing to race with the test goroutine.
	aliceRegistry := newMockRegistry(t)
	bobRegistry := newMockRegistry(t)
	alicePCache := newMockPreimageCache()
	bobPCache := newMockPreimageCache()
	aliceCircuits := &mockCircuitMap{lookup: make(chan *PaymentCircuit)}
	bobCircuits := &mockCircuitMap{lookup: make(chan *PaymentCircuit)}

	// Both links read the chain tip through bestHeight; EvMineBlocks
	// advances it.
	height := new(uint32)
	*height = uint32(blockHeight)
	bestHeight := func() uint32 { return *height }

	aliceDecoder := newMockIteratorDecoder()
	bobDecoder := newMockIteratorDecoder()

	// Create both links without starting the htlcManager goroutine and
	// without a Switch. newFuzzLink sets link.upstream directly so we can
	// drive reestablishment synchronously below.
	aliceLink, aliceUpstream := hopNet.newFuzzLink(
		t, alicePeer, alice.channel, aliceDecoder,
		aliceRegistry, alicePCache, aliceCircuits, bestHeight,
		maxFeeExposure, maxFeeAllocation, feeEstimator,
		failures.recorder(true),
	)
	bobLink, bobUpstream := hopNet.newFuzzLink(
		t, bobPeer, bob.channel, bobDecoder,
		bobRegistry, bobPCache, bobCircuits, bestHeight,
		maxFeeExposure, maxFeeAllocation, feeEstimator,
		failures.recorder(false),
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

	f := &fuzzFSM{
		t:                   t,
		in:                  &fuzzInput{},
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
		height:              height,
		maxFeeExposure:      maxFeeExposure,
		maxFeeAllocation:    maxFeeAllocation,
		feeEstimator:        feeEstimator,
		failures:            failures,
		expectedAliceMSat:   lnwire.NewMSatFromSatoshis(aliceAmount),
		expectedBobMSat:     lnwire.NewMSatFromSatoshis(bobAmount),
		aliceSettlesPending: make(map[uint64]resolvedHTLC),
		bobSettlesPending:   make(map[uint64]resolvedHTLC),
		aliceFailsPending:   make(map[uint64]resolvedHTLC),
		bobFailsPending:     make(map[uint64]resolvedHTLC),
	}

	// Deliver the handshake messages resumeLink queued (reestablish and
	// channel_ready) so every run starts from an idle, synced channel and
	// the first hold or single delivery acts on protocol traffic.
	f.drainMessages()

	return f
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

	// The initiator also funds both anchor outputs, which no balance or
	// commitment fee field records.
	var anchors lnwire.MilliSatoshi
	if aliceChanState.ChanType.HasAnchors() {
		anchors = lnwire.NewMSatFromSatoshis(2 * lnwallet.AnchorSize)
	}

	// Conservation: every commitment either party holds, local or remote,
	// must account for exactly the channel capacity across both balances,
	// the commitment fee, the anchors and the HTLCs.
	capacity := lnwire.NewMSatFromSatoshis(aliceChanState.Capacity)
	for _, c := range []struct {
		name   string
		commit *channeldb.ChannelCommitment
	}{
		{"alice local", &aliceChanState.LocalCommitment},
		{"alice remote", &aliceChanState.RemoteCommitment},
		{"bob local", &bobChanState.LocalCommitment},
		{"bob remote", &bobChanState.RemoteCommitment},
	} {
		total := c.commit.LocalBalance + c.commit.RemoteBalance +
			lnwire.NewMSatFromSatoshis(c.commit.CommitFee) + anchors
		for _, h := range c.commit.Htlcs {
			total += h.Amt
		}
		require.Equalf(f.t, capacity, total, "%s commitment does not "+
			"add up to the channel capacity", c.name)
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
	for id := range f.aliceFailsPending {
		_, inAlice := aliceIncoming[id]
		_, inBob := bobOutgoing[id]
		if !inAlice && !inBob {
			delete(f.aliceFailsPending, id)
		}
	}
	for id := range f.bobFailsPending {
		_, inBob := bobIncoming[id]
		_, inAlice := aliceOutgoing[id]
		if !inBob && !inAlice {
			delete(f.bobFailsPending, id)
		}
	}

	aliceClaim := aliceChanState.LocalCommitment.LocalBalance
	if aliceChanState.IsInitiator {
		aliceClaim += lnwire.NewMSatFromSatoshis(
			aliceChanState.LocalCommitment.CommitFee,
		) + anchors
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
		) + anchors
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

// flushAndAssertConverged ends a run that was not terminated by an expected
// link failure. It drives commit rounds until neither side owes a commitment
// and then checks that the channel has actually converged, catching stuck
// states that the per-event safety invariants cannot see: an update that is
// never committed, a commitment that is never revoked, or two parties that
// disagree on the commitment they both signed.
func (f *fuzzFSM) flushAndAssertConverged() {
	// A tamper that never hit a message leaves the run honest; disarm it
	// so the flush itself runs untampered.
	if f.tamper != nil && !f.tamper.applied {
		f.tamper = nil
	}

	// Release any held outbox first: those messages are already in flight
	// and must land in the session state they were sent in (a held stfu
	// reply arriving after the session ended would be invalid).
	f.aliceHold, f.bobHold = 0, 0
	f.drainMessages()
	if f.terminated {
		return
	}

	// A quiescence session always ends in a real node, either when the
	// quiescent operation completes or when the STFU timeout disconnects
	// the peer. End any open session so that updates it deferred (such as
	// failing back a bad-onion HTLC) can go out before convergence is
	// demanded.
	f.aliceLink.quiescer.Resume()
	f.bobLink.quiescer.Resume()
	f.drainMessages()

	for round := 0; round < maxFlushRounds; round++ {
		progressed := false
		for _, link := range []*channelLink{f.aliceLink, f.bobLink} {
			if f.terminated {
				return
			}
			if !link.channel.OweCommitment() {
				continue
			}

			f.sendCommitSig(link)
			f.drainMessages()
			progressed = true
		}
		if !progressed {
			break
		}
	}
	if f.terminated {
		return
	}

	f.assertInvariants()

	// A deferred tamper that nobody tripped over is a stall, not an
	// acceptance: Mallory need not sign the corrupted update, and the
	// victim may be left waiting for a commitment with it pending. The
	// safety oracle still holds: whatever both sides committed must be the
	// same state. Liveness does not, so stop before those checks.
	if f.tampered() {
		f.t.Logf("%v left pending, not committed", f.tamper)
		aliceState := f.alice.channel.State()
		bobState := f.bob.channel.State()
		assertMirrored(f.t, "alice local/bob remote",
			&aliceState.LocalCommitment, &bobState.RemoteCommitment)
		assertMirrored(f.t, "bob local/alice remote",
			&bobState.LocalCommitment, &aliceState.RemoteCommitment)

		return
	}

	for _, side := range []struct {
		name string
		link *channelLink
	}{{"alice", f.aliceLink}, {"bob", f.bobLink}} {
		require.Falsef(f.t, side.link.channel.OweCommitment(),
			"%s still owes a commitment after %d flush rounds",
			side.name, maxFlushRounds)
		require.Falsef(f.t, side.link.channel.NeedCommitment(),
			"%s still needs a commitment after %d flush rounds",
			side.name, maxFlushRounds)

		_, err := side.link.channel.State().RemoteCommitChainTip()
		require.ErrorIsf(f.t, err, channeldb.ErrNoPendingCommit,
			"%s has an unrevoked remote commitment", side.name)
	}

	// Every settle has been committed on both sides, so nothing may be
	// left pending.
	require.Empty(f.t, f.aliceSettlesPending, "alice settles never landed")
	require.Empty(f.t, f.bobSettlesPending, "bob settles never landed")
	require.Empty(f.t, f.aliceFailsPending, "alice fails never landed")
	require.Empty(f.t, f.bobFailsPending, "bob fails never landed")

	aliceState := f.alice.channel.State()
	bobState := f.bob.channel.State()
	assertMirrored(f.t, "alice local/bob remote",
		&aliceState.LocalCommitment, &bobState.RemoteCommitment)
	assertMirrored(f.t, "bob local/alice remote",
		&bobState.LocalCommitment, &aliceState.RemoteCommitment)

	// Every HTLC still on the commitment must be one the harness tracks
	// as unresolved: incoming HTLCs are waiting on our own settle/fail,
	// outgoing ones on the peer's. Anything else leaked past a fail or a
	// bad-onion bounce.
	assertTracked := func(name string, c *channeldb.ChannelCommitment,
		incoming, outgoing map[uint64]lntypes.Preimage) {

		for _, h := range c.Htlcs {
			tracked := outgoing
			if h.Incoming {
				tracked = incoming
			}
			_, ok := tracked[h.HtlcIndex]
			require.Truef(f.t, ok, "%s: untracked HTLC left on "+
				"commitment: index=%d incoming=%v amt=%v",
				name, h.HtlcIndex, h.Incoming, h.Amt)
		}
	}
	assertTracked("alice", &aliceState.LocalCommitment,
		f.alicePreimages, f.bobPreimages)
	assertTracked("bob", &bobState.LocalCommitment,
		f.bobPreimages, f.alicePreimages)
}

// assertMirrored checks that owner's local commitment and the counterparty's
// copy of it (its remote commitment) describe the same transaction: same
// height, fee, swapped balances and the same HTLC set seen from the other
// direction.
func assertMirrored(t *testing.T, name string, local,
	remote *channeldb.ChannelCommitment) {

	t.Helper()

	require.Equalf(t, local.CommitHeight, remote.CommitHeight,
		"%s: commit height", name)
	require.Equalf(t, local.FeePerKw, remote.FeePerKw, "%s: fee rate", name)
	require.Equalf(t, local.CommitFee, remote.CommitFee,
		"%s: commit fee", name)
	require.Equalf(t, local.LocalBalance, remote.RemoteBalance,
		"%s: owner balance", name)
	require.Equalf(t, local.RemoteBalance, remote.LocalBalance,
		"%s: counterparty balance", name)
	require.Equalf(t, local.CommitTx.TxHash(), remote.CommitTx.TxHash(),
		"%s: commitment txid", name)

	type htlcKey struct {
		index    uint64
		incoming bool
		amt      lnwire.MilliSatoshi
		rHash    [32]byte
	}
	localSet := make(map[htlcKey]struct{}, len(local.Htlcs))
	for _, h := range local.Htlcs {
		localSet[htlcKey{h.HtlcIndex, h.Incoming, h.Amt, h.RHash}] =
			struct{}{}
	}
	remoteSet := make(map[htlcKey]struct{}, len(remote.Htlcs))
	for _, h := range remote.Htlcs {
		// The counterparty sees every HTLC from the other direction.
		remoteSet[htlcKey{h.HtlcIndex, !h.Incoming, h.Amt, h.RHash}] =
			struct{}{}
	}
	require.Equalf(t, localSet, remoteSet, "%s: HTLC set", name)
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
	case *lnwire.Stfu:
		return fmt.Sprintf("Stfu(initiator=%v)", m.Initiator)
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

// drainMessages delivers queued messages until every outbox that is not held
// is empty. It alternates directions one message at a time, so the order is
// deterministic (a select over both outboxes would pick at random whenever
// both hold messages, breaking fuzz reproducibility) and replies interleave
// the way they would over a live connection.
func (f *fuzzFSM) drainMessages() {
	for {
		deliveredAlice := f.aliceHold == 0 && f.deliverOne(true)
		deliveredBob := f.bobHold == 0 && f.deliverOne(false)
		if !deliveredAlice && !deliveredBob {
			return
		}
	}
}

// deliverOne hands the oldest message in one side's outbox to the other
// side's link, through Mallory's tamper if one is armed on that direction:
// fromAlice selects Alice→Bob, otherwise Bob→Alice. It returns false when the
// outbox is empty or the run has terminated.
func (f *fuzzFSM) deliverOne(fromAlice bool) bool {
	if f.terminated {
		return false
	}

	outbox, from, toName := f.alicePeer, "Alice", "Bob"
	if !fromAlice {
		outbox, from, toName = f.bobPeer, "Bob", "Alice"
	}

	var msg lnwire.Message
	select {
	case msg = <-outbox.sentMsgs:
	default:
		return false
	}

	msgs := []lnwire.Message{msg}
	if t := f.tamper; t != nil && !t.applied && t.fromAlice == fromAlice {
		if tampered, hit := t.tamperMsg(msg); hit {
			t.applied = true
			t.hit = msg.MsgType()
			msgs = tampered
			if t.immediate(f.malloryIsInitiator()) {
				t.mustReject = tampered[len(tampered)-1]
			}
			f.t.Logf("MALLORY applied %v", t)
		}
	}

	for _, m := range msgs {
		f.t.Logf("%s→%s: %v", from, toName, htlcMsgStr(m))
		if !f.deliver(!fromAlice, m) {
			return false
		}

		// The victim handled a message that is invalid on its own
		// without failing the link: it accepted corrupted input.
		if f.tamper != nil && m == f.tamper.mustReject {
			f.t.Fatalf("%s accepted the corrupted message of %v "+
				"without failing the link", toName, f.tamper)
		}
	}

	return true
}

// deliver hands msg to Alice's (toAlice) or Bob's link and checks whether the
// link failed. A failure ends the run cleanly when it is a known protocol
// boundary or, after Mallory struck, a reaction the tamper can legitimately
// cause; any other failure fails the test. It returns false once the run has
// terminated.
func (f *fuzzFSM) deliver(toAlice bool, msg lnwire.Message) bool {
	to, name, failure := f.bobLink, "Bob", &f.failures.bob
	if toAlice {
		to, name, failure = f.aliceLink, "Alice", &f.failures.alice
	}

	to.handleUpstreamMsg(f.t.Context(), msg)
	if !to.failed {
		return true
	}

	// Known protocol boundaries (reserve, fee exposure, a mutated
	// reestablish, ...) stay legitimate whether or not Mallory struck, so
	// they are checked before the tamper's own codes.
	reason := to.failReason
	add, isAdd := msg.(*lnwire.UpdateAddHTLC)
	switch {
	// lnd fails the link on an incoming add that pushes it over its max
	// fee exposure, a documented consequence of a tight limit. It is only
	// legitimate if the add really does exceed the limit, though: see
	// exactlyOverexposed for the stale dust lnd counts on top.
	case isAdd && strings.Contains(reason, feeExposureFailure):
		exact, over := f.exactlyOverexposed(to, add)
		if !over {
			f.t.Fatalf("%s's link failed on an add its exact fee "+
				"exposure allows (%v): %v", name, exact, reason)
		}
		f.t.Logf("%s's link correctly terminated: add exceeds max "+
			"fee exposure (%v)", name, exact)

	case isExpectedLinkFailure(reason):
		f.t.Logf("%s's link correctly terminated (expected protocol "+
			"boundary) after %v: %v", name, htlcMsgStr(msg), reason)

	case f.tampered():
		require.NotNilf(f.t, *failure, "%s's link failed without "+
			"reporting a failure code: %v", name, reason)
		code := (*failure).code
		if !f.tamper.expects(code) {
			f.t.Fatalf("%s's link failed with %v after %v, which "+
				"that tamper cannot cause: %v", name, code,
				f.tamper, reason)
		}
		f.t.Logf("%s's link detected %v (%v): %v", name, f.tamper,
			code, reason)

	default:
		f.t.Fatalf("%s's link failed unexpectedly after handling %v: "+
			"%v", name, htlcMsgStr(msg), reason)
	}
	f.terminated = true

	return false
}

// malloryIsInitiator reports whether the armed tamper's Mallory is the
// channel initiator, the only side allowed to send update_fee.
func (f *fuzzFSM) malloryIsInitiator() bool {
	mallory := f.bobLink
	if f.tamper.fromAlice {
		mallory = f.aliceLink
	}

	return mallory.channel.IsInitiator()
}

// feeExposureFailure is the reason lnd fails the link with when a remote add
// pushes it over its max fee exposure (processRemoteUpdateAddHTLC).
const feeExposureFailure = "peer sent us an HTLC that exceeded our max fee " +
	"exposure"

// exactlyOverexposed reports whether adding the incoming htlc to link's
// channel really exceeds its max fee exposure, and describes the sums. It
// mirrors channelLink.isOverexposedWithHtlc with one difference: the dust
// sums leave out HTLCs already removed from the commitment in question.
//
// GetDustSum sums every add still in the update logs, and a log is only
// compacted when a revocation is received. The side that resolves an HTLC
// receives its last revocation of that round before its own commitment drops
// the HTLC, so the stale add stays in its logs, and counts as dust, until the
// next round. That over-count is a deliberate simplification (7d16e58b5c),
// but once the link fails on it, an honest peer whose accurate check passed
// loses the channel. The subtracted adds are those the aux view still holds
// with a non-zero add height on the commitment chain, yet absent from every
// commitment of that chain that can still be broadcast: the local one, or
// the remote's current one and its pending successor (until the remote
// revokes, either may hit the chain). Adds not yet committed stay counted.
func (f *fuzzFSM) exactlyOverexposed(link *channelLink,
	htlc *lnwire.UpdateAddHTLC) (string, bool) {

	state := link.channel.State()
	localLive := []*channeldb.ChannelCommitment{&state.LocalCommitment}
	remoteLive := []*channeldb.ChannelCommitment{&state.RemoteCommitment}
	if diff, err := state.RemoteCommitChainTip(); err == nil {
		remoteLive = append(remoteLive, &diff.Commitment)
	}

	view := link.channel.FetchLatestAuxHTLCView()
	exactDust := func(whoseCommit lntypes.ChannelParty,
		live []*channeldb.ChannelCommitment) lnwire.MilliSatoshi {

		// The same parameters GetDustSum evaluates dust with.
		dustLimit := state.LocalChanCfg.DustLimit
		feeRate := chainfee.SatPerKWeight(
			state.LocalCommitment.FeePerKw,
		)
		if whoseCommit.IsRemote() {
			dustLimit = state.RemoteChanCfg.DustLimit
			feeRate = chainfee.SatPerKWeight(
				state.RemoteCommitment.FeePerKw,
			)
		}

		type key struct {
			index    uint64
			incoming bool
		}
		present := make(map[key]struct{})
		for _, c := range live {
			for _, h := range c.Htlcs {
				present[key{h.HtlcIndex, h.Incoming}] =
					struct{}{}
			}
		}

		sum := link.channel.GetDustSum(
			whoseCommit, fn.None[chainfee.SatPerKWeight](),
		)
		stale := func(adds []lnwallet.AuxHtlcDescriptor,
			incoming bool) {

			for _, a := range adds {
				if !a.IsAdd() || a.AddHeight(whoseCommit) == 0 {
					continue
				}
				k := key{a.HtlcIndex, incoming}
				if _, ok := present[k]; ok {
					continue
				}
				if lnwallet.HtlcIsDust(
					state.ChanType, incoming, whoseCommit,
					feeRate, a.Amount.ToSatoshis(),
					dustLimit,
				) {
					sum -= a.Amount
				}
			}
		}
		stale(view.Updates.Local, false)
		stale(view.Updates.Remote, true)

		return sum
	}

	// From here on, isOverexposedWithHtlc(htlc, true) verbatim.
	dustClosure := link.getDustClosure()
	feeRate := link.channel.WorstCaseFeeRate()
	amount := htlc.Amount.ToSatoshis()
	commitFee := max(link.getCommitFee(false), link.getCommitFee(true))
	additional := lnwire.NewMSatFromSatoshis(
		feeRate.FeeForWeight(lntypes.WeightUnit(input.HTLCWeight)),
	)

	exposure := func(whoseCommit lntypes.ChannelParty,
		live []*channeldb.ChannelCommitment) lnwire.MilliSatoshi {

		sum := exactDust(whoseCommit, live) +
			lnwire.NewMSatFromSatoshis(commitFee)
		if dustClosure(feeRate, true, whoseCommit, amount) {
			return sum + htlc.Amount
		}

		return sum + additional
	}

	local := exposure(lntypes.Local, localLive)
	remote := exposure(lntypes.Remote, remoteLive)
	limit := link.cfg.MaxFeeExposure
	desc := fmt.Sprintf("exact exposure local=%v remote=%v, lnd's "+
		"dust sums local=%v remote=%v, limit=%v", local, remote,
		link.getDustSum(lntypes.Local,
			fn.None[chainfee.SatPerKWeight]()),
		link.getDustSum(lntypes.Remote,
			fn.None[chainfee.SatPerKWeight]()), limit)

	return desc, local > limit || remote > limit
}

// tampered reports whether Mallory has corrupted a message in this run.
func (f *fuzzFSM) tampered() bool {
	return f.tamper != nil && f.tamper.applied
}

// armTamper arms a tamper read from the argument stream on the direction
// leaving Alice (fromAlice) or Bob. A run carries at most one applied tamper;
// arming again before it hits replaces the pending one. The inject kind needs
// no message to hit and is applied immediately.
func (f *fuzzFSM) armTamper(fromAlice bool) {
	kind := tamperKind(f.in.u8() % uint8(numTamperKinds))
	param := f.in.u8()
	if f.tampered() {
		return
	}

	f.tamper = &tamper{kind: kind, param: param, fromAlice: fromAlice}
	if kind != tamperInjectFee {
		f.t.Logf("MALLORY armed %v", f.tamper)
		return
	}

	mallory, peer := f.bobLink, f.bobPeer
	if fromAlice {
		mallory, peer = f.aliceLink, f.alicePeer
	}
	fee := &lnwire.UpdateFee{
		ChanID: mallory.ChanID(),
		FeePerKw: distinctFeeRate(
			uint32(chainfee.FeePerKwFloor)+100*uint32(param),
			uint32(mallory.channel.CommitFeeRate()),
		),
	}
	require.NoError(f.t, peer.SendMessage(false, fee))
	f.tamper.applied = true
	f.tamper.hit = fee.MsgType()
	if f.tamper.immediate(mallory.channel.IsInitiator()) {
		f.tamper.mustReject = fee
	}
	f.t.Logf("MALLORY applied %v", f.tamper)
}

// step applies one event, delivers the messages it produced and counts down
// any outbox holds.
func (f *fuzzFSM) step(e Event) {
	// A cut armed by an earlier event applies to this event's exchange.
	cut := f.cutAfter > 0
	f.applyEvent(e)
	if cut {
		f.cutConnection()
	}
	f.drainMessages()

	if f.aliceHold > 0 {
		f.aliceHold--
	}
	if f.bobHold > 0 {
		f.bobHold--
	}
}

// pickHTLCID selects an HTLC ID from preimages using the next argument byte as
// an index, giving the fuzzer control over which pending HTLC gets resolved.
// The IDs are sorted first so the choice is independent of map order.
func (f *fuzzFSM) pickHTLCID(preimages map[uint64]lntypes.Preimage) uint64 {
	ids := make([]uint64, 0, len(preimages))
	for id := range preimages {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	return ids[uint64(f.in.u8())%uint64(len(ids))]
}

// pickInvalidSettleTarget returns the HTLC ID an invalid settlement aims at:
// a pending HTLC from preimages when the next argument byte is odd and one
// exists, otherwise an ID the channel never allocates.
func (f *fuzzFSM) pickInvalidSettleTarget(
	preimages map[uint64]lntypes.Preimage) uint64 {

	if f.in.u8()%2 == 0 || len(preimages) == 0 {
		return nonExistentHTLCID
	}

	return f.pickHTLCID(preimages)
}

// pickHTLCAmount reads an HTLC amount for sender from the argument stream.
// The first byte picks a range so that a single run can mix HTLCs on either
// side of every threshold the commitment logic cares about:
//   - 0: the run's default, htlcRef modulo the channel capacity.
//   - 1: [0, 4096) sat plus msat noise. The dust limits are 200 and 800 sat
//     and the second-level HTLC fee adds a few hundred sat on top, so this
//     straddles the trimmed/untrimmed boundary on both commitments.
//   - 2: a boundary value relative to the sender's current bandwidth
//     (0, 1 msat, bandwidth-1, bandwidth, bandwidth+1, capacity).
//   - 3: an arbitrary amount up to twice the capacity.
func (f *fuzzFSM) pickHTLCAmount(sender *channelLink) lnwire.MilliSatoshi {
	capacity := lnwire.NewMSatFromSatoshis(sender.channel.Capacity)

	switch f.in.u8() % 4 {
	case 1:
		sat := lnwire.MilliSatoshi(f.in.u16() % 4096)
		msat := lnwire.MilliSatoshi(f.in.u16() % 1000)

		return sat*1000 + msat

	case 2:
		bandwidth := sender.Bandwidth()
		boundaries := []lnwire.MilliSatoshi{
			0, 1, bandwidth, bandwidth + 1, capacity,
		}
		if bandwidth > 0 {
			boundaries = append(boundaries, bandwidth-1)
		}

		return boundaries[int(f.in.u8())%len(boundaries)]

	case 3:
		return lnwire.MilliSatoshi(f.in.u64() % uint64(2*capacity))

	default:
		return lnwire.MilliSatoshi(f.htlcRef) % capacity
	}
}

// pickFeeRate reads the network fee rate link's estimator reports from the
// argument stream. The link clamps it before proposing a commitment fee:
//   - 0: the run's default, scaled by the number of active HTLCs.
//   - 1: [0, 1000) sat/kw, straddling the 253 sat/kw relay floor.
//   - 2: [253, 65788) sat/kw, the realistic range.
//   - 3: an arbitrary 32-bit rate, as large as update_fee can carry.
func (f *fuzzFSM) pickFeeRate(link *channelLink) chainfee.SatPerKWeight {
	switch f.in.u8() % 4 {
	case 1:
		return chainfee.SatPerKWeight(f.in.u16() % 1000)

	case 2:
		return chainfee.FeePerKwFloor +
			chainfee.SatPerKWeight(f.in.u16())

	case 3:
		return chainfee.SatPerKWeight(f.in.u32())

	default:
		numHtlcs := uint64(len(link.channel.ActiveHtlcs()))

		return chainfee.SatPerKWeight((numHtlcs+f.htlcRef)*100 + 1000)
	}
}

// derivePreimageSeed builds a collision-free preimage seed for the given
// party (0 = Alice, 1 = Bob) and attempt counter. Hashing a party-tagged
// domain guarantees Alice and Bob can never derive the same preimage, while
// htlcRef varies the preimages across fuzz runs.
func (f *fuzzFSM) derivePreimageSeed(party byte, attempt uint64) uint64 {
	var b [17]byte
	b[0] = party
	binary.BigEndian.PutUint64(b[1:9], f.htlcRef)
	binary.BigEndian.PutUint64(b[9:17], attempt)
	h := sha256.Sum256(b[:])

	return binary.BigEndian.Uint64(h[:8])
}

// sendHTLC initiates an outgoing HTLC from sender by registering a hodl
// invoice on the receiver's registry, committing the payment circuit on the
// sender's Switch, and injecting the UpdateAddHTLC directly into the link via
// handleDownstreamUpdateAdd. circuitID keys the payment circuit. Returns the
// HTLC index the channel assigned, the preimage and true on success, or false
// if the channel is full (circuit and invoice are cleaned up in that case).
func (f *fuzzFSM) sendHTLC(sender *channelLink, circuitID uint64) (uint64,
	lntypes.Preimage, bool) {

	var senderCircuits *mockCircuitMap
	var invoiceRegistry *mockInvoiceRegistry
	// preimageSeed derives a unique preimage per attempt via an explicit
	// domain hash: a party tag (so Alice and Bob can never share a
	// preimage), htlcRef (varies preimages across fuzz runs), and the
	// per-party attempt counter (uniqueness within a run).
	var preimageSeed uint64
	switch sender {
	case f.aliceLink:
		senderCircuits = f.aliceCircuits
		invoiceRegistry = f.bobRegistry
		preimageSeed = f.derivePreimageSeed(0, f.aliceHTLCAttempts)
		f.aliceHTLCAttempts++
	case f.bobLink:
		senderCircuits = f.bobCircuits
		invoiceRegistry = f.aliceRegistry
		preimageSeed = f.derivePreimageSeed(1, f.bobHTLCAttempts)
		f.bobHTLCAttempts++
	default:
		f.t.Fatal("HTLC sender does not exist")
	}

	htlcAmt := f.pickHTLCAmount(sender)

	// HTLC preimage is derived from the htlcRef.
	htlc, preimage, err := generateSingleHopHtlc(
		f.t, circuitID, htlcAmt, preimageSeed,
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
		incomingHTLCID: circuitID,
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

		return 0, lntypes.Preimage{}, false
	}

	// handleDownstreamUpdateAdd stamped the add with the channel's index.
	return htlc.ID, preimage, true
}

// sendBadOnionHTLC sends an HTLC from sender after arming the receiver's
// onion decoder with a one-shot failure. The failure mode is read from the
// argument stream so the fuzz corpus drives which branch of processRemoteAdds
// is exercised:
//   - arg%3 == 0 → onionFailDecode  (DecodeHopIterator returns failcode)
//   - arg%3 == 1 → onionFailPayload (HopPayload returns ErrInvalidPayload)
//   - arg%3 == 2 → onionFailExtract (ExtractErrorEncrypter returns failcode)
//
// The HTLC is failed-back by the receiver rather than settled, so the preimage
// is not tracked in alice/bobPreimages.
func (f *fuzzFSM) sendBadOnionHTLC(sender *channelLink,
	receiverDec *mockIteratorDecoder) {

	var (
		circuitSeq *uint64
		who        string
	)
	switch sender {
	case f.aliceLink:
		circuitSeq = &f.aliceCircuitSeq
		who = "Alice"
	case f.bobLink:
		circuitSeq = &f.bobCircuitSeq
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

	mode := onionFailMode(f.in.u8()%3) + 1

	htlcID, preimage, ok := f.sendHTLC(sender, *circuitSeq)
	if !ok {
		f.t.Logf("%s Bad Onion Skipped: channel full", who)

		return
	}
	*circuitSeq++

	// The receiver only decodes the onion once the add is locked in, so
	// arming the decoder after the add is still in time.
	receiverDec.setOnionFailMode(preimage.Hash(), mode)
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
		// Once Mallory has corrupted the protocol, a peer refusing to
		// sign the state it was fed is a correct reaction.
		if f.tampered() {
			f.t.Logf("sendCommitSig refused after %v: %v", f.tamper,
				err)
			f.terminated = true

			return 0, false
		}
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
// real link fail path. The fail variant is read from the argument stream so
// the fuzz corpus drives the choice:
//   - arg%2 == 0 → channelLink.sendHTLCError (regular UpdateFailHTLC with a
//     TemporaryChannelFailure obfuscated by the mock encrypter).
//   - arg%2 == 1 → channelLink.sendMalformedHTLCError
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

	if f.in.u8()%2 == 0 {
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

// pickRelayFee reads the minimum relay fee link's estimator reports from the
// argument stream. Real estimators never report less than the floor, but a
// congested mempool raises it, and a relay fee above what the initiator can
// afford drives IdealCommitFeeRate into its absoluteMaxFee fallback:
//   - 0: the floor, 253 sat/kw.
//   - 1: floor + [0, 65536) sat/kw, a congested mempool.
//   - 2: floor + an arbitrary 32-bit rate.
func (f *fuzzFSM) pickRelayFee() chainfee.SatPerKWeight {
	switch f.in.u8() % 3 {
	case 1:
		return chainfee.FeePerKwFloor +
			chainfee.SatPerKWeight(f.in.u16())

	case 2:
		return chainfee.FeePerKwFloor +
			chainfee.SatPerKWeight(f.in.u32())

	default:
		return chainfee.FeePerKwFloor
	}
}

// updateFee sets the network and relay fees the link samples to rates read
// from the argument stream and runs the link's periodic fee update. As in
// production, only the initiator acts, and the rate is clamped by
// IdealCommitFeeRate and filtered by shouldAdjustCommitFee before any
// update_fee is sent.
func (f *fuzzFSM) updateFee(link *channelLink) error {
	f.feeEstimator.feeRate = f.pickFeeRate(link)
	f.feeEstimator.relayFee = f.pickRelayFee()

	// After STFU is sent the link must not emit any more update messages;
	// the receiving side would call stfuFailf and fail the link.
	if !link.quiescer.CanSendUpdates() {
		return fmt.Errorf("quiescence pending")
	}

	return link.handleUpdateFee(f.t.Context())
}

// sendWarning emits an lnwire.Warning from sender's peer so that drainMessages
// delivers it to the other side's link. The payload alternates between
// printable ASCII and a binary blob (driven by the argument stream) to cover
// both branches of Warning.Warning().
func (f *fuzzFSM) sendWarning(sender *channelLink) {
	data := []byte(fmt.Sprintf("fuzz warning ref=%d", f.htlcRef))
	// Append a binary blob to cover the Warning.Warning() branch that
	// returns the raw data instead of a string.
	if f.in.u8()%2 == 0 {
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

// pickSyncHeight reads the NextLocalCommitHeight to put in the remote's
// ChannelReestablish on a restart, given the canonical value:
//   - 0, 1: canonical (half of all restarts are honest).
//   - 2: canonical shifted by a delta in [-3, +3], which lands on the
//     off-by-one boundaries of syncChanStates (retransmit vs. data loss vs.
//     cannot sync).
//   - 3: an arbitrary 64-bit height.
func (f *fuzzFSM) pickSyncHeight(canonical uint64) uint64 {
	switch f.in.u8() % 4 {
	case 2:
		delta := int64(f.in.u8()%7) - 3
		if delta < 0 && uint64(-delta) > canonical {
			return 0
		}

		return uint64(int64(canonical) + delta)

	case 3:
		return f.in.u64()

	default:
		return canonical
	}
}

// renewDecoder replaces one side's onion decoder with a fresh one, as a
// restart clears the Sphinx replay cache: resolveFwdPkgs then re-decodes onion
// blobs from scratch instead of hitting iterator entries consumed before. The
// bad onions stay bad though, so the fail modes carry over, and later
// bad-onion events arm the decoder the new link uses.
func (f *fuzzFSM) renewDecoder(isAlice bool) *mockIteratorDecoder {
	oldDecoder := &f.bobDecoder
	if isAlice {
		oldDecoder = &f.aliceDecoder
	}
	fresh := newMockIteratorDecoder()
	(*oldDecoder).mu.RLock()
	maps.Copy(fresh.onionFailModes, (*oldDecoder).onionFailModes)
	(*oldDecoder).mu.RUnlock()
	*oldDecoder = fresh

	return fresh
}

// reconnect models a real disconnect followed by a reconnect of both peers,
// which restartLink cannot: messages in flight are lost in both directions,
// and both sides reload their channel state from disk, which forgets every
// update not yet covered by a commitment signature. Both then exchange
// channel_reestablish and retransmit what the other is missing.
//
// Two argument bytes pick how many of the messages queued Alice→Bob and
// Bob→Alice still arrive before the connection drops; the rest are lost.
func (f *fuzzFSM) reconnect() {
	aliceArrive := int(f.in.u8())
	bobArrive := int(f.in.u8())

	// Holds belong to the old connection.
	f.aliceHold, f.bobHold = 0, 0

	// Deliver the prefix of each outbox that makes it across, then drop
	// the rest. Replies the deliveries provoke join the outboxes and are
	// lost with them unless the prefix covers them.
	aliceArrive %= len(f.alicePeer.sentMsgs) + 1
	bobArrive %= len(f.bobPeer.sentMsgs) + 1
	for aliceArrive > 0 || bobArrive > 0 {
		if aliceArrive > 0 {
			f.deliverOne(true)
			aliceArrive--
		}
		if bobArrive > 0 {
			f.deliverOne(false)
			bobArrive--
		}
		if f.terminated {
			return
		}
	}
	f.disconnectAndReestablish()
}

// cutConnection delivers the first cutAfter messages of the exchange the last
// event started, alternating directions and ignoring holds, then drops the
// connection with the rest in flight. This puts the cut in the middle of a
// commitment dance, where a reconnect has the most to recover.
func (f *fuzzFSM) cutConnection() {
	n := f.cutAfter
	f.cutAfter = 0
	f.aliceHold, f.bobHold = 0, 0

	for n > 0 {
		deliveredAlice := f.deliverOne(true)
		if deliveredAlice {
			n--
		}
		deliveredBob := n > 0 && f.deliverOne(false)
		if deliveredBob {
			n--
		}
		if f.terminated || !deliveredAlice && !deliveredBob {
			break
		}
	}
	if f.terminated {
		return
	}

	f.disconnectAndReestablish()
}

// disconnectAndReestablish drops every message still in flight, reloads both
// channels from disk and resyncs both links over a new connection.
func (f *fuzzFSM) disconnectAndReestablish() {
	lost := len(f.alicePeer.sentMsgs) + len(f.bobPeer.sentMsgs)
	for _, peer := range []*mockPeer{f.alicePeer, f.bobPeer} {
		for len(peer.sentMsgs) > 0 {
			<-peer.sentMsgs
		}
	}

	f.aliceLink.Stop()
	f.bobLink.Stop()

	// Reload both channels from disk, as the peer does for a new
	// connection.
	for _, c := range []*testLightningChannel{f.alice, f.bob} {
		reloaded, err := c.restore()
		require.NoError(f.t, err)
		c.channel = reloaded
	}

	aliceLink, aliceUpstream := f.hopNet.newFuzzLink(
		f.t, f.alicePeer, f.alice.channel, f.renewDecoder(true),
		f.aliceRegistry, f.alicePCache, f.aliceCircuits, f.bestHeight,
		f.maxFeeExposure, f.maxFeeAllocation, f.feeEstimator,
		f.failures.recorder(true),
	)
	bobLink, bobUpstream := f.hopNet.newFuzzLink(
		f.t, f.bobPeer, f.bob.channel, f.renewDecoder(false),
		f.bobRegistry, f.bobPCache, f.bobCircuits, f.bestHeight,
		f.maxFeeExposure, f.maxFeeAllocation, f.feeEstimator,
		f.failures.recorder(false),
	)
	f.aliceLink, f.bobLink = aliceLink, bobLink

	aliceSync, err := f.alice.channel.State().ChanSyncMsg()
	require.NoError(f.t, err)
	bobSync, err := f.bob.channel.State().ChanSyncMsg()
	require.NoError(f.t, err)
	aliceUpstream <- bobSync
	bobUpstream <- aliceSync

	for _, link := range []*channelLink{aliceLink, bobLink} {
		err := link.resumeLink(f.t.Context())
		if err == nil {
			continue
		}

		// Honest peers that reload their own state must always
		// resync. After Mallory struck, a refusal is a reaction.
		if !f.tampered() {
			f.t.Fatalf("resync after reconnect failed: %v", err)
		}
		f.t.Logf("resync after reconnect refused after %v: %v",
			f.tamper, err)
		f.terminated = true

		return
	}

	f.t.Logf("EV Reconnect: %d in-flight messages lost", lost)
	f.drainMessages()
	if f.terminated {
		return
	}

	f.reconcileAfterReconnect()
}

// reconcileAfterReconnect brings the harness bookkeeping back in line with
// what survived the reload. An add, settle or fail that never got under a
// commitment signature is forgotten by both sides:
//   - a lost settle undoes its shadow balance credit and the HTLC is
//     tracked again, so it can be settled anew (as the invoice registry
//     would on a real node);
//   - a lost fail tracks the HTLC again;
//   - a lost add leaves an entry no channel knows, which is dropped (the
//     channel may reuse its index for the next add).
func (f *fuzzFSM) reconcileAfterReconnect() {
	type side struct {
		link     *channelLink
		tracked  map[uint64]lntypes.Preimage
		settles  map[uint64]resolvedHTLC
		fails    map[uint64]resolvedHTLC
		credit   *lnwire.MilliSatoshi
		debit    *lnwire.MilliSatoshi
		settlers string
	}
	for _, x := range []side{
		{f.aliceLink, f.alicePreimages, f.aliceSettlesPending,
			f.aliceFailsPending, &f.expectedAliceMSat,
			&f.expectedBobMSat, "Alice"},
		{f.bobLink, f.bobPreimages, f.bobSettlesPending,
			f.bobFailsPending, &f.expectedBobMSat,
			&f.expectedAliceMSat, "Bob"},
	} {
		// Incoming HTLCs still on any commitment of this side.
		state := x.link.channel.State()
		live := []*channeldb.ChannelCommitment{
			&state.LocalCommitment, &state.RemoteCommitment,
		}
		if diff, err := state.RemoteCommitChainTip(); err == nil {
			live = append(live, &diff.Commitment)
		}
		onCommit := make(map[uint64]struct{})
		for _, c := range live {
			for _, h := range c.Htlcs {
				if h.Incoming {
					onCommit[h.HtlcIndex] = struct{}{}
				}
			}
		}

		// Settles and fails of incoming HTLCs this side still holds in
		// its log, i.e. that survived the reload.
		resolving := make(map[uint64]struct{})
		view := x.link.channel.FetchLatestAuxHTLCView()
		for _, u := range view.Updates.Local {
			switch u.EntryType {
			case lnwallet.Settle, lnwallet.Fail,
				lnwallet.MalformedFail:

				resolving[u.ParentIndex] = struct{}{}
			}
		}
		survived := func(id uint64) bool {
			_, still := onCommit[id]
			_, resolved := resolving[id]

			return !still || resolved
		}

		for id, r := range x.settles {
			if survived(id) {
				continue
			}
			*x.credit -= r.amt
			*x.debit += r.amt
			delete(x.settles, id)
			x.tracked[id] = r.preimage
			f.t.Logf("%s's settle of HTLC %d was lost",
				x.settlers, id)
		}
		for id, r := range x.fails {
			if survived(id) {
				continue
			}
			delete(x.fails, id)
			x.tracked[id] = r.preimage
			f.t.Logf("%s's fail of HTLC %d was lost",
				x.settlers, id)
		}
		for id := range x.tracked {
			if _, ok := onCommit[id]; !ok {
				delete(x.tracked, id)
				f.t.Logf("HTLC %d to %s was lost", id,
					x.settlers)
			}
		}
	}
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

	// Everything in flight in either direction lands before the
	// connection drops, and holds belong to the old connection. Losing
	// in-flight messages would need both sides to reload their channel
	// state and reestablish (the peer forgets the other side's uncommitted
	// updates on a real reconnect), which this one-sided restart over the
	// same LightningChannel does not model.
	f.aliceHold, f.bobHold = 0, 0
	f.drainMessages()
	if f.terminated {
		return
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

	// Optionally inject a mutated height so the fuzzer can reach
	// syncChanStates paths that are unreachable with canonical messages.
	canonicalHeight := remoteSyncMsg.NextLocalCommitHeight
	remoteSyncMsg.NextLocalCommitHeight = f.pickSyncHeight(canonicalHeight)
	mutated := remoteSyncMsg.NextLocalCommitHeight != canonicalHeight

	// A real restart clears the Sphinx replay cache. Use a fresh decoder so
	// resolveFwdPkgs can re-decode onion blobs from scratch instead of
	// hitting stale, already-consumed iterator entries from the prior run.
	// The bad onions stay bad though, so the fail modes carry over, and
	// later bad-onion events must arm the decoder the new link uses.
	freshDecoder := f.renewDecoder(isAlice)

	newLink, newUpstream := f.hopNet.newFuzzLink(
		f.t, peer, testChan.channel, freshDecoder,
		registry, pCache, circuits, f.bestHeight,
		f.maxFeeExposure, f.maxFeeAllocation, f.feeEstimator,
		f.failures.recorder(isAlice),
	)

	// A taproot channel's MuSig2 verification nonce is single-use and was
	// consumed by the previous session. A real reconnect reloads the
	// channel, and NewLightningChannel generates fresh nonces; this
	// restart keeps the channel, so do the same here. The nonces derive
	// deterministically from the revocation producer, so the remote,
	// which keeps its session, expects exactly these.
	if testChan.channel.State().ChanType.IsTaproot() {
		_, err := testChan.channel.GenMusigNonces()
		require.NoError(f.t, err)
	}

	// Pre-load the remote's reestablish so resumeLink can read it
	// synchronously from upstream.
	newUpstream <- remoteSyncMsg

	err = newLink.resumeLink(f.t.Context())
	if err != nil {
		if !mutated && !f.tampered() {
			// Canonical sync message — any error is a real bug.
			require.NoError(f.t, err)
		}

		// The link failed the channel, marking it borked or recording
		// data loss on disk as it went, so the channel is dead from
		// here on. Hand the Error a real peer sends straight to the
		// remote, where no queued delivery (a hold, a cut connection)
		// can lose it, and end the run.
		f.deliver(!isAlice, &lnwire.Error{
			Data: []byte(err.Error()),
		})
		f.terminated = true

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

// costOf returns a deterministic work weight approximating an event's real
// CPU cost. The dominant term is commit-sig generation, which re-signs every
// active HTLC, so commit-style events (and link restarts, which reload and
// re-derive HTLC scripts) are charged proportionally to the number of active
// HTLCs on the relevant side. This lets MaxWorkPerRun bound the worst-case
// (adds × commit rounds) blow-up that a flat per-event count cap cannot.
func (f *fuzzFSM) costOf(e Event) int {
	aliceHtlcs := len(f.aliceLink.channel.ActiveHtlcs())
	bobHtlcs := len(f.bobLink.channel.ActiveHtlcs())

	switch e {
	case EvAliceSendCommit:
		return 1 + aliceHtlcs
	case EvBobSendCommit:
		return 1 + bobHtlcs

	// CommitNoWindow sends two back-to-back commit sigs.
	case EvAliceSendCommitNoWindow:
		return 2 * (1 + aliceHtlcs)
	case EvBobSendCommitNoWindow:
		return 2 * (1 + bobHtlcs)

	// A reconnect reloads both channels and resyncs both links.
	case EvReconnect:
		return 1 + aliceHtlcs + bobHtlcs

	// A restart reloads the channel and regenerates HTLC scripts.
	case EvAliceRestartLink:
		return 1 + aliceHtlcs
	case EvBobRestartLink:
		return 1 + bobHtlcs

	// HTLC adds (including the bad-onion variants) inflate the HTLC count
	// every later commit must re-sign.
	case EvAliceSendAddHtlc, EvBobSendAddHtlc,
		EvAliceSendBadOnion, EvBobSendBadOnion:

		return 2

	default:
		return 1
	}
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
		id, preimage, ok := f.sendHTLC(f.aliceLink, f.aliceCircuitSeq)
		if !ok {
			f.t.Log("Alice Add HTLC Skipped: channel full")
			return
		}
		f.aliceCircuitSeq++
		// bobPreimages are those Bob keep track to settle the hold
		// invoices.
		f.bobPreimages[id] = preimage
		f.t.Logf("EV Alice Send Add HTLC ID:%v", id)
	case EvBobSendAddHtlc:
		if len(f.alicePreimages) >= maxInflightHtlcs {
			f.t.Logf("Bob Add HTLC Skipped: HTLCs pending > %v",
				maxInflightHtlcs)

			return
		}
		// Alice create the Hold Invoice, Bob send the HTLC.
		id, preimage, ok := f.sendHTLC(f.bobLink, f.bobCircuitSeq)
		if !ok {
			f.t.Log("Bob Add HTLC Skipped: channel full")
			return
		}
		f.bobCircuitSeq++
		// alicePreimages are those Alice keep track to settle the hold
		// invoices.
		f.alicePreimages[id] = preimage
		f.t.Logf("EV Bob Send Add HTLC ID:%v", id)
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
			f.aliceSettlesPending[chosenID] = resolvedHTLC{
				amt: amt, preimage: preimage,
			}
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
			f.bobSettlesPending[chosenID] = resolvedHTLC{
				amt: amt, preimage: preimage,
			}
			delete(f.bobPreimages, chosenID)
			f.t.Logf("EV Bob Settle HTLC ID:%v amt:%v",
				chosenID, amt)

			return
		}
		f.t.Log("Bob Settle HTLC Skipped")
	// Invalid settlement with a wrong preimage, targeting either an HTLC
	// ID the channel never allocated or (arg odd) a real pending HTLC.
	case EvAliceInvalidHtlcSettlement:
		htlcID := f.pickInvalidSettleTarget(f.alicePreimages)
		err := f.aliceLink.channel.SettleHTLC(
			lntypes.Preimage{0x01}, htlcID, nil, nil, nil,
		)
		require.Error(f.t, err)
		f.t.Logf("EV Alice Invalid HTLC Settlement: %v", err)
	case EvBobInvalidHtlcSettlement:
		htlcID := f.pickInvalidSettleTarget(f.bobPreimages)
		err := f.bobLink.channel.SettleHTLC(
			lntypes.Preimage{0x01}, htlcID, nil, nil, nil,
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
			f.aliceFailsPending[chosenID] = resolvedHTLC{
				preimage: f.alicePreimages[chosenID],
			}
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
			f.bobFailsPending[chosenID] = resolvedHTLC{
				preimage: f.bobPreimages[chosenID],
			}
			delete(f.bobPreimages, chosenID)
			f.t.Logf("EV Bob Fail HTLC ID: %v", chosenID)
			return
		}
		f.t.Log("Bob Fail HTLC Skipped")
	case EvAliceFailNonExistentHtlc:
		htlcID := nonExistentHTLCID
		reason := []byte("fuzz test")
		err := f.aliceLink.channel.FailHTLC(
			htlcID, reason, nil, nil, nil,
		)
		require.Error(f.t, err)
		f.t.Logf("EV Alice Invalid HTLC Failure: %v", err)
	case EvBobFailNonExistentHtlc:
		htlcID := nonExistentHTLCID
		reason := []byte("fuzz test")
		err := f.bobLink.channel.FailHTLC(htlcID, reason, nil, nil, nil)
		require.Error(f.t, err)
		f.t.Logf("EV Bob Invalid HTLC Failure: %v", err)
	case EvAliceSendUpdateFee:
		err := f.updateFee(f.aliceLink)
		if err == nil {
			f.t.Logf("EV Alice Update Fee (network fee %v, "+
				"relay fee %v)", f.feeEstimator.feeRate,
				f.feeEstimator.relayFee)

			return
		}
		f.t.Logf("Alice skipped Update Fee: %s", err)
	case EvBobSendUpdateFee:
		err := f.updateFee(f.bobLink)
		if err == nil {
			f.t.Logf("EV Bob Update Fee (network fee %v, "+
				"relay fee %v)", f.feeEstimator.feeRate,
				f.feeEstimator.relayFee)

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
	// Hold one side's outbox for a few events so the other side acts
	// without seeing it, e.g. both signing a commitment concurrently.
	// The hold covers the next 1..maxHoldEvents events; step also counts
	// down this event, hence the extra one.
	case EvAliceHoldOutbox:
		f.aliceHold = 2 + int(f.in.u8()%maxHoldEvents)
		f.t.Logf("EV Alice Hold Outbox for %d events", f.aliceHold-1)
	case EvBobHoldOutbox:
		f.bobHold = 2 + int(f.in.u8()%maxHoldEvents)
		f.t.Logf("EV Bob Hold Outbox for %d events", f.bobHold-1)
	// Deliver a single message even while its outbox is held, stepping
	// through one direction while the other keeps flowing.
	case EvDeliverAliceToBob:
		if !f.deliverOne(true) {
			f.t.Log("No Alice→Bob message to deliver")
		}
	case EvDeliverBobToAlice:
		if !f.deliverOne(false) {
			f.t.Log("No Bob→Alice message to deliver")
		}
	// Mallory corrupts the next matching message on her side of the
	// connection (see fuzz_link_mallory_test.go).
	case EvAliceTamper:
		f.armTamper(true)
	case EvBobTamper:
		f.armTamper(false)
	case EvReconnect:
		f.reconnect()
	// Arm a connection drop in the middle of whatever the next event
	// starts: only the first 1..8 messages of its exchange get through.
	case EvArmDisconnect:
		f.cutAfter = 1 + int(f.in.u8()%8)
		f.t.Logf("EV Arm Disconnect after %d messages", f.cutAfter)
	// Advance the chain tip by 1..8 blocks. Every HTLC the harness
	// sends expires at a fixed height, so this walks it towards and past
	// the receiver's final CLTV check: adds that arrive too late are
	// failed back by the exit hop.
	case EvMineBlocks:
		*f.height += 1 + uint32(f.in.u8()%8)
		f.t.Logf("EV Mine Blocks: height %d", *f.height)
	}
}

// taprootShape is a chanShape (see fuzzChanShape) for a production simple
// taproot channel with asymmetric parameters.
const taprootShape = uint32(0x5a5a5a5b | 1<<31)

// TestChannelLinkFSMScenarios runs deterministic event sequences through the
// fuzz harness to validate each event type before enabling the full fuzzer.
func TestChannelLinkFSMScenarios(t *testing.T) {
	// step is one scenario event together with the argument bytes it reads
	// (see fuzzInput). Events given no arguments take their defaults.
	type step struct {
		ev   Event
		args []byte
	}

	// runSteps applies each step, checking the invariants after every one
	// and the end-of-run convergence oracle afterwards, exactly like the
	// fuzz target. It returns the FSM for extra assertions.
	// runShapedSteps is runSteps on a channel of the given shape (see
	// fuzzChanShape).
	runShapedSteps := func(t *testing.T, chanShape uint32,
		steps []step) *fuzzFSM {

		t.Helper()

		f := newFuzzFSM(
			t, uint64(1_000_000), uint64(50), chanShape, uint64(0),
			uint64(0),
		)
		f.htlcRef = uint64(10_000_000)

		for _, s := range steps {
			f.in = &fuzzInput{data: s.args}
			f.step(s.ev)
			if f.terminated {
				return f
			}
			f.assertInvariants()
		}
		f.flushAndAssertConverged()

		return f
	}
	runSteps := func(t *testing.T, steps []step) *fuzzFSM {
		t.Helper()

		return runShapedSteps(t, 0, steps)
	}

	run := func(t *testing.T, events []Event) {
		t.Helper()

		steps := make([]step, len(events))
		for i, ev := range events {
			steps[i] = step{ev: ev}
		}
		runSteps(t, steps)
	}

	// runWithSyncHeight is like run but, on every restart event, replaces
	// NextLocalCommitHeight in the remote ChannelReestablish with an
	// arbitrary height (pickSyncHeight mode 3), so that syncChanStates
	// error paths are exercised deterministically.
	runWithSyncHeight := func(t *testing.T, syncHeight uint64,
		events []Event) {

		t.Helper()

		restartArgs := binary.BigEndian.AppendUint64(
			[]byte{3}, syncHeight,
		)
		steps := make([]step, len(events))
		for i, ev := range events {
			steps[i] = step{ev: ev}
			if ev == EvAliceRestartLink || ev == EvBobRestartLink {
				steps[i].args = restartArgs
			}
		}
		f := runSteps(t, steps)

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

	// Both sides sign concurrently: Alice's add and commit_sig are held
	// until Bob has added and signed, so the two commit_sigs cross on the
	// wire.
	t.Run("crossing_commit_sigs", func(t *testing.T) {
		runSteps(t, []step{
			{ev: EvAliceHoldOutbox, args: []byte{2}},
			{ev: EvAliceSendAddHtlc},
			{ev: EvAliceSendCommit},
			{ev: EvBobSendAddHtlc},
			{ev: EvBobSendCommit},
			{ev: EvAliceSettleHtlc},
			{ev: EvBobSettleHtlc},
			{ev: EvAliceSendCommit},
		})
	})

	// Alice's outbox is held and stepped through one message at a time
	// while Bob keeps answering and adds an HTLC of his own mid-dance.
	t.Run("step_through_held_outbox", func(t *testing.T) {
		runSteps(t, []step{
			{
				ev:   EvAliceHoldOutbox,
				args: []byte{maxHoldEvents - 1},
			},
			{ev: EvAliceSendAddHtlc},
			{ev: EvAliceSendCommit},
			{ev: EvDeliverAliceToBob},
			{ev: EvBobSendAddHtlc},
			{ev: EvDeliverAliceToBob},
			{ev: EvBobSendCommit},
			{ev: EvDeliverAliceToBob},
		})
	})

	// After the chain moves past what the HTLC expiry allows, Bob's exit
	// hop fails Alice's add back instead of holding it; the channel must
	// still converge.
	t.Run("add_expires_at_exit_hop", func(t *testing.T) {
		f := runSteps(t, []step{
			{ev: EvMineBlocks, args: []byte{7}},
			{ev: EvAliceSendAddHtlc},
			{ev: EvAliceSendCommit},
			{ev: EvBobSettleHtlc},
		})
		require.Empty(t, f.bob.channel.ActiveHtlcs())
	})

	// Every tamper kind, by either peer, over traffic that carries every
	// message type both sides send (adds, commits and revocations, a fee
	// update, fulfills and fails). The runner fails the test if the tamper
	// goes undetected or a peer fails with a code the tamper cannot cause.
	feeTo12000 := []byte{3, 0x00, 0x00, 0x2e, 0xe0, 0}
	malloryTraffic := []step{
		{ev: EvAliceSendAddHtlc},
		{ev: EvBobSendAddHtlc},
		{ev: EvAliceSendCommit},
		{ev: EvAliceSendUpdateFee, args: feeTo12000},
		{ev: EvAliceSettleHtlc},
		{ev: EvBobSettleHtlc},
		{ev: EvAliceSendCommit},
		{ev: EvAliceSendAddHtlc},
		{ev: EvBobSendAddHtlc},
		{ev: EvBobSendCommit},
		{ev: EvAliceFailHtlc},
		{ev: EvBobFailHtlc},
		{ev: EvBobSendCommit},
	}
	// The same on a taproot channel, where the commitment signature is a
	// MuSig2 partial signature checked by real cryptography.
	malloryShapes := []struct {
		prefix string
		shape  uint32
	}{{"", 0}, {"taproot_", taprootShape}}
	type malloryCase struct {
		name      string
		shape     uint32
		ev        Event
		kind      tamperKind
		param     byte
		fromAlice bool
	}
	var malloryCases []malloryCase
	addMalloryCase := func(prefix string, shape uint32, kind tamperKind,
		fromAlice bool, param byte) {

		ev, mallory := EvBobTamper, "bob"
		if fromAlice {
			ev, mallory = EvAliceTamper, "alice"
		}
		malloryCases = append(malloryCases, malloryCase{
			name: fmt.Sprintf("%smallory_%s_%v_%d", prefix,
				mallory, kind, param),
			shape:     shape,
			ev:        ev,
			kind:      kind,
			param:     param,
			fromAlice: fromAlice,
		})
	}
	for _, ms := range malloryShapes {
		for kind := tamperKind(0); kind < numTamperKinds; kind++ {
			for _, fromAlice := range []bool{true, false} {
				for _, param := range []byte{6, 7} {
					addMalloryCase(ms.prefix, ms.shape,
						kind, fromAlice, param)
				}
			}
		}
	}
	for _, c := range malloryCases {
		t.Run(c.name, func(t *testing.T) {
			steps := append([]step{{
				ev:   c.ev,
				args: []byte{byte(c.kind), c.param},
			}}, malloryTraffic...)
			f := runShapedSteps(t, c.shape, steps)

			// Only the initiator (Alice) sends update_fee, so
			// Bob's FeeRate tamper has nothing to hit.
			if c.kind == tamperFeeRate && !c.fromAlice {
				require.False(t, f.tampered())
				return
			}
			require.True(t, f.tampered(),
				"tamper never hit a message")
			require.True(t, f.terminated)
		})
	}

	// Reconnects that lose different parts of the protocol. Updates no
	// commitment signature covers are forgotten by both sides; the rest is
	// retransmitted after channel_reestablish. Every run must still
	// converge.
	t.Run("reconnect_lost_add", func(t *testing.T) {
		runSteps(t, []step{
			{ev: EvAliceSendAddHtlc},
			{ev: EvReconnect},
			{ev: EvAliceSendAddHtlc},
			{ev: EvAliceSendCommit},
			{ev: EvBobSettleHtlc},
			{ev: EvBobSendCommit},
		})
	})
	t.Run("reconnect_lost_settle", func(t *testing.T) {
		runSteps(t, []step{
			{ev: EvAliceSendAddHtlc},
			{ev: EvAliceSendCommit},
			{ev: EvBobSettleHtlc},
			{ev: EvReconnect},
			{ev: EvBobSettleHtlc},
			{ev: EvBobSendCommit},
		})
	})
	t.Run("reconnect_lost_fail", func(t *testing.T) {
		runSteps(t, []step{
			{ev: EvAliceSendAddHtlc},
			{ev: EvAliceSendCommit},
			{ev: EvBobFailHtlc},
			{ev: EvReconnect},
			{ev: EvBobFailHtlc},
			{ev: EvBobSendCommit},
		})
	})
	// Alice's add arrives but her commit_sig is lost: she persisted the
	// signed state, so after the reestablish she retransmits both, and
	// Bob, who forgot the uncommitted add, accepts them.
	t.Run("reconnect_lost_commit_sig", func(t *testing.T) {
		runSteps(t, []step{
			{ev: EvAliceHoldOutbox, args: []byte{3}},
			{ev: EvAliceSendAddHtlc},
			{ev: EvAliceSendCommit},
			{ev: EvReconnect, args: []byte{1, 0}},
			{ev: EvBobSettleHtlc},
			{ev: EvBobSendCommit},
		})
	})
	// The connection drops in the middle of Alice's commitment dance,
	// right after her commit_sig reaches Bob: his revoke_and_ack and
	// commit_sig are lost and must be recovered after reestablish.
	t.Run("reconnect_cut_mid_dance", func(t *testing.T) {
		runSteps(t, []step{
			{ev: EvAliceSendAddHtlc},
			{ev: EvArmDisconnect, args: []byte{0}},
			{ev: EvAliceSendCommit},
			{ev: EvBobSettleHtlc},
			{ev: EvBobSendCommit},
		})
	})
	// Bob's revoke_and_ack (and his own commit_sig) for Alice's commitment
	// are lost: he must retransmit the revocation after reestablish.
	t.Run("reconnect_lost_revoke", func(t *testing.T) {
		runSteps(t, []step{
			{ev: EvBobHoldOutbox, args: []byte{3}},
			{ev: EvAliceSendAddHtlc},
			{ev: EvAliceSendCommit},
			{ev: EvReconnect},
			{ev: EvBobSettleHtlc},
			{ev: EvBobSendCommit},
		})
	})

	// An anchor channel with zero-fee HTLC transactions and asymmetric
	// parameters (dust limits 859/960 sat, reserves 2/5%, MinHTLC 1000/1
	// msat, 30/15 accepted HTLCs, 658 sat/kw): HTLCs both ways, a fee
	// update, a connection cut mid-dance, then resolution.
	t.Run("anchors_shaped_channel", func(t *testing.T) {
		f := runShapedSteps(t, 0x5a5a5a5b, []step{
			{ev: EvAliceSendAddHtlc},
			{ev: EvBobSendAddHtlc},
			{ev: EvAliceSendCommit},
			{ev: EvAliceSendUpdateFee, args: feeTo12000},
			{ev: EvArmDisconnect, args: []byte{2}},
			{ev: EvAliceSendCommit},
			{ev: EvAliceSettleHtlc},
			{ev: EvBobFailHtlc},
			{ev: EvBobSendCommit},
		})
		require.True(t, f.alice.channel.State().ChanType.HasAnchors())
	})

	// The same exchange on a production simple taproot channel, which
	// runs on real MuSig2 and Schnorr signatures instead of the trivial
	// signing scheme.
	t.Run("taproot_shaped_channel", func(t *testing.T) {
		f := runShapedSteps(t, taprootShape, []step{
			{ev: EvAliceSendAddHtlc},
			{ev: EvBobSendAddHtlc},
			{ev: EvAliceSendCommit},
			{ev: EvAliceSendUpdateFee, args: feeTo12000},
			{ev: EvArmDisconnect, args: []byte{2}},
			{ev: EvAliceSendCommit},
			{ev: EvAliceSettleHtlc},
			{ev: EvBobFailHtlc},
			{ev: EvBobSendCommit},
		})
		require.True(t, f.alice.channel.State().ChanType.IsTaproot())
	})

	// Direct assertion that the ErrNoWindow path is reachable: after one
	// SignNextCommitment the remote chain is unacked, so a second call
	// must return ErrNoWindow. This complements the scenarios above by
	// pinning the precondition the new events rely on.
	t.Run("sign_next_commitment_no_window", func(t *testing.T) {
		f := newFuzzFSM(
			t, uint64(1_000_000), uint64(50), uint32(0), uint64(0),
			uint64(0),
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

	// Bad-onion scenarios — each mode argument selects one of the three
	// failure branches in processRemoteAdds (decode / payload / extract).
	// Driving Alice→Bob and Bob→Alice for each mode runs the bad HTLC
	// through a full add/commit/revoke cycle, so the receiver hits the
	// targeted branch and fails the HTLC back via UpdateFailHTLC.
	badOnionSteps := func(mode byte) []step {
		return []step{
			{ev: EvAliceSendBadOnion, args: []byte{mode}},
			{ev: EvAliceSendCommit},
			{ev: EvBobSendBadOnion, args: []byte{mode}},
			{ev: EvBobSendCommit},
		}
	}
	t.Run("bad_onion_decode", func(t *testing.T) {
		runSteps(t, badOnionSteps(0))
	})
	t.Run("bad_onion_payload", func(t *testing.T) {
		runSteps(t, badOnionSteps(1))
	})
	t.Run("bad_onion_extract", func(t *testing.T) {
		runSteps(t, badOnionSteps(2))
	})
}

// FuzzChannelLinkFSM is a coverage-guided fuzz test for the two-party
// commitment protocol between Alice and Bob. The corpus is read as a stream:
// one byte selects one of the NumEvents protocol actions for either peer, and
// the event then reads its own arguments (amount, fee rate, target HTLC, ...)
// from the bytes that follow. After every event the pending messages are
// drained and assertInvariants verifies that both sides remain in a
// consistent state (matching commitment heights, balanced totals). A run that
// ends without an expected link failure is then flushed and must converge
// (flushAndAssertConverged). The fuzzer explores arbitrary interleavings of
// these actions to find protocol violations that deterministic scenarios
// might miss.
func FuzzChannelLinkFSM(f *testing.F) {
	// Seed inputs: the same event sequence on the default tweakless
	// channel, an anchor channel and a taproot channel (chanShape, see
	// fuzzChanShape).
	// Bytes following an event double as its arguments.
	// maxFeeExposureGen=0  → DefaultMaxFeeExposure (no override).
	// maxFeeAllocationGen=0 → DefaultMaxLinkFeeAllocation (no override).
	seedEvents := []byte{byte(EvAliceSendAddHtlc), byte(EvAliceSendCommit),
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
		byte(EvAliceInitQuiescence)}
	f.Add(uint64(1_000_000), uint64(10_000_000), uint64(50), uint32(0),
		uint64(0), uint64(0), seedEvents)
	f.Add(uint64(1_000_000), uint64(10_000_000), uint64(50), uint32(1),
		uint64(0), uint64(0), seedEvents)
	f.Add(uint64(1_000_000), uint64(10_000_000), uint64(50),
		uint32(1|1<<31), uint64(0), uint64(0), seedEvents)
	f.Fuzz(func(t *testing.T, channelSize, htlcRef uint64,
		aliceShareGen uint64, chanShape uint32, maxFeeExposureGen,
		maxFeeAllocationGen uint64, data []byte) {

		fuzzFSM := newFuzzFSM(
			t, channelSize, aliceShareGen, chanShape,
			maxFeeExposureGen, maxFeeAllocationGen,
		)

		fuzzFSM.htlcRef = htlcRef
		fuzzFSM.in = &fuzzInput{data: data}

		for !fuzzFSM.in.done() {
			evt := Event(fuzzFSM.in.u8() % uint8(NumEvents))

			// Stop once the run has spent its work budget. This
			// bounds the real (adds × commit rounds) cost so heavy
			// events cannot make a run hang, while staying fully
			// deterministic (no wall-clock).
			fuzzFSM.workSpent += fuzzFSM.costOf(evt)
			if fuzzFSM.workSpent > MaxWorkPerRun {
				break
			}

			fuzzFSM.step(evt)
			if fuzzFSM.terminated {
				return
			}
			fuzzFSM.assertInvariants()
		}

		fuzzFSM.flushAndAssertConverged()
	})
}
