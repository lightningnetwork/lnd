package lnwallet

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"fmt"
	"math"
	"slices"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

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
				{
					htlcIndex:  42,
					primarySig: []byte{0xaa, 0xbb, 0xcc},
					altSig:     []byte{0xdd, 0xee, 0xff},
				},
			},
		},
		{
			name: "multiple entries with various HTLC indices",
			entries: []revocationAuxSigEntry{
				{
					htlcIndex:  0,
					primarySig: []byte{0x01},
					altSig:     []byte{0x02, 0x03},
				},
				{
					htlcIndex:  100,
					primarySig: []byte{0x04, 0x05, 0x06},
					altSig:     []byte{0x07},
				},
				{
					htlcIndex:  999,
					primarySig: []byte{0x08, 0x09},
					altSig: []byte{
						0x0a, 0x0b, 0x0c, 0x0d,
					},
				},
			},
		},
		{
			name: "entry with empty primary and alt sigs",
			entries: []revocationAuxSigEntry{
				{
					htlcIndex:  7,
					primarySig: []byte{},
					altSig:     []byte{},
				},
			},
		},
		{
			name: "max uint64 HTLC index",
			entries: []revocationAuxSigEntry{
				{
					htlcIndex:  math.MaxUint64,
					primarySig: []byte{0xde, 0xad},
					altSig:     []byte{0xbe, 0xef},
				},
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
				got, ok := sigMap[entry.htlcIndex]
				require.True(t, ok, "missing HTLC index %d",
					entry.htlcIndex)

				require.Equal(t, entry.htlcIndex, got.htlcIndex)
				require.Equal(
					t, entry.primarySig,
					got.primarySig,
				)
				require.Equal(t, entry.altSig, got.altSig)
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

// Canned signature blobs handed out by fakeRevocationAuxSigner, distinct per
// spend path so tests can assert that primary and alternate sigs are never
// mixed up between packing, wire transfer, verification and persistence.
var (
	fakePrimarySigBlob = []byte{0x01, 0xaa, 0xbb, 0xcc}
	fakeAltSigBlob     = []byte{0x02, 0xdd, 0xee, 0xff}
)

// fakeRevocationAuxSigner is a minimal AuxSigner used to exercise the
// revocation AuxSig paths end to end on a custom channel. It negotiates
// SigHashDefault, answers every sign job with a canned signature blob
// (distinct for the primary and alternate spend paths), and records the
// sign and verify jobs it processes.
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

		blob := fakePrimarySigBlob
		if job.Incoming != job.IncomingHTLCLookup {
			blob = fakeAltSigBlob
		}
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

	return s.verifyErr
}

func (s *fakeRevocationAuxSigner) HtlcSigHashType(
	_ HtlcSigHashReq) fn.Option[txscript.SigHashType] {

	return fn.Some(txscript.SigHashDefault)
}

// setupRevocationAuxSigTest creates a custom (tapscript root) channel pair,
// locks in one non-dust HTLC (index 0) and one dust HTLC (index 1) from
// Alice to Bob, then triggers a second state transition (a further HTLC add)
// so that Bob revokes a commitment actually carrying those HTLCs. Bob's fake
// aux signer is attached right before his revocation, so the sigs it
// produces end up in the returned RevokeAndAck. Alice's fake signer is
// returned unattached: callers attach it (after optionally configuring the
// verify error) right before feeding the revocation to Alice.
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
// DeterministicHTLCs (SigHashDefault) at construction, by setting the
// channel's construction-time resolved sighash type and the commitment
// builder's flag to the values NewLightningChannel would have derived.
func markChanSigHashDefault(c *LightningChannel) {
	c.htlcSigHashType = txscript.SigHashDefault
	c.commitBuilder.sigHashDefault = true
}

// setupRevocationAuxSigChannels creates the custom channel pair and runs the
// commitment dance right up to (but not including) Bob's revocation of the
// commitment carrying HTLC 0 (non-dust) and HTLC 1 (dust). No fake
// revocation aux signer is attached yet.
func setupRevocationAuxSigChannels(t *testing.T) (*LightningChannel,
	*LightningChannel) {

	t.Helper()

	chanType := channeldb.SimpleTaprootFeatureBit |
		channeldb.TapscriptRootBit

	aliceChannel, bobChannel, err := CreateTestChannels(t, chanType)
	require.NoError(t, err, "unable to create test channels")

	// Emulate channels whose aux signer negotiated DeterministicHTLCs at
	// construction: the cached resolved sighash type and the commitment
	// builder flag are set to exactly what NewLightningChannel would have
	// derived had a SigHashDefault-negotiating signer been attached from
	// the start. The sighash type is resolved exactly once at channel
	// construction, so a late signer swap (as done below for the fake
	// revocation signers) deliberately cannot influence it.
	markChanSigHashDefault(aliceChannel)
	markChanSigHashDefault(bobChannel)

	var nextHtlcID uint64
	addHtlc := func(id byte, amtSat btcutil.Amount) {
		preimage := bytes.Repeat([]byte{id}, 32)
		hash := sha256.Sum256(preimage)
		htlc := &lnwire.UpdateAddHTLC{
			ID:          nextHtlcID,
			PaymentHash: hash,
			Amount:      lnwire.NewMSatFromSatoshis(amtSat),
			Expiry:      uint32(10),
		}
		nextHtlcID++
		_, err := aliceChannel.AddHTLC(htlc, nil)
		require.NoError(t, err)
		_, err = bobChannel.ReceiveHTLC(htlc)
		require.NoError(t, err)
	}

	// HTLC index 0: well above dust. HTLC index 1: below the dust
	// threshold once the second-level fee and anchor are accounted for.
	addHtlc(1, 100_000)
	addHtlc(2, 400)

	// Lock both HTLCs into both commitments. No aux signer is attached
	// yet, so the CommitSig-time aux machinery stays out of the picture.
	require.NoError(t, ForceStateTransition(aliceChannel, bobChannel))

	// A further add triggers the state transition whose revocation we
	// care about: Bob will revoke the commitment carrying HTLCs 0 and 1.
	addHtlc(3, 100_000)

	aliceNewCommit, err := aliceChannel.SignNextCommitment(t.Context())
	require.NoError(t, err)
	err = bobChannel.ReceiveNewCommitment(aliceNewCommit.CommitSigs)
	require.NoError(t, err)

	return aliceChannel, bobChannel
}

// TestRevocationAuxSigsRoundTrip exercises the full happy path on a custom
// channel: Bob's revocation signs both spend paths of every non-dust HTLC on
// the revoked commitment, ships them in RevokeAndAck.CustomRecords, and
// Alice verifies them before accepting the revocation and persists them into
// the remote commitment's HTLC custom records. Dust HTLCs are excluded on
// both sides.
func TestRevocationAuxSigsRoundTrip(t *testing.T) {
	t.Parallel()

	bobSigner := &fakeRevocationAuxSigner{}
	aliceChannel, _, bobRevocation, aliceSigner :=
		setupRevocationAuxSigTest(t, bobSigner)

	// The remote chain tail is the commitment Bob is revoking; its height
	// keys the revocation log entry we'll inspect below.
	revokedHeight := aliceChannel.commitChains.Remote.tail().height

	// Bob signed exactly one non-dust HTLC, two paths: the dust HTLC
	// must not have produced sign jobs.
	require.Len(t, bobSigner.signJobs, 2,
		"expected 2 sign jobs (1 non-dust HTLC x 2 paths)")

	// The RevokeAndAck carries the packed sigs for HTLC index 0 only.
	recType := uint64(lnwire.MinCustomRecordsTlvType)
	blob := bobRevocation.CustomRecords[recType]
	require.NotEmpty(t, blob, "revocation aux sig blob missing")

	sigMap, err := unpackRevocationAuxSigs(blob)
	require.NoError(t, err)
	require.Len(t, sigMap, 1)

	entry, ok := sigMap[0]
	require.True(t, ok, "expected sigs for HTLC index 0")
	require.Equal(t, fakePrimarySigBlob, entry.primarySig)
	require.Equal(t, fakeAltSigBlob, entry.altSig)

	// Alice verifies and accepts.
	aliceChannel.auxSigner = fn.Some[AuxSigner](aliceSigner)
	_, _, err = aliceChannel.ReceiveRevocation(bobRevocation)
	require.NoError(t, err)

	// Alice verified both paths of the non-dust HTLC, with the right
	// blob matched to the right path.
	require.Len(t, aliceSigner.verifyJobs, 2,
		"expected 2 verify jobs (1 non-dust HTLC x 2 paths)")
	for _, job := range aliceSigner.verifyJobs {
		want := fakePrimarySigBlob
		if job.Incoming != job.IncomingHTLCLookup {
			want = fakeAltSigBlob
		}
		require.Equal(t, want, job.SigBlob.UnwrapOr(nil))
	}

	// The verified sigs are persisted in the revocation log entry of the
	// revoked commitment (the breach-time source of truth); the dust
	// HTLC's entry carries none.
	primaryType := uint64(revocationAuxSigType.TypeVal())
	altType := uint64(revocationAuxSigAltType.TypeVal())

	revokedLog, _, err := aliceChannel.channelState.FindPreviousState(
		revokedHeight,
	)
	require.NoError(t, err)

	var found bool
	for _, htlcEntry := range revokedLog.HTLCEntries {
		switch htlcEntry.Amt.Val.Int() {
		// The non-dust HTLC.
		case 100_000:
			found = true

			blob := htlcEntry.CustomBlob.ValOpt().UnwrapOr(nil)
			require.NotEmpty(t, blob)

			records, err := lnwire.ParseCustomRecords(blob)
			require.NoError(t, err)
			require.Equal(
				t, fakePrimarySigBlob, records[primaryType],
			)
			require.Equal(t, fakeAltSigBlob, records[altType])

		// The dust HTLC.
		case 400:
			require.True(
				t, htlcEntry.CustomBlob.ValOpt().IsNone(),
				"dust HTLC must carry no revocation aux sigs",
			)
		}
	}
	require.True(t, found, "non-dust HTLC not found in revocation log")
}

// TestRevocationAuxSigsRejected asserts that ReceiveRevocation rejects the
// revocation BEFORE advancing the remote chain tail whenever the received
// aux sigs are unverifiable, malformed, or missing for a non-dust HTLC.
func TestRevocationAuxSigsRejected(t *testing.T) {
	t.Parallel()

	recType := uint64(lnwire.MinCustomRecordsTlvType)

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

			// A well-formed blob whose sigs are tagged with an
			// HTLC index that isn't on the commitment.
			packed, err := packRevocationAuxSigs(
				[]revocationAuxSigEntry{{
					htlcIndex:  99,
					primarySig: fakePrimarySigBlob,
					altSig:     fakeAltSigBlob,
				}},
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
		name: "extra entry for unknown HTLC index",
		mutate: func(rev *lnwire.RevokeAndAck,
			_ *fakeRevocationAuxSigner) {

			mutatePackedEntries(t, rev,
				func(entries []revocationAuxSigEntry,
				) []revocationAuxSigEntry {

					return append(
						entries,
						revocationAuxSigEntry{
							htlcIndex: 99,
							primarySig: []byte{
								0x99,
							},
							altSig: []byte{0x99},
						},
					)
				},
			)
		},
		wantErr: "unexpected revocation aux sig entry for HTLC " +
			"index 99",
	}, {
		name: "entry for dust HTLC",
		mutate: func(rev *lnwire.RevokeAndAck,
			_ *fakeRevocationAuxSigner) {

			// HTLC index 1 is the dust HTLC: an honest signer
			// never packs an entry for it.
			mutatePackedEntries(t, rev,
				func(entries []revocationAuxSigEntry,
				) []revocationAuxSigEntry {

					return append(
						entries,
						revocationAuxSigEntry{
							htlcIndex: 1,
							primarySig: []byte{
								0x01,
							},
							altSig: []byte{0x01},
						},
					)
				},
			)
		},
		wantErr: "unexpected revocation aux sig entry for HTLC " +
			"index 1",
	}, {
		name: "one-sided entry",
		mutate: func(rev *lnwire.RevokeAndAck,
			_ *fakeRevocationAuxSigner) {

			// Withhold just the alternate path sig of the
			// otherwise valid entry.
			mutatePackedEntries(t, rev,
				func(entries []revocationAuxSigEntry,
				) []revocationAuxSigEntry {

					entries[0].altSig = nil

					return entries
				},
			)
		},
		wantErr: "carries only one spend path",
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
// signature data, and the receiver accepts the revocation without running
// any verification jobs. This is what lets the receiver tell a sig-less HTLC
// apart from a withheld signature.
func TestRevocationAuxSigsBtcOnlyHtlcs(t *testing.T) {
	t.Parallel()

	bobSigner := &fakeRevocationAuxSigner{noneSigs: true}
	aliceChannel, _, bobRevocation, aliceSigner :=
		setupRevocationAuxSigTest(t, bobSigner)

	// Both spend paths of the non-dust HTLC were still offered to the
	// signer.
	require.Len(t, bobSigner.signJobs, 2)

	// The RevokeAndAck carries a blob with a sig-less entry for the
	// non-dust HTLC.
	recType := uint64(lnwire.MinCustomRecordsTlvType)
	blob := bobRevocation.CustomRecords[recType]
	require.NotEmpty(t, blob, "lockstep entry blob missing")

	sigMap, err := unpackRevocationAuxSigs(blob)
	require.NoError(t, err)
	require.Len(t, sigMap, 1)

	entry, ok := sigMap[0]
	require.True(t, ok)
	require.Empty(t, entry.primarySig)
	require.Empty(t, entry.altSig)

	// Alice accepts the revocation without any verification jobs.
	aliceChannel.auxSigner = fn.Some[AuxSigner](aliceSigner)
	_, _, err = aliceChannel.ReceiveRevocation(bobRevocation)
	require.NoError(t, err)
	require.Empty(t, aliceSigner.verifyJobs)
}

// TestRevocationAuxSigsRetransmission asserts that a retransmitted
// RevokeAndAck carries the exact same revocation aux sigs as the original:
// the packed blob is persisted atomically with the commitment advance and
// survives a restart, so a peer that never processed the original still
// receives the sigs it is owed on reestablish.
func TestRevocationAuxSigsRetransmission(t *testing.T) {
	t.Parallel()

	bobSigner := &fakeRevocationAuxSigner{}
	aliceChannel, bobChannel, bobRevocation, _ :=
		setupRevocationAuxSigTest(t, bobSigner)

	// Sanity check: Bob's original RevokeAndAck carries the sigs.
	require.NotEmpty(t, bobRevocation.CustomRecords)

	// Alice never processes the revocation. On reconnect, her channel
	// reestablish message tells Bob her view of his commitment tail is
	// one behind, so Bob owes her the revocation again.
	aliceSyncMsg, err := aliceChannel.channelState.ChanSyncMsg()
	require.NoError(t, err)
	bobSyncMsg, err := bobChannel.channelState.ChanSyncMsg()
	require.NoError(t, err)

	// Simulate the link/peer binding the generated musig nonces on
	// reconnect.
	aliceChannel.pendingVerificationNonce = &musig2.Nonces{
		PubNonce: aliceSyncMsg.LocalNonce.UnwrapOrFail(t).Val,
	}
	bobChannel.pendingVerificationNonce = &musig2.Nonces{
		PubNonce: bobSyncMsg.LocalNonce.UnwrapOrFail(t).Val,
	}

	// Simulate a restart on Bob's side: wipe the in-memory copy of the
	// persisted sigs and reload the channel state from disk.
	bobChannel.channelState.RevocationAuxSigs = fn.None[tlv.Blob]()
	require.NoError(t, bobChannel.channelState.Refresh())
	require.True(
		t, bobChannel.channelState.RevocationAuxSigs.IsSome(),
		"revocation aux sigs not persisted to disk",
	)

	// Bob processes Alice's reestablish and must retransmit a single
	// RevokeAndAck carrying the exact same aux sig records.
	bobMsgs, _, _, err := bobChannel.ProcessChanSyncMsg(
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
}

// mutatePackedEntries unpacks the aux sig blob carried by the given
// RevokeAndAck, applies f to the entries (sorted by HTLC index), and packs
// the result back into the message.
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

// TestUnpackRevocationAuxSigsCorrupt asserts that corrupt packed blobs are
// rejected by the decoder rather than yielding partial results.
func TestUnpackRevocationAuxSigsCorrupt(t *testing.T) {
	t.Parallel()

	valid, err := packRevocationAuxSigs([]revocationAuxSigEntry{{
		htlcIndex:  0,
		primarySig: fakePrimarySigBlob,
		altSig:     fakeAltSigBlob,
	}, {
		htlcIndex:  1,
		primarySig: fakePrimarySigBlob,
		altSig:     fakeAltSigBlob,
	}})
	require.NoError(t, err)

	testCases := []struct {
		name string
		blob []byte
	}{{
		name: "truncated mid-entry",
		blob: valid[:len(valid)-3],
	}, {
		name: "truncated to first entry",
		blob: valid[:len(valid)/2],
	}, {
		name: "garbage",
		blob: []byte{0xde, 0xad, 0xbe, 0xef},
	}}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := unpackRevocationAuxSigs(tc.blob)
			require.Error(t, err)
		})
	}
}
