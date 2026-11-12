package lnwallet

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"runtime"
	"slices"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// newRevAuxSigEntry is a shorthand for building a revocation aux sig entry.
func newRevAuxSigEntry(signerIncoming bool, htlcIndex uint64,
	sig []byte) revocationAuxSigEntry {

	return revocationAuxSigEntry{
		revocationAuxSigKey: revocationAuxSigKey{
			signerIncoming: signerIncoming,
			htlcIndex:      htlcIndex,
		},
		sig: sig,
	}
}

// TestPackUnpackRevocationAuxSigs tests the round-trip encoding and decoding
// of revocation aux sig entries.
func TestPackUnpackRevocationAuxSigs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		entries []revocationAuxSigEntry
	}{
		{
			name:    "empty entries",
			entries: []revocationAuxSigEntry{},
		},
		{
			name: "single entry with one HTLC",
			entries: []revocationAuxSigEntry{
				newRevAuxSigEntry(
					true, 42, []byte{0xaa, 0xbb, 0xcc},
				),
			},
		},
		{
			name: "multiple entries with various HTLC indices",
			entries: []revocationAuxSigEntry{
				newRevAuxSigEntry(false, 0, []byte{0x01}),
				newRevAuxSigEntry(
					true, 100, []byte{0x04, 0x05, 0x06},
				),
				newRevAuxSigEntry(
					false, 999, []byte{0x08, 0x09},
				),
			},
		},
		{
			// Both parties count the HTLCs they offer from zero,
			// so the same index in both directions is the normal
			// case and must not collapse into one entry.
			name: "same HTLC index in both directions",
			entries: []revocationAuxSigEntry{
				newRevAuxSigEntry(false, 0, []byte{0x0a}),
				newRevAuxSigEntry(true, 0, []byte{0x0b}),
			},
		},
		{
			name: "entry without a sig",
			entries: []revocationAuxSigEntry{
				newRevAuxSigEntry(true, 7, []byte{}),
			},
		},
		{
			name: "max uint64 HTLC index",
			entries: []revocationAuxSigEntry{
				newRevAuxSigEntry(
					false, math.MaxUint64,
					[]byte{0xde, 0xad},
				),
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			packed, err := packRevocationAuxSigs(tc.entries)
			require.NoError(t, err)

			sigMap, err := unpackRevocationAuxSigs(packed)
			require.NoError(t, err)

			require.Len(t, sigMap, len(tc.entries))

			for _, entry := range tc.entries {
				got, ok := sigMap[entry.revocationAuxSigKey]
				require.True(t, ok, "missing HTLC index %d "+
					"(signer incoming=%v)",
					entry.htlcIndex, entry.signerIncoming)

				require.Equal(
					t, entry.revocationAuxSigKey,
					got.revocationAuxSigKey,
				)
				require.Equal(t, entry.sig, got.sig)
			}
		})
	}
}

// TestRevokeAndAckAuxSigsCustomOnly asserts the custom-channel-only gating of
// the revocation AuxSig hooks: on a NON-custom taproot channel, even with an
// aux signer present that negotiates SigHashDefault, RevokeCurrentCommitment
// must not attach any custom records to the RevokeAndAck, and
// ReceiveRevocation must accept a revocation while ignoring any custom
// records it may carry.
func TestRevokeAndAckAuxSigsCustomOnly(t *testing.T) {
	t.Parallel()

	chanType := channeldb.SimpleTaprootFeatureBit |
		channeldb.AnchorOutputsBit | channeldb.ZeroHtlcTxFeeBit |
		channeldb.SingleFunderTweaklessBit

	aliceChannel, bobChannel, err := CreateTestChannels(t, chanType)
	require.NoError(t, err, "unable to create test channels")

	// An aux signer that would negotiate SigHashDefault. Since the
	// channel type carries no tapscript root, the custom-channel gate
	// must ignore it entirely. It is attached right before the
	// revocation steps below (rather than at channel creation) so the
	// unrelated CommitSig-time aux machinery stays out of the picture.
	pushySigner := fn.Some[AuxSigner](&sigHashOverrideSigner{
		MockAuxSigner: NewAuxSignerMock(nil),
		sigHash:       txscript.SigHashDefault,
	})

	// Lock in an HTLC so the revoked commitment actually carries one.
	paymentPreimage := bytes.Repeat([]byte{1}, 32)
	paymentHash := sha256.Sum256(paymentPreimage)
	htlcAmt := lnwire.NewMSatFromSatoshis(btcutil.SatoshiPerBitcoin)
	htlc := &lnwire.UpdateAddHTLC{
		PaymentHash: paymentHash,
		Amount:      htlcAmt,
		Expiry:      uint32(5),
	}
	_, err = aliceChannel.AddHTLC(htlc, nil)
	require.NoError(t, err)
	_, err = bobChannel.ReceiveHTLC(htlc)
	require.NoError(t, err)

	aliceNewCommit, err := aliceChannel.SignNextCommitment(t.Context())
	require.NoError(t, err)
	err = bobChannel.ReceiveNewCommitment(aliceNewCommit.CommitSigs)
	require.NoError(t, err)

	// Bob revokes: the resulting RevokeAndAck must carry NO custom
	// records on a non-custom channel.
	bobChannel.auxSigner = pushySigner
	bobRevocation, _, _, err := bobChannel.RevokeCurrentCommitment()
	require.NoError(t, err)
	require.Empty(t, bobRevocation.CustomRecords,
		"non-custom channel must not attach revocation aux sigs")

	// Alice processes the revocation: even if the message carried stray
	// custom records, a non-custom channel must ignore them and accept
	// the revocation.
	bobRevocation.CustomRecords = lnwire.CustomRecords{
		uint64(lnwire.MinCustomRecordsTlvType): []byte{0xde, 0xad},
	}
	aliceChannel.auxSigner = pushySigner
	_, _, err = aliceChannel.ReceiveRevocation(bobRevocation)
	require.NoError(t, err,
		"non-custom channel must ignore revocation custom records")
}

// fakeAuxSigFor returns the canned signature blob fakeRevocationAuxSigner
// hands out for an HTLC. It is distinct per direction and index, so tests can
// assert that a sig is never attributed to another HTLC between signing,
// packing, wire transfer, verification and persistence.
func fakeAuxSigFor(signerIncoming bool, htlcIndex uint64) []byte {
	direction := byte(0x00)
	if signerIncoming {
		direction = 0x01
	}

	return []byte{0x51, direction, byte(htlcIndex)}
}

// fakeRevocationAuxSigner is a minimal AuxSigner used to exercise the
// revocation AuxSig paths end to end on a custom channel. It negotiates
// SigHashDefault, answers every sign job with the canned signature blob of
// the job's HTLC, and records the sign and verify jobs it processes.
type fakeRevocationAuxSigner struct {
	signJobs   []AuxSigJob
	verifyJobs []AuxVerifyJob
	verifyErr  error

	// noneSigs makes every sign job respond without a signature blob,
	// simulating BTC-only HTLCs on a custom channel that need no aux
	// sigs.
	noneSigs bool

	// signErr makes SubmitSecondLevelSigBatch fail as a whole,
	// simulating an aux signer that cannot take the batch at all.
	signErr error

	// respErr makes every sign job respond with an error, simulating an
	// aux signer that fails while signing.
	respErr error

	// strict makes verification behave like a real aux signer for a
	// commitment whose every HTLC carries aux outputs: a job must carry
	// a sig, and it must be the sig of exactly that HTLC, direction
	// included.
	strict bool
}

func (s *fakeRevocationAuxSigner) SubmitSecondLevelSigBatch(_ AuxChanState,
	_ *wire.MsgTx, jobs []AuxSigJob) error {

	if s.signErr != nil {
		return s.signErr
	}

	s.signJobs = append(s.signJobs, jobs...)
	for _, job := range jobs {
		if s.respErr != nil {
			job.Resp <- AuxSigJobResp{
				HtlcIndex: job.HTLC.HtlcIndex,
				Err:       s.respErr,
			}

			continue
		}
		if s.noneSigs {
			job.Resp <- AuxSigJobResp{
				SigBlob:   fn.None[tlv.Blob](),
				HtlcIndex: job.HTLC.HtlcIndex,
			}

			continue
		}

		// The job describes an HTLC of the signer's own commitment,
		// so its direction is already the signer's.
		blob := fakeAuxSigFor(job.Incoming, job.HTLC.HtlcIndex)
		job.Resp <- AuxSigJobResp{
			SigBlob:   fn.Some[tlv.Blob](blob),
			HtlcIndex: job.HTLC.HtlcIndex,
		}
	}

	return nil
}

func (s *fakeRevocationAuxSigner) PackSigs(
	[]fn.Option[tlv.Blob]) fn.Result[fn.Option[tlv.Blob]] {

	return fn.Ok(fn.None[tlv.Blob]())
}

func (s *fakeRevocationAuxSigner) UnpackSigs(
	fn.Option[tlv.Blob]) fn.Result[[]fn.Option[tlv.Blob]] {

	return fn.Ok([]fn.Option[tlv.Blob]{})
}

func (s *fakeRevocationAuxSigner) VerifySecondLevelSigs(_ AuxChanState,
	_ *wire.MsgTx, jobs []AuxVerifyJob) error {

	s.verifyJobs = append(s.verifyJobs, jobs...)

	if s.verifyErr != nil {
		return s.verifyErr
	}
	if !s.strict {
		return nil
	}

	for _, job := range jobs {
		idx := job.HTLC.HtlcIndex
		if job.SigBlob.IsNone() {
			return fmt.Errorf("missing aux sig for HTLC index %d "+
				"(incoming=%v)", idx, job.Incoming)
		}

		// The job describes an HTLC of the remote commitment from
		// the verifier's point of view, so the signer saw it in the
		// opposite direction.
		want := fakeAuxSigFor(!job.Incoming, idx)
		if !bytes.Equal(want, job.SigBlob.UnwrapOr(nil)) {
			return fmt.Errorf("aux sig of another HTLC for HTLC "+
				"index %d (incoming=%v)", idx, job.Incoming)
		}
	}

	return nil
}

func (s *fakeRevocationAuxSigner) HtlcSigHashType(
	_ HtlcSigHashReq) fn.Option[txscript.SigHashType] {

	return fn.Some(txscript.SigHashDefault)
}

// setupRevocationAuxSigTest creates a custom (tapscript root) channel pair,
// locks in the HTLCs of setupRevocationAuxSigChannels, then lets Bob revoke
// the commitment carrying them. Bob's fake aux signer is attached right
// before his revocation, so the sigs it produces end up in the returned
// RevokeAndAck. Alice's fake signer is returned unattached: callers attach
// it (after optionally configuring it) right before feeding the revocation
// to Alice.
func setupRevocationAuxSigTest(t *testing.T,
	bobSigner *fakeRevocationAuxSigner) (*LightningChannel,
	*LightningChannel, *lnwire.RevokeAndAck, *fakeRevocationAuxSigner) {

	t.Helper()

	aliceChannel, bobChannel := setupRevocationAuxSigChannels(t)

	// Attach Bob's fake signer only now, so only the revocation-time aux
	// paths see it.
	bobChannel.auxSigner = fn.Some[AuxSigner](bobSigner)

	bobRevocation, _, _, err := bobChannel.RevokeCurrentCommitment()
	require.NoError(t, err)

	return aliceChannel, bobChannel, bobRevocation,
		&fakeRevocationAuxSigner{}
}

// markChanSigHashDefault emulates a channel whose aux signer negotiated
// DeterministicHTLCs (SigHashDefault), by setting the sighash type the
// channel uses for new commitments to the value NewLightningChannel would
// have resolved.
func markChanSigHashDefault(c *LightningChannel) {
	c.htlcSigHashType.Store(uint32(txscript.SigHashDefault))
}

// Amounts of the HTLCs the revocation aux sig tests lock in.
const (
	// revAuxNonDustAmt is far above every dust threshold.
	revAuxNonDustAmt = btcutil.Amount(100_000)

	// revAuxDustAmt is dust on both commitments once the second-level
	// fee and anchor are accounted for.
	revAuxDustAmt = btcutil.Amount(400)

	// revAuxBobDustAmt sits between the two parties' dust thresholds: it
	// is dust on Bob's commitment and would not be on Alice's. It is what
	// tells the two dust limits apart in these tests.
	revAuxBobDustAmt = btcutil.Amount(1_000)
)

// revAuxHtlcAdder adds HTLCs to a pair of channels in either direction,
// keeping the per-offerer HTLC counters the way the wire protocol does.
type revAuxHtlcAdder struct {
	t *testing.T

	alice *LightningChannel
	bob   *LightningChannel

	nextPreimage byte
}

// add offers an HTLC of the given amount from one channel to the other and
// returns its HTLC index.
func (a *revAuxHtlcAdder) add(from, to *LightningChannel,
	amt btcutil.Amount) uint64 {

	a.t.Helper()

	a.nextPreimage++
	preimage := bytes.Repeat([]byte{a.nextPreimage}, 32)
	htlc := &lnwire.UpdateAddHTLC{
		PaymentHash: sha256.Sum256(preimage),
		Amount:      lnwire.NewMSatFromSatoshis(amt),
		Expiry:      uint32(10),
	}

	htlcIndex, err := from.AddHTLC(htlc, nil)
	require.NoError(a.t, err)

	htlc.ID = htlcIndex
	_, err = to.ReceiveHTLC(htlc)
	require.NoError(a.t, err)

	return htlcIndex
}

// fromAlice offers an HTLC from Alice to Bob.
func (a *revAuxHtlcAdder) fromAlice(amt btcutil.Amount) uint64 {
	return a.add(a.alice, a.bob, amt)
}

// fromBob offers an HTLC from Bob to Alice.
func (a *revAuxHtlcAdder) fromBob(amt btcutil.Amount) uint64 {
	return a.add(a.bob, a.alice, amt)
}

// newRevocationAuxSigChannels creates the custom channel pair the revocation
// aux sig tests run on, with DeterministicHTLCs negotiated on both sides.
func newRevocationAuxSigChannels(t *testing.T) (*LightningChannel,
	*LightningChannel, *revAuxHtlcAdder) {

	t.Helper()

	chanType := channeldb.SimpleTaprootFeatureBit |
		channeldb.TapscriptRootBit

	aliceChannel, bobChannel, err := CreateTestChannels(t, chanType)
	require.NoError(t, err, "unable to create test channels")

	// Emulate channels whose aux signer negotiated DeterministicHTLCs:
	// the sighash type used for new commitments is set to exactly what
	// NewLightningChannel would have resolved had a
	// SigHashDefault-negotiating signer been attached from the start. A
	// late signer swap (as done below for the fake revocation signers)
	// does not change it, as it is only re-resolved on request.
	markChanSigHashDefault(aliceChannel)
	markChanSigHashDefault(bobChannel)

	// The tests rely on the two parties having different dust limits, so
	// that using the wrong party's limit shows.
	aliceDust := aliceChannel.channelState.LocalChanCfg.DustLimit
	bobDust := bobChannel.channelState.LocalChanCfg.DustLimit
	require.Less(t, aliceDust, bobDust)

	return aliceChannel, bobChannel, &revAuxHtlcAdder{
		t:     t,
		alice: aliceChannel,
		bob:   bobChannel,
	}
}

// signRevokedCommitment makes Alice sign a new commitment for Bob and Bob
// accept it, which leaves Bob about to revoke his previous commitment. No
// aux signer is attached yet, so the CommitSig-time aux machinery stays out
// of the picture.
func signRevokedCommitment(t *testing.T, aliceChannel,
	bobChannel *LightningChannel) {

	t.Helper()

	aliceNewCommit, err := aliceChannel.SignNextCommitment(t.Context())
	require.NoError(t, err)
	err = bobChannel.ReceiveNewCommitment(aliceNewCommit.CommitSigs)
	require.NoError(t, err)
}

// setupRevocationAuxSigChannels creates the custom channel pair and runs the
// commitment dance right up to (but not including) Bob's revocation of the
// commitment carrying three HTLCs offered by Alice: index 0 (non-dust), index
// 1 (dust for both parties) and index 2 (dust under Bob's dust limit only).
// No fake revocation aux signer is attached yet.
func setupRevocationAuxSigChannels(t *testing.T) (*LightningChannel,
	*LightningChannel) {

	t.Helper()

	aliceChannel, bobChannel, adder := newRevocationAuxSigChannels(t)

	require.EqualValues(t, 0, adder.fromAlice(revAuxNonDustAmt))
	require.EqualValues(t, 1, adder.fromAlice(revAuxDustAmt))
	require.EqualValues(t, 2, adder.fromAlice(revAuxBobDustAmt))

	// The HTLC at index 2 is dust on the commitment Bob revokes, which is
	// built with Bob's dust limit, and would not be with Alice's.
	bobState := bobChannel.channelState
	feePerKw := chainfee.SatPerKWeight(bobState.LocalCommitment.FeePerKw)
	isDust := func(dustLimit btcutil.Amount) bool {
		return HtlcIsDust(
			bobState.ChanType, true, lntypes.Local, feePerKw,
			revAuxBobDustAmt, dustLimit, true,
		)
	}
	require.True(t, isDust(bobState.LocalChanCfg.DustLimit))
	require.False(t, isDust(bobState.RemoteChanCfg.DustLimit))

	// Lock the HTLCs into both commitments.
	require.NoError(t, ForceStateTransition(aliceChannel, bobChannel))

	// A further add triggers the state transition whose revocation we
	// care about: Bob will revoke the commitment carrying HTLCs 0 to 2.
	adder.fromAlice(revAuxNonDustAmt)
	signRevokedCommitment(t, aliceChannel, bobChannel)

	return aliceChannel, bobChannel
}

// revokedHtlcAuxSigs returns the revocation aux sig persisted for each HTLC
// of the given revoked state, keyed by direction and HTLC index.
func revokedHtlcAuxSigs(t *testing.T, channel *LightningChannel,
	revokedHeight uint64) map[revocationAuxSigKey][]byte {

	t.Helper()

	revokedLog, _, err := channel.channelState.FindPreviousState(
		revokedHeight,
	)
	require.NoError(t, err)

	sigType := uint64(revocationAuxSigType.TypeVal())
	sigs := make(map[revocationAuxSigKey][]byte)
	for _, htlcEntry := range revokedLog.HTLCEntries {
		htlcIndex, err := htlcEntry.HtlcIndex.ValOpt().UnwrapOrErr(
			errors.New("revocation log entry without HTLC index"),
		)
		require.NoError(t, err)

		key := revocationAuxSigKey{
			// The log is ours, so an HTLC incoming to us is
			// outgoing for the party that revoked the state.
			signerIncoming: !htlcEntry.Incoming.Val,
			htlcIndex:      htlcIndex.Int(),
		}

		blob := htlcEntry.CustomBlob.ValOpt().UnwrapOr(nil)
		if len(blob) == 0 {
			sigs[key] = nil

			continue
		}

		records, err := lnwire.ParseCustomRecords(blob)
		require.NoError(t, err)
		sigs[key] = records[sigType]
	}

	return sigs
}

// TestRevocationAuxSigsRoundTrip exercises the full happy path on a custom
// channel: Bob's revocation signs the second-level path of every non-dust
// HTLC on the revoked commitment, ships the sigs in
// RevokeAndAck.CustomRecords, and Alice verifies them before accepting the
// revocation and persists them into the remote commitment's HTLC custom
// records. Dust HTLCs are excluded on both sides, as decided by the dust
// limit of the commitment's owner.
func TestRevocationAuxSigsRoundTrip(t *testing.T) {
	t.Parallel()

	bobSigner := &fakeRevocationAuxSigner{}
	aliceChannel, _, bobRevocation, aliceSigner :=
		setupRevocationAuxSigTest(t, bobSigner)

	// The remote chain tail is the commitment Bob is revoking; its height
	// keys the revocation log entry we'll inspect below.
	revokedHeight := aliceChannel.commitChains.Remote.tail().height

	// Bob signed exactly one HTLC: the two dust HTLCs must not have
	// produced sign jobs. The HTLC is incoming to him, so the path he
	// signs is the success, which carries no timeout.
	require.Len(t, bobSigner.signJobs, 1)
	signJob := bobSigner.signJobs[0]
	require.True(t, signJob.Incoming)
	require.EqualValues(t, 0, signJob.HTLC.HtlcIndex)
	require.Equal(t, lntypes.Local, signJob.WhoseCommit)
	require.True(t, signJob.HtlcTimeout.IsNone())

	// The RevokeAndAck carries the packed sig for HTLC index 0 only.
	recType := uint64(lnwire.MinCustomRecordsTlvType)
	blob := bobRevocation.CustomRecords[recType]
	require.NotEmpty(t, blob, "revocation aux sig blob missing")

	sigMap, err := unpackRevocationAuxSigs(blob)
	require.NoError(t, err)
	require.Len(t, sigMap, 1)

	htlcKey := revocationAuxSigKey{signerIncoming: true, htlcIndex: 0}
	entry, ok := sigMap[htlcKey]
	require.True(t, ok, "expected sig for HTLC index 0")
	require.Equal(t, fakeAuxSigFor(true, 0), entry.sig)

	// Alice verifies and accepts. Her verifier insists on the sig of
	// exactly this HTLC.
	aliceSigner.strict = true
	aliceChannel.auxSigner = fn.Some[AuxSigner](aliceSigner)
	_, _, err = aliceChannel.ReceiveRevocation(bobRevocation)
	require.NoError(t, err)

	// Alice verified the non-dust HTLC only. From her side it is an
	// outgoing HTLC on the remote commitment.
	require.Len(t, aliceSigner.verifyJobs, 1)
	verifyJob := aliceSigner.verifyJobs[0]
	require.False(t, verifyJob.Incoming)
	require.EqualValues(t, 0, verifyJob.HTLC.HtlcIndex)
	require.Equal(t, lntypes.Remote, verifyJob.WhoseCommit)
	require.True(t, verifyJob.HtlcTimeout.IsNone())
	require.Equal(
		t, fakeAuxSigFor(true, 0), verifyJob.SigBlob.UnwrapOr(nil),
	)

	// The verified sig is persisted in the revocation log entry of the
	// revoked commitment (the breach-time source of truth). The dust
	// HTLCs have no entry there at all.
	require.Equal(t, map[revocationAuxSigKey][]byte{
		htlcKey: fakeAuxSigFor(true, 0),
	}, revokedHtlcAuxSigs(t, aliceChannel, revokedHeight))
}

// TestRevocationAuxSigsSameIndexBothDirections asserts that the revocation
// aux sigs of an HTLC offered by each party under the same HTLC index stay
// apart. Both parties count the HTLCs they offer from zero, so this is the
// normal state of any channel that carries traffic in both directions.
// Keying the sigs by the bare index would fold the two HTLCs into one entry:
// the receiver would then either reject an honest revocation, leaving the
// channel unusable, or store one HTLC's sig for the other, leaving it
// without breach protection.
func TestRevocationAuxSigsSameIndexBothDirections(t *testing.T) {
	t.Parallel()

	aliceChannel, bobChannel, adder := newRevocationAuxSigChannels(t)

	// One HTLC in each direction, both at index 0.
	require.EqualValues(t, 0, adder.fromAlice(revAuxNonDustAmt))
	require.EqualValues(t, 0, adder.fromBob(revAuxNonDustAmt))

	// Lock both HTLCs into both commitments.
	require.NoError(t, ForceStateTransition(aliceChannel, bobChannel))
	require.NoError(t, ForceStateTransition(bobChannel, aliceChannel))

	// Bob is about to revoke the commitment carrying both.
	adder.fromAlice(revAuxNonDustAmt)
	signRevokedCommitment(t, aliceChannel, bobChannel)

	bobSigner := &fakeRevocationAuxSigner{}
	bobChannel.auxSigner = fn.Some[AuxSigner](bobSigner)
	bobRevocation, _, _, err := bobChannel.RevokeCurrentCommitment()
	require.NoError(t, err)

	// Bob signed each HTLC once: the success path of the one he
	// received, the timeout path of the one he offered.
	require.Len(t, bobSigner.signJobs, 2)
	for _, job := range bobSigner.signJobs {
		require.EqualValues(t, 0, job.HTLC.HtlcIndex)
		require.Equal(t, !job.Incoming, job.HtlcTimeout.IsSome())
	}
	require.NotEqual(
		t, bobSigner.signJobs[0].Incoming,
		bobSigner.signJobs[1].Incoming,
	)

	// The blob carries one entry per HTLC, each with its own sig.
	recType := uint64(lnwire.MinCustomRecordsTlvType)
	sigMap, err := unpackRevocationAuxSigs(
		bobRevocation.CustomRecords[recType],
	)
	require.NoError(t, err)
	require.Len(t, sigMap, 2)

	receivedKey := revocationAuxSigKey{signerIncoming: true}
	offeredKey := revocationAuxSigKey{signerIncoming: false}
	require.Equal(t, fakeAuxSigFor(true, 0), sigMap[receivedKey].sig)
	require.Equal(t, fakeAuxSigFor(false, 0), sigMap[offeredKey].sig)

	// Alice's verifier rejects a sig that belongs to another HTLC, so
	// accepting the revocation proves each sig reached the right one.
	revokedHeight := aliceChannel.commitChains.Remote.tail().height
	aliceSigner := &fakeRevocationAuxSigner{strict: true}
	aliceChannel.auxSigner = fn.Some[AuxSigner](aliceSigner)
	_, _, err = aliceChannel.ReceiveRevocation(bobRevocation)
	require.NoError(t, err)
	require.Len(t, aliceSigner.verifyJobs, 2)

	// And each HTLC keeps its own sig in the revocation log.
	require.Equal(t, map[revocationAuxSigKey][]byte{
		receivedKey: fakeAuxSigFor(true, 0),
		offeredKey:  fakeAuxSigFor(false, 0),
	}, revokedHtlcAuxSigs(t, aliceChannel, revokedHeight))
}

// TestRevocationAuxSigsRejected asserts that ReceiveRevocation rejects the
// revocation BEFORE advancing the remote chain tail whenever the received
// aux sigs are unverifiable, malformed, or missing for a non-dust HTLC.
func TestRevocationAuxSigsRejected(t *testing.T) {
	t.Parallel()

	recType := uint64(lnwire.MinCustomRecordsTlvType)

	// appendEntry adds the given entry to the blob of a revocation.
	appendEntry := func(entry revocationAuxSigEntry) func(
		*lnwire.RevokeAndAck, *fakeRevocationAuxSigner) {

		return func(rev *lnwire.RevokeAndAck,
			_ *fakeRevocationAuxSigner) {

			mutatePackedEntries(t, rev,
				func(entries []revocationAuxSigEntry,
				) []revocationAuxSigEntry {

					return append(entries, entry)
				},
			)
		}
	}

	testCases := []struct {
		name    string
		mutate  func(*lnwire.RevokeAndAck, *fakeRevocationAuxSigner)
		wantErr string
	}{{
		name: "verification failure",
		mutate: func(_ *lnwire.RevokeAndAck,
			s *fakeRevocationAuxSigner) {

			s.verifyErr = fmt.Errorf("sig does not verify")
		},
		wantErr: "invalid revocation aux sigs",
	}, {
		name: "malformed blob",
		mutate: func(rev *lnwire.RevokeAndAck,
			_ *fakeRevocationAuxSigner) {

			rev.CustomRecords[recType] = []byte{0xde, 0xad}
		},
		wantErr: "invalid revocation aux sigs",
	}, {
		name: "missing sig for non-dust HTLC",
		mutate: func(rev *lnwire.RevokeAndAck,
			_ *fakeRevocationAuxSigner) {

			// A well-formed blob whose sig is tagged with an HTLC
			// index that isn't on the commitment.
			packed, err := packRevocationAuxSigs(
				[]revocationAuxSigEntry{newRevAuxSigEntry(
					true, 99, fakeAuxSigFor(true, 99),
				)},
			)
			if err != nil {
				panic(err)
			}
			rev.CustomRecords[recType] = packed
		},
		wantErr: "no revocation aux sig for HTLC index 0",
	}, {
		name: "sig tagged with the wrong direction",
		mutate: func(rev *lnwire.RevokeAndAck,
			_ *fakeRevocationAuxSigner) {

			// The right index, but for an HTLC the signer would
			// have offered. The HTLC on the commitment is one it
			// received.
			packed, err := packRevocationAuxSigs(
				[]revocationAuxSigEntry{newRevAuxSigEntry(
					false, 0, fakeAuxSigFor(true, 0),
				)},
			)
			if err != nil {
				panic(err)
			}
			rev.CustomRecords[recType] = packed
		},
		wantErr: "no revocation aux sig for HTLC index 0",
	}, {
		name: "withheld blob",
		mutate: func(rev *lnwire.RevokeAndAck,
			_ *fakeRevocationAuxSigner) {

			// A peer that strips the records entirely must not
			// get its revocation accepted while non-dust HTLCs
			// sit on the revoked commitment.
			rev.CustomRecords = nil
		},
		wantErr: "no revocation aux sig for HTLC index 0",
	}, {
		// An entry is there, but it carries no sig. Only the aux
		// signer knows that the HTLC carries aux outputs and owes
		// one, so it must be asked, and its refusal must fail the
		// revocation. Otherwise a peer could opt out of the breach
		// protection for exactly the states it means to broadcast.
		name: "sig withheld from the entry",
		mutate: func(rev *lnwire.RevokeAndAck,
			s *fakeRevocationAuxSigner) {

			s.strict = true
			mutatePackedEntries(t, rev,
				func(entries []revocationAuxSigEntry,
				) []revocationAuxSigEntry {

					entries[0].sig = nil

					return entries
				},
			)
		},
		wantErr: "missing aux sig for HTLC index 0",
	}, {
		name: "sig of another HTLC",
		mutate: func(rev *lnwire.RevokeAndAck,
			s *fakeRevocationAuxSigner) {

			s.strict = true
			mutatePackedEntries(t, rev,
				func(entries []revocationAuxSigEntry,
				) []revocationAuxSigEntry {

					entries[0].sig = fakeAuxSigFor(
						false, 0,
					)

					return entries
				},
			)
		},
		wantErr: "aux sig of another HTLC",
	}, {
		name: "extra entry for unknown HTLC index",
		mutate: appendEntry(
			newRevAuxSigEntry(true, 99, []byte{0x99}),
		),
		wantErr: "unexpected revocation aux sig entry for HTLC " +
			"index 99",
	}, {
		// The same index as the HTLC on the commitment, but in the
		// direction nothing was offered in.
		name: "extra entry for the other direction",
		mutate: appendEntry(
			newRevAuxSigEntry(false, 0, []byte{0x99}),
		),
		wantErr: "unexpected revocation aux sig entry for HTLC " +
			"index 0",
	}, {
		// HTLC index 1 is dust for both parties: an honest signer
		// never packs an entry for it.
		name: "entry for dust HTLC",
		mutate: appendEntry(
			newRevAuxSigEntry(true, 1, []byte{0x01}),
		),
		wantErr: "unexpected revocation aux sig entry for HTLC " +
			"index 1",
	}, {
		// HTLC index 2 is dust on the revoked commitment, which is
		// built with the signer's dust limit. It would not be dust
		// under ours, so this entry is only refused if the dust
		// limit of the right party is applied.
		name: "entry for HTLC that is dust for the signer only",
		mutate: appendEntry(
			newRevAuxSigEntry(true, 2, []byte{0x02}),
		),
		wantErr: "unexpected revocation aux sig entry for HTLC " +
			"index 2",
	}}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			aliceChannel, _, bobRevocation, aliceSigner :=
				setupRevocationAuxSigTest(
					t, &fakeRevocationAuxSigner{},
				)

			tc.mutate(bobRevocation, aliceSigner)

			tailBefore := aliceChannel.commitChains.Remote.
				tail().height

			aliceChannel.auxSigner = fn.Some[AuxSigner](aliceSigner)
			_, _, err := aliceChannel.ReceiveRevocation(
				bobRevocation,
			)
			require.ErrorContains(t, err, tc.wantErr)

			// The revocation must not have been accepted.
			require.Equal(
				t, tailBefore,
				aliceChannel.commitChains.Remote.tail().height,
			)
		})
	}
}

// TestRevocationAuxSigsBtcOnlyHtlcs asserts the lockstep-entry semantics for
// HTLCs that need no aux signature (BTC-only HTLCs on a custom channel): the
// revoking party still attaches one entry per non-dust HTLC, just without
// signature data. The receiver still hands the HTLC to its aux signer, which
// is the one to know that no sig is owed for it, and accepts the revocation
// when the signer agrees. This is what lets the receiver tell a sig-less
// HTLC apart from a withheld signature.
func TestRevocationAuxSigsBtcOnlyHtlcs(t *testing.T) {
	t.Parallel()

	bobSigner := &fakeRevocationAuxSigner{noneSigs: true}
	aliceChannel, _, bobRevocation, aliceSigner :=
		setupRevocationAuxSigTest(t, bobSigner)

	// The non-dust HTLC was still offered to the signer.
	require.Len(t, bobSigner.signJobs, 1)

	// The RevokeAndAck carries a blob with a sig-less entry for the
	// non-dust HTLC.
	recType := uint64(lnwire.MinCustomRecordsTlvType)
	blob := bobRevocation.CustomRecords[recType]
	require.NotEmpty(t, blob, "lockstep entry blob missing")

	sigMap, err := unpackRevocationAuxSigs(blob)
	require.NoError(t, err)
	require.Len(t, sigMap, 1)

	htlcKey := revocationAuxSigKey{signerIncoming: true, htlcIndex: 0}
	entry, ok := sigMap[htlcKey]
	require.True(t, ok)
	require.Empty(t, entry.sig)

	// Alice's aux signer is asked about the HTLC, with no sig attached,
	// and has no objection.
	revokedHeight := aliceChannel.commitChains.Remote.tail().height
	aliceChannel.auxSigner = fn.Some[AuxSigner](aliceSigner)
	_, _, err = aliceChannel.ReceiveRevocation(bobRevocation)
	require.NoError(t, err)

	require.Len(t, aliceSigner.verifyJobs, 1)
	require.True(t, aliceSigner.verifyJobs[0].SigBlob.IsNone())

	// Nothing is stored for the HTLC.
	require.Equal(t, map[revocationAuxSigKey][]byte{
		htlcKey: nil,
	}, revokedHtlcAuxSigs(t, aliceChannel, revokedHeight))
}

// TestRevocationAuxSigsPeerRecordNotPersisted asserts that a custom record
// the peer put on its own UpdateAddHTLC under the type we store verified
// revocation aux sigs with never reaches the revocation log. The breach code
// reads that type back as a sig we verified, so a record the peer chose
// itself must not survive there, whether or not the revocation carries a sig
// for the HTLC.
func TestRevocationAuxSigsPeerRecordNotPersisted(t *testing.T) {
	t.Parallel()

	sigType := uint64(revocationAuxSigType.TypeVal())

	testCases := []struct {
		name     string
		noneSigs bool
		want     []byte
	}{{
		name: "replaced by the verified sig",
		want: fakeAuxSigFor(false, 0),
	}, {
		name:     "dropped when the revocation carries no sig",
		noneSigs: true,
	}}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			aliceChannel, bobChannel, adder :=
				newRevocationAuxSigChannels(t)

			// Bob offers an HTLC and plants a record of our
			// storage type on it.
			preimage := bytes.Repeat([]byte{9}, 32)
			htlc := &lnwire.UpdateAddHTLC{
				PaymentHash: sha256.Sum256(preimage),
				Amount: lnwire.NewMSatFromSatoshis(
					revAuxNonDustAmt,
				),
				Expiry: uint32(10),
				CustomRecords: lnwire.CustomRecords{
					sigType: []byte{0xba, 0xad},
				},
			}
			htlcIndex, err := bobChannel.AddHTLC(htlc, nil)
			require.NoError(t, err)
			htlc.ID = htlcIndex
			_, err = aliceChannel.ReceiveHTLC(htlc)
			require.NoError(t, err)

			require.NoError(
				t,
				ForceStateTransition(bobChannel, aliceChannel),
			)
			require.NoError(
				t,
				ForceStateTransition(aliceChannel, bobChannel),
			)

			// Bob revokes the commitment carrying the HTLC.
			adder.fromAlice(revAuxNonDustAmt)
			signRevokedCommitment(t, aliceChannel, bobChannel)

			bobChannel.auxSigner = fn.Some[AuxSigner](
				&fakeRevocationAuxSigner{noneSigs: tc.noneSigs},
			)
			bobRevocation, _, _, err :=
				bobChannel.RevokeCurrentCommitment()
			require.NoError(t, err)

			revokedHeight := aliceChannel.commitChains.Remote.
				tail().height
			aliceChannel.auxSigner = fn.Some[AuxSigner](
				&fakeRevocationAuxSigner{},
			)
			_, _, err = aliceChannel.ReceiveRevocation(
				bobRevocation,
			)
			require.NoError(t, err)

			sigs := revokedHtlcAuxSigs(
				t, aliceChannel, revokedHeight,
			)
			require.Equal(t, tc.want, sigs[revocationAuxSigKey{
				signerIncoming: false,
				htlcIndex:      htlcIndex,
			}])
		})
	}
}

// TestRevocationAuxSigsRetransmission asserts that a retransmitted
// RevokeAndAck carries the exact same revocation aux sigs as the original:
// the packed blob is persisted atomically with the commitment advance and
// survives a restart, so a peer that never processed the original still
// receives the sigs it is owed on reestablish, and accepts them.
func TestRevocationAuxSigsRetransmission(t *testing.T) {
	t.Parallel()

	bobSigner := &fakeRevocationAuxSigner{}
	aliceChannel, bobChannel, bobRevocation, aliceSigner :=
		setupRevocationAuxSigTest(t, bobSigner)

	// Sanity check: Bob's original RevokeAndAck carries the sigs.
	require.NotEmpty(t, bobRevocation.CustomRecords)

	// Alice never processes the revocation. On reconnect, her channel
	// reestablish message tells Bob her view of his commitment tail is
	// one behind, so Bob owes her the revocation again.
	aliceSyncMsg, err := aliceChannel.channelState.ChanSyncMsg()
	require.NoError(t, err)

	// Bob restarts: his channel is rebuilt from what is on disk, with a
	// fresh signer. The retransmission must come from the persisted
	// blob, not from signing the revoked commitment again.
	bobState := bobChannel.channelState
	bobState.RevocationAuxSigs = fn.None[tlv.Blob]()
	require.NoError(t, bobState.Refresh())
	require.True(
		t, bobState.RevocationAuxSigs.IsSome(),
		"revocation aux sigs not persisted to disk",
	)

	restartedSigner := &fakeRevocationAuxSigner{}
	restartedBob, err := NewLightningChannel(
		bobChannel.Signer, bobState, bobChannel.sigPool,
		WithLeafStore(&MockAuxLeafStore{}),
		WithAuxSigner(restartedSigner),
	)
	require.NoError(t, err)

	bobSyncMsg, err := bobState.ChanSyncMsg()
	require.NoError(t, err)

	// Simulate the link/peer binding the generated musig nonces on
	// reconnect.
	aliceChannel.pendingVerificationNonce = &musig2.Nonces{
		PubNonce: aliceSyncMsg.LocalNonce.UnwrapOrFail(t).Val,
	}
	restartedBob.pendingVerificationNonce = &musig2.Nonces{
		PubNonce: bobSyncMsg.LocalNonce.UnwrapOrFail(t).Val,
	}

	// Bob processes Alice's reestablish and must retransmit a single
	// RevokeAndAck carrying the exact same aux sig records.
	bobMsgs, _, _, err := restartedBob.ProcessChanSyncMsg(
		t.Context(), aliceSyncMsg,
	)
	require.NoError(t, err)

	// Bob may also owe Alice a CommitSig for the pending state
	// transition; the RevokeAndAck retransmission is what we care about
	// here.
	var reRevoke *lnwire.RevokeAndAck
	for _, msg := range bobMsgs {
		if rev, ok := msg.(*lnwire.RevokeAndAck); ok {
			reRevoke = rev
		}
	}
	require.NotNil(t, reRevoke, "expected RevokeAndAck retransmission")
	require.Equal(
		t, bobRevocation.CustomRecords, reRevoke.CustomRecords,
		"retransmitted RevokeAndAck must carry the original aux sigs",
	)

	// Whatever the restarted signer was asked to sign is a CommitSig for
	// Alice's commitment, never the HTLCs of Bob's own revoked one.
	for _, job := range restartedSigner.signJobs {
		require.Equal(t, lntypes.Remote, job.WhoseCommit)
	}

	// Alice verifies the retransmitted sigs like the original ones and
	// accepts the revocation.
	revokedHeight := aliceChannel.commitChains.Remote.tail().height
	aliceSigner.strict = true
	aliceChannel.auxSigner = fn.Some[AuxSigner](aliceSigner)
	_, _, err = aliceChannel.ReceiveRevocation(reRevoke)
	require.NoError(t, err)
	require.Len(t, aliceSigner.verifyJobs, 1)

	htlcKey := revocationAuxSigKey{signerIncoming: true, htlcIndex: 0}
	require.Equal(t, map[revocationAuxSigKey][]byte{
		htlcKey: fakeAuxSigFor(true, 0),
	}, revokedHtlcAuxSigs(t, aliceChannel, revokedHeight))
}

// mutatePackedEntries unpacks the aux sig blob carried by the given
// RevokeAndAck, applies f to the entries (sorted by direction, then HTLC
// index), and packs the result back into the message.
func mutatePackedEntries(t *testing.T, rev *lnwire.RevokeAndAck,
	f func([]revocationAuxSigEntry) []revocationAuxSigEntry) {

	t.Helper()

	recType := uint64(lnwire.MinCustomRecordsTlvType)
	sigMap, err := unpackRevocationAuxSigs(rev.CustomRecords[recType])
	require.NoError(t, err)

	entries := make([]revocationAuxSigEntry, 0, len(sigMap))
	for _, entry := range sigMap {
		entries = append(entries, entry)
	}
	slices.SortFunc(entries, func(i, j revocationAuxSigEntry) int {
		if i.signerIncoming != j.signerIncoming {
			if i.signerIncoming {
				return 1
			}

			return -1
		}

		return cmp.Compare(i.htlcIndex, j.htlcIndex)
	})

	packed, err := packRevocationAuxSigs(f(entries))
	require.NoError(t, err)

	rev.CustomRecords[recType] = packed
}

// TestRevocationAuxSigsSignFailure asserts that a failure of the aux signer
// at revocation time fails RevokeCurrentCommitment as a whole, BEFORE any
// state is advanced or persisted: sending a sig-less RevokeAndAck would only
// fail the channel on the peer's side.
func TestRevocationAuxSigsSignFailure(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name   string
		signer *fakeRevocationAuxSigner
	}{{
		name: "batch submission fails",
		signer: &fakeRevocationAuxSigner{
			signErr: fmt.Errorf("signer offline"),
		},
	}, {
		name: "sign job fails",
		signer: &fakeRevocationAuxSigner{
			respErr: fmt.Errorf("cannot sign"),
		},
	}}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, bobChannel := setupRevocationAuxSigChannels(t)
			bobChannel.auxSigner = fn.Some[AuxSigner](tc.signer)

			tailBefore := bobChannel.commitChains.Local.
				tail().height
			heightBefore := bobChannel.currentHeight

			_, _, _, err := bobChannel.RevokeCurrentCommitment()
			require.ErrorContains(
				t, err, "unable to sign local HTLC aux sigs",
			)

			// Nothing advanced: the revocation can be retried.
			require.Equal(
				t, tailBefore,
				bobChannel.commitChains.Local.tail().height,
			)
			require.Equal(
				t, heightBefore, bobChannel.currentHeight,
			)
		})
	}
}

// revAuxSigBlob assembles a raw revocation aux sig blob from an entry count
// and already framed entries, bypassing the encoder so tests can build blobs
// an honest peer never would.
func revAuxSigBlob(t *testing.T, count uint64, framed ...[]byte) []byte {
	t.Helper()

	var (
		buf     bytes.Buffer
		scratch [8]byte
	)
	require.NoError(t, tlv.WriteVarInt(&buf, count, &scratch))
	for _, entry := range framed {
		buf.Write(entry)
	}

	return buf.Bytes()
}

// framedRevAuxSigEntry returns the length-prefixed encoding of an entry, the
// way it sits in a blob.
func framedRevAuxSigEntry(t *testing.T, entry revocationAuxSigEntry) []byte {
	t.Helper()

	var inner bytes.Buffer
	require.NoError(t, entry.encode(&inner))

	return framedRevAuxSigBytes(t, inner.Bytes())
}

// framedRevAuxSigBytes length-prefixes raw entry bytes.
func framedRevAuxSigBytes(t *testing.T, entry []byte) []byte {
	t.Helper()

	var (
		buf     bytes.Buffer
		scratch [8]byte
	)
	require.NoError(
		t, tlv.WriteVarInt(&buf, uint64(len(entry)), &scratch),
	)
	buf.Write(entry)

	return buf.Bytes()
}

// TestUnpackRevocationAuxSigsCorrupt asserts that corrupt and hostile packed
// blobs are rejected by the decoder rather than yielding partial results.
func TestUnpackRevocationAuxSigsCorrupt(t *testing.T) {
	t.Parallel()

	first := newRevAuxSigEntry(true, 0, fakeAuxSigFor(true, 0))
	second := newRevAuxSigEntry(true, 1, fakeAuxSigFor(true, 1))

	valid, err := packRevocationAuxSigs(
		[]revocationAuxSigEntry{first, second},
	)
	require.NoError(t, err)

	// An entry stream with a single record of the given type and value.
	record := func(typ tlv.Type, val []byte) []byte {
		return append([]byte{byte(typ), byte(len(val))}, val...)
	}
	indexRecord := record(revAuxSigIndexType, make([]byte, 8))
	incomingRecord := record(revAuxSigIncomingType, []byte{1})

	testCases := []struct {
		name    string
		blob    []byte
		wantErr string
	}{{
		name: "truncated mid-entry",
		blob: valid[:len(valid)-3],
	}, {
		name: "truncated to first entry",
		blob: valid[:len(valid)/2],
	}, {
		name: "garbage",
		blob: []byte{0xde, 0xad, 0xbe, 0xef},
	}, {
		// Two entries for the same HTLC: the second must not
		// silently replace the first.
		name: "duplicate entry",
		blob: revAuxSigBlob(
			t, 2, framedRevAuxSigEntry(t, first),
			framedRevAuxSigEntry(t, first),
		),
		wantErr: "duplicate entry for HTLC index 0",
	}, {
		name:    "trailing bytes",
		blob:    append(slices.Clone(valid), 0x00),
		wantErr: "1 trailing bytes after the last entry",
	}, {
		// Fewer entries declared than present leaves a whole entry
		// behind as trailing data.
		name: "undeclared entry",
		blob: revAuxSigBlob(
			t, 1, framedRevAuxSigEntry(t, first),
			framedRevAuxSigEntry(t, second),
		),
		wantErr: "trailing bytes after the last entry",
	}, {
		name:    "count beyond the blob",
		blob:    revAuxSigBlob(t, 3, framedRevAuxSigEntry(t, first)),
		wantErr: "read entry 1 length",
	}, {
		name:    "entry length beyond the blob",
		blob:    append(revAuxSigBlob(t, 1), 0x40, 0x00),
		wantErr: "entry 0 length 64 exceeds the 1 bytes left",
	}, {
		// The entry's declared length covers more than its records,
		// so the stream ends in a record that is cut short.
		name: "entry with a cut-off record",
		blob: revAuxSigBlob(t, 1, framedRevAuxSigBytes(
			t, append(
				append(slices.Clone(indexRecord),
					incomingRecord...,
				), 0x05, 0x04, 0x00,
			),
		)),
		wantErr: "decode entry 0",
	}, {
		name: "entry without an HTLC index",
		blob: revAuxSigBlob(
			t, 1, framedRevAuxSigBytes(t, incomingRecord),
		),
		wantErr: "missing HTLC index",
	}, {
		name: "entry without a direction",
		blob: revAuxSigBlob(
			t, 1, framedRevAuxSigBytes(t, indexRecord),
		),
		wantErr: "missing HTLC direction",
	}, {
		name: "entry with an invalid direction",
		blob: revAuxSigBlob(t, 1, framedRevAuxSigBytes(
			t, append(
				slices.Clone(indexRecord),
				record(revAuxSigIncomingType, []byte{2})...,
			),
		)),
		wantErr: "invalid HTLC direction 2",
	}}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := unpackRevocationAuxSigs(tc.blob)
			require.Error(t, err)
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}

// TestUnpackRevocationAuxSigsHugeCount asserts that the entry count a peer
// declares is never used to size an allocation. The blob is a handful of
// bytes long; a count of billions must be rejected without the decoder ever
// reserving memory for that many entries, which would take the node down.
func TestUnpackRevocationAuxSigsHugeCount(t *testing.T) {
	// Not parallel: the test measures this goroutine's allocations.
	for _, count := range []uint64{1 << 22, 1 << 32, math.MaxUint64} {
		blob := revAuxSigBlob(t, count)
		require.LessOrEqual(t, len(blob), 9)

		var unpackErr error
		allocated := testing.AllocsPerRun(1, func() {
			_, unpackErr = unpackRevocationAuxSigs(blob)
		})
		require.ErrorContains(t, unpackErr, "exceeds the 0 bytes")

		// A map sized for the declared count would be a single
		// allocation as well, so bound the bytes, not the count.
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		_, _ = unpackRevocationAuxSigs(blob)
		runtime.ReadMemStats(&after)

		require.Less(t, allocated, float64(32))
		require.Less(
			t, after.TotalAlloc-before.TotalAlloc,
			uint64(1<<20), "decoding a %d byte blob allocated "+
				"memory for its declared entry count",
			len(blob),
		)
	}
}

// TestRevocationAuxSigsGateFollowsRevokedCommitment asserts that whether a
// revocation carries aux sigs is decided by the sighash recorded for the
// commitment being revoked, not by the one new commitments use. While the
// sighash type of a channel is switching, the two differ, and both peers
// must still agree on which revocations carry the signatures.
func TestRevocationAuxSigsGateFollowsRevokedCommitment(t *testing.T) {
	t.Parallel()

	channel, signer := newLiveAuxSignerChannel(t)

	legacyCommit := &commitment{}
	deterministicCommit := &commitment{
		customBlob: fn.Some(sigHashDefaultBlob),
	}

	// New commitments use the legacy sighash, yet revoking a commitment
	// that was built with SigHashDefault still exchanges the sigs.
	require.False(t, channel.IsChanSigHashDefault())
	require.False(t, channel.exchangesRevocationAuxSigs(legacyCommit))
	require.True(
		t, channel.exchangesRevocationAuxSigs(deterministicCommit),
	)

	// New commitments switch to SigHashDefault. Revoking a commitment
	// that predates the switch must not start exchanging the sigs: its
	// second-level HTLCs are not deterministic and the peer does not
	// expect any.
	signer.live = fn.Some(txscript.SigHashDefault)
	channel.RefreshHtlcSigHashType()
	require.True(t, channel.IsChanSigHashDefault())
	require.False(t, channel.exchangesRevocationAuxSigs(legacyCommit))
	require.True(
		t, channel.exchangesRevocationAuxSigs(deterministicCommit),
	)

	// Without a commitment or an aux signer nothing is exchanged.
	require.False(t, channel.exchangesRevocationAuxSigs(nil))
	channel.auxSigner = fn.None[AuxSigner]()
	require.False(
		t, channel.exchangesRevocationAuxSigs(deterministicCommit),
	)
}
