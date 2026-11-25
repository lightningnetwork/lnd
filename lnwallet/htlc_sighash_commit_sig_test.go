package lnwallet

import (
	"bytes"
	"crypto/sha256"
	"testing"

	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// legacySigHashBlob is the commitment blob the sigHashLeafStore records for a
// commitment built with the legacy HTLC sighash. Any blob other than
// sigHashDefaultBlob reads as legacy, this one just makes the intent explicit.
var legacySigHashBlob = tlv.Blob("sighash-legacy")

// sigHashLeafStore is an AuxLeafStore test double that does what a real leaf
// store must do with the sighash mode lnd hands it: record it in the blob of
// the new commitment. It also keeps the modes it was asked for, so a test can
// assert that both calls that build a commitment were given the same one.
type sigHashLeafStore struct {
	MockAuxLeafStore

	viewModes []bool
	blobModes []bool
}

// FetchLeavesFromView records the mode the leaves were asked for.
func (s *sigHashLeafStore) FetchLeavesFromView(
	in CommitDiffAuxInput) fn.Result[CommitDiffAuxResult] {

	s.viewModes = append(s.viewModes, in.SigHashDefault)

	return fn.Ok(CommitDiffAuxResult{})
}

// ApplyHtlcView returns the blob that records the given mode.
func (s *sigHashLeafStore) ApplyHtlcView(
	in CommitDiffAuxInput) fn.Result[fn.Option[tlv.Blob]] {

	s.blobModes = append(s.blobModes, in.SigHashDefault)

	if in.SigHashDefault {
		return fn.Ok(fn.Some(sigHashDefaultBlob))
	}

	return fn.Ok(fn.Some(legacySigHashBlob))
}

// unpackRecordingSigner is a liveAuxSigner that keeps every blob it was asked
// to unpack.
type unpackRecordingSigner struct {
	*liveAuxSigner

	unpacked []tlv.Blob
}

// UnpackSigs records the blob and defers to the mock.
func (s *unpackRecordingSigner) UnpackSigs(
	blob fn.Option[tlv.Blob]) fn.Result[[]fn.Option[tlv.Blob]] {

	s.unpacked = append(s.unpacked, blob.UnwrapOr(nil))

	return s.liveAuxSigner.UnpackSigs(blob)
}

// sigHashParty is one side of a custom channel whose HTLC sighash mode is
// driven by a live feature negotiation.
type sigHashParty struct {
	channel *LightningChannel
	signer  *unpackRecordingSigner
	store   *sigHashLeafStore
}

// negotiate sets the live answer of the party's aux signer and has the
// channel pick it up, which is what happens when the channel is
// re-established.
func (p *sigHashParty) negotiate(live fn.Option[txscript.SigHashType]) {
	p.signer.live = live
	p.channel.RefreshHtlcSigHashType()
}

// tipDefault returns whether the latest commitment on the given chain of the
// party records SigHashDefault.
func (p *sigHashParty) tipDefault(chain lntypes.ChannelParty) bool {
	commitChain := p.channel.commitChains.Local
	if chain.IsRemote() {
		commitChain = p.channel.commitChains.Remote
	}

	return p.channel.commitSigHashDefault(commitChain.tip().customBlob)
}

// newSigHashParties creates a custom (tapscript root) channel between two
// parties that both start out on the legacy HTLC sighash, with the feature
// not negotiated.
func newSigHashParties(t *testing.T) (*sigHashParty, *sigHashParty) {
	t.Helper()

	chanType := channeldb.SimpleTaprootFeatureBit |
		channeldb.TapscriptRootBit

	aliceChannel, bobChannel, err := CreateTestChannels(t, chanType)
	require.NoError(t, err)

	party := func(channel *LightningChannel) *sigHashParty {
		p := &sigHashParty{
			channel: channel,
			signer: &unpackRecordingSigner{
				liveAuxSigner: &liveAuxSigner{
					MockAuxSigner: NewDefaultAuxSignerMock(
						t,
					),
				},
			},
			store: &sigHashLeafStore{},
		}

		channel.auxSigner = fn.Some[AuxSigner](p.signer)
		channel.leafStore = fn.Some[AuxLeafStore](p.store)
		channel.commitBuilder.auxLeafStore = channel.leafStore

		// The leaf store is only consulted for a commitment that
		// follows one with a blob, so the commitments the channel
		// starts with get one.
		legacy := fn.Some(legacySigHashBlob)
		channel.channelState.LocalCommitment.CustomBlob = legacy
		channel.channelState.RemoteCommitment.CustomBlob = legacy
		channel.commitChains.Local.tip().customBlob = legacy
		channel.commitChains.Remote.tip().customBlob = legacy

		channel.RefreshHtlcSigHashType()
		require.False(t, channel.IsChanSigHashDefault())

		return p
	}

	return party(aliceChannel), party(bobChannel)
}

// addSigHashHtlc adds an HTLC of the given amount from one party to the
// other, without committing to it.
func addSigHashHtlc(t *testing.T, from, to *sigHashParty,
	amt lnwire.MilliSatoshi, id byte) {

	t.Helper()

	htlc := &lnwire.UpdateAddHTLC{
		PaymentHash: sha256.Sum256(bytes.Repeat([]byte{id}, 32)),
		Amount:      amt,
		Expiry:      uint32(10),
	}
	htlcID, err := from.channel.AddHTLC(htlc, nil)
	require.NoError(t, err)
	htlc.ID = htlcID
	_, err = to.channel.ReceiveHTLC(htlc)
	require.NoError(t, err)
}

// statesSigHashDefault returns whether the custom records of a
// commitment_signed state SigHashDefault.
func statesSigHashDefault(t *testing.T, auxSigBlob tlv.Blob) bool {
	t.Helper()

	records, err := lnwire.ParseCustomRecords(auxSigBlob)
	require.NoError(t, err)

	val, ok := records[uint64(commitSigHtlcSigHashType.TypeVal())]
	if !ok {
		return false
	}
	require.Equal(t, []byte{byte(txscript.SigHashDefault)}, val)

	return true
}

// withSigHashRecord returns the given custom records blob with the HTLC
// sighash record set to the given value, or removed when the value is nil.
func withSigHashRecord(t *testing.T, auxSigBlob tlv.Blob,
	val []byte) tlv.Blob {

	t.Helper()

	records, err := lnwire.ParseCustomRecords(auxSigBlob)
	require.NoError(t, err)
	if records == nil {
		records = make(lnwire.CustomRecords)
	}

	recordType := uint64(commitSigHtlcSigHashType.TypeVal())
	delete(records, recordType)
	if val != nil {
		records[recordType] = val
	}

	blob, err := records.Serialize()
	require.NoError(t, err)

	return blob
}

// commitSigsFromMsg turns a commitment_signed message into what the link
// hands the channel when it receives one.
func commitSigsFromMsg(t *testing.T, msg *lnwire.CommitSig) *CommitSigs {
	t.Helper()

	auxSigBlob, err := msg.CustomRecords.Serialize()
	require.NoError(t, err)

	return &CommitSigs{
		CommitSig:  msg.CommitSig,
		HtlcSigs:   msg.HtlcSigs,
		PartialSig: msg.PartialSig,
		AuxSigBlob: auxSigBlob,
	}
}

// completeSigHashTransition has the receiver of a commitment revoke its old
// one and sign one for the sender in return, and the sender do the same.
func completeSigHashTransition(t *testing.T, sender, receiver *sigHashParty) {
	t.Helper()

	revocation, _, _, err := receiver.channel.RevokeCurrentCommitment()
	require.NoError(t, err)
	_, _, err = sender.channel.ReceiveRevocation(revocation)
	require.NoError(t, err)

	commit, err := receiver.channel.SignNextCommitment(ctxb)
	require.NoError(t, err)
	err = sender.channel.ReceiveNewCommitment(commit.CommitSigs)
	require.NoError(t, err)

	revocation, _, _, err = sender.channel.RevokeCurrentCommitment()
	require.NoError(t, err)
	_, _, err = receiver.channel.ReceiveRevocation(revocation)
	require.NoError(t, err)
}

// sigHashHtlcAmounts returns an HTLC amount that has an output under both
// sighash modes, and one that has an output under the legacy sighash only.
func sigHashHtlcAmounts(t *testing.T,
	channel *LightningChannel) (lnwire.MilliSatoshi, lnwire.MilliSatoshi) {

	t.Helper()

	state := channel.channelState
	feePerKw := chainfee.SatPerKWeight(state.LocalCommitment.FeePerKw)
	dustLimit := state.RemoteChanCfg.DustLimit

	between := dustLimit + 100
	for _, incoming := range []bool{true, false} {
		for _, whose := range []lntypes.ChannelParty{
			lntypes.Local, lntypes.Remote,
		} {
			require.False(t, HtlcIsDust(
				state.ChanType, incoming, whose, feePerKw,
				between, dustLimit, false,
			))
			require.True(t, HtlcIsDust(
				state.ChanType, incoming, whose, feePerKw,
				between, dustLimit, true,
			))
		}
	}

	return lnwire.NewMSatFromSatoshis(100_000),
		lnwire.NewMSatFromSatoshis(between)
}

// TestCommitSigStatesHtlcSigHash asserts that a commitment_signed states the
// HTLC sighash mode of the commitment it signs, that the receiver builds and
// verifies the commitment with the stated mode, and that both sides record
// the same mode for it. The leaf store is handed that mode for both the
// leaves and the blob, and the aux signer never sees the record.
func TestCommitSigStatesHtlcSigHash(t *testing.T) {
	t.Parallel()

	alice, bob := newSigHashParties(t)
	htlcAmt, _ := sigHashHtlcAmounts(t, alice.channel)

	// Without the feature the message is what it always was.
	addSigHashHtlc(t, alice, bob, htlcAmt, 1)
	commit, err := alice.channel.SignNextCommitment(ctxb)
	require.NoError(t, err)
	require.Len(t, commit.HtlcSigs, 1)
	require.False(t, statesSigHashDefault(t, commit.AuxSigBlob))
	require.NoError(
		t, bob.channel.ReceiveNewCommitment(commit.CommitSigs),
	)
	require.False(t, alice.tipDefault(lntypes.Remote))
	require.False(t, bob.tipDefault(lntypes.Local))
	completeSigHashTransition(t, alice, bob)

	// Every commitment so far was built as legacy, on both ends and for
	// both the leaves and the blob.
	for _, p := range []*sigHashParty{alice, bob} {
		require.NotEmpty(t, p.store.viewModes)
		require.NotContains(t, p.store.viewModes, true)
		require.NotEmpty(t, p.store.blobModes)
		require.NotContains(t, p.store.blobModes, true)

		p.store.viewModes, p.store.blobModes = nil, nil
		p.signer.unpacked = nil
	}

	// Once the feature is negotiated the next commitment uses
	// SigHashDefault and says so.
	alice.negotiate(fn.Some(txscript.SigHashDefault))
	bob.negotiate(fn.Some(txscript.SigHashDefault))

	addSigHashHtlc(t, alice, bob, htlcAmt, 2)
	commit, err = alice.channel.SignNextCommitment(ctxb)
	require.NoError(t, err)
	require.Len(t, commit.HtlcSigs, 2)
	require.True(t, statesSigHashDefault(t, commit.AuxSigBlob))
	require.NoError(
		t, bob.channel.ReceiveNewCommitment(commit.CommitSigs),
	)
	require.True(t, alice.tipDefault(lntypes.Remote))
	require.True(t, bob.tipDefault(lntypes.Local))

	// The aux signer got its own records and nothing else.
	require.Len(t, bob.signer.unpacked, 1)
	require.NotEmpty(t, bob.signer.unpacked[0])
	require.Equal(
		t, withSigHashRecord(t, commit.AuxSigBlob, nil),
		bob.signer.unpacked[0],
	)

	// The commitments Bob signs in return follow the same rule.
	completeSigHashTransition(t, alice, bob)
	require.True(t, bob.tipDefault(lntypes.Remote))
	require.True(t, alice.tipDefault(lntypes.Local))

	for _, p := range []*sigHashParty{alice, bob} {
		require.NotEmpty(t, p.store.viewModes)
		require.NotContains(t, p.store.viewModes, false)
		require.NotEmpty(t, p.store.blobModes)
		require.NotContains(t, p.store.blobModes, false)
	}
}

// TestSigHashUpgradeWithPendingCommitSig asserts that a commitment_signed
// that was signed before both peers negotiated the feature is still accepted
// when it is retransmitted after they did. The receiver refreshes its view of
// the negotiation before it processes the retransmission, so it must take
// the sighash mode from the message: verifying a legacy signature as
// SigHashDefault fails and force closes the channel. The HTLC that sits
// between the two dust thresholds makes the two modes disagree about the
// commitment transaction itself, not only about the HTLC signatures.
func TestSigHashUpgradeWithPendingCommitSig(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name           string
		betweenDust    bool
		numLegacySigs  int
		numDefaultSigs int
	}{{
		name:           "htlc above both dust limits",
		numLegacySigs:  1,
		numDefaultSigs: 2,
	}, {
		name:           "htlc between the dust limits",
		betweenDust:    true,
		numLegacySigs:  1,
		numDefaultSigs: 1,
	}}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			alice, bob := newSigHashParties(t)
			htlcAmt, between := sigHashHtlcAmounts(
				t, alice.channel,
			)
			pendingAmt := htlcAmt
			if tc.betweenDust {
				pendingAmt = between
			}

			// Alice signs a legacy commitment for Bob, who never
			// gets the message.
			addSigHashHtlc(t, alice, bob, pendingAmt, 1)
			lost, err := alice.channel.SignNextCommitment(ctxb)
			require.NoError(t, err)
			require.Len(t, lost.HtlcSigs, tc.numLegacySigs)
			require.False(
				t, statesSigHashDefault(t, lost.AuxSigBlob),
			)

			// Both upgrade, reconnect and negotiate the feature.
			// The channel picks that up before the sync messages
			// are processed.
			_, err = alice.channel.GenMusigNonces()
			require.NoError(t, err)
			_, err = bob.channel.GenMusigNonces()
			require.NoError(t, err)

			aliceState := alice.channel.channelState
			aliceSync, err := aliceState.ChanSyncMsg()
			require.NoError(t, err)
			bobState := bob.channel.channelState
			bobSync, err := bobState.ChanSyncMsg()
			require.NoError(t, err)

			alice.negotiate(fn.Some(txscript.SigHashDefault))
			bob.negotiate(fn.Some(txscript.SigHashDefault))
			require.True(t, alice.channel.IsChanSigHashDefault())
			require.True(t, bob.channel.IsChanSigHashDefault())

			// Alice retransmits the HTLC and the commitment as it
			// was signed, Bob has nothing to send.
			msgs, _, _, err := alice.channel.ProcessChanSyncMsg(
				ctxb, bobSync,
			)
			require.NoError(t, err)
			require.Len(t, msgs, 2)
			retransmit, ok := msgs[1].(*lnwire.CommitSig)
			require.True(t, ok)

			bobMsgs, _, _, err := bob.channel.ProcessChanSyncMsg(
				ctxb, aliceSync,
			)
			require.NoError(t, err)
			require.Empty(t, bobMsgs)

			// The retransmission still reads as legacy, and Bob
			// accepts it as such although his next commitments
			// would use SigHashDefault.
			commitSigs := commitSigsFromMsg(t, retransmit)
			require.False(t, statesSigHashDefault(
				t, commitSigs.AuxSigBlob,
			))
			require.NoError(
				t, bob.channel.ReceiveNewCommitment(commitSigs),
			)
			require.False(t, alice.tipDefault(lntypes.Remote))
			require.False(t, bob.tipDefault(lntypes.Local))

			// The commitment Bob signs in return is a new one, so
			// it uses SigHashDefault.
			completeSigHashTransition(t, alice, bob)
			require.True(t, bob.tipDefault(lntypes.Remote))
			require.True(t, alice.tipDefault(lntypes.Local))

			// And so does the next one Alice signs. The HTLC
			// between the dust limits no longer has an output on
			// it.
			addSigHashHtlc(t, alice, bob, htlcAmt, 2)
			next, err := alice.channel.SignNextCommitment(ctxb)
			require.NoError(t, err)
			require.Len(t, next.HtlcSigs, tc.numDefaultSigs)
			require.True(
				t, statesSigHashDefault(t, next.AuxSigBlob),
			)
			require.NoError(t, bob.channel.ReceiveNewCommitment(
				next.CommitSigs,
			))
			require.True(t, alice.tipDefault(lntypes.Remote))
			require.True(t, bob.tipDefault(lntypes.Local))
			completeSigHashTransition(t, alice, bob)
		})
	}
}

// TestSigHashDefaultCommitSigRetransmitted asserts that a commitment_signed
// that states SigHashDefault is accepted when it is retransmitted in a later
// connection, and that the mode of a chain does not depend on the live
// negotiation once its latest commitment uses SigHashDefault: the sender
// keeps signing with it and the receiver keeps accepting it.
func TestSigHashDefaultCommitSigRetransmitted(t *testing.T) {
	t.Parallel()

	alice, bob := newSigHashParties(t)
	htlcAmt, _ := sigHashHtlcAmounts(t, alice.channel)

	alice.negotiate(fn.Some(txscript.SigHashDefault))
	bob.negotiate(fn.Some(txscript.SigHashDefault))

	// The first SigHashDefault commitment is lost on the way to Bob.
	addSigHashHtlc(t, alice, bob, htlcAmt, 1)
	lost, err := alice.channel.SignNextCommitment(ctxb)
	require.NoError(t, err)
	require.True(t, statesSigHashDefault(t, lost.AuxSigBlob))

	_, err = alice.channel.GenMusigNonces()
	require.NoError(t, err)
	_, err = bob.channel.GenMusigNonces()
	require.NoError(t, err)

	aliceSync, err := alice.channel.channelState.ChanSyncMsg()
	require.NoError(t, err)
	bobSync, err := bob.channel.channelState.ChanSyncMsg()
	require.NoError(t, err)

	msgs, _, _, err := alice.channel.ProcessChanSyncMsg(ctxb, bobSync)
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	retransmit, ok := msgs[1].(*lnwire.CommitSig)
	require.True(t, ok)
	_, _, _, err = bob.channel.ProcessChanSyncMsg(ctxb, aliceSync)
	require.NoError(t, err)

	commitSigs := commitSigsFromMsg(t, retransmit)
	require.True(t, statesSigHashDefault(t, commitSigs.AuxSigBlob))
	require.NoError(t, bob.channel.ReceiveNewCommitment(commitSigs))
	require.True(t, bob.tipDefault(lntypes.Local))
	completeSigHashTransition(t, alice, bob)

	// The live negotiation is gone, as it is between a restart and the
	// peer reconnecting. Alice's own latest commitment is the only thing
	// her cached answer falls back to, so point that at a legacy blob to
	// have the answer for new commitments go back to legacy.
	legacy := fn.Some(legacySigHashBlob)
	alice.channel.channelState.LocalCommitment.CustomBlob = legacy
	bob.channel.channelState.LocalCommitment.CustomBlob = legacy
	alice.negotiate(fn.None[txscript.SigHashType]())
	bob.negotiate(fn.None[txscript.SigHashType]())
	require.False(t, alice.channel.IsChanSigHashDefault())
	require.False(t, bob.channel.IsChanSigHashDefault())

	// Both chains still use SigHashDefault.
	require.True(t, alice.channel.nextSigHashDefault(lntypes.Remote))
	require.True(t, bob.channel.nextSigHashDefault(lntypes.Local))

	addSigHashHtlc(t, alice, bob, htlcAmt, 2)
	commit, err := alice.channel.SignNextCommitment(ctxb)
	require.NoError(t, err)
	require.True(t, statesSigHashDefault(t, commit.AuxSigBlob))
	require.NoError(
		t, bob.channel.ReceiveNewCommitment(commit.CommitSigs),
	)
	require.True(t, bob.tipDefault(lntypes.Local))
}

// TestCommitSigHtlcSigHashRejected asserts the cases in which the receiver
// refuses the HTLC sighash mode a commitment_signed states, and that it does
// so before it looks at any signature.
func TestCommitSigHtlcSigHashRejected(t *testing.T) {
	t.Parallel()

	sigHashDefault := []byte{byte(txscript.SigHashDefault)}

	testCases := []struct {
		name string

		// negotiated is whether the feature is negotiated, in which
		// case one SigHashDefault commitment is exchanged first.
		negotiated bool

		// record is the value the record of the commitment_signed is
		// replaced with, nil removes it.
		record []byte

		expectedErr error
	}{{
		name:        "sighash default without the feature",
		record:      sigHashDefault,
		expectedErr: ErrHtlcSigHashTypeNotNegotiated,
	}, {
		name:        "back to legacy after sighash default",
		negotiated:  true,
		expectedErr: ErrHtlcSigHashTypeReverted,
	}, {
		name:        "unknown sighash type",
		negotiated:  true,
		record:      []byte{byte(txscript.SigHashAll)},
		expectedErr: ErrUnknownHtlcSigHashType,
	}, {
		name:        "empty record",
		negotiated:  true,
		record:      []byte{},
		expectedErr: ErrUnknownHtlcSigHashType,
	}, {
		name:        "record too long",
		negotiated:  true,
		record:      []byte{byte(txscript.SigHashDefault), 0},
		expectedErr: ErrUnknownHtlcSigHashType,
	}}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			alice, bob := newSigHashParties(t)
			htlcAmt, _ := sigHashHtlcAmounts(t, alice.channel)

			if tc.negotiated {
				defaultType := fn.Some(txscript.SigHashDefault)
				alice.negotiate(defaultType)
				bob.negotiate(defaultType)

				addSigHashHtlc(t, alice, bob, htlcAmt, 1)
				require.NoError(t, ForceStateTransition(
					alice.channel, bob.channel,
				))
				require.True(t, bob.tipDefault(lntypes.Local))
			}

			addSigHashHtlc(t, alice, bob, htlcAmt, 2)
			commit, err := alice.channel.SignNextCommitment(ctxb)
			require.NoError(t, err)

			tipBefore := bob.channel.commitChains.Local.tip()
			bob.signer.unpacked = nil

			commit.AuxSigBlob = withSigHashRecord(
				t, commit.AuxSigBlob, tc.record,
			)
			err = bob.channel.ReceiveNewCommitment(
				commit.CommitSigs,
			)
			require.ErrorIs(t, err, tc.expectedErr)

			// Nothing was verified and nothing changed.
			require.Empty(t, bob.signer.unpacked)
			require.Same(
				t, tipBefore,
				bob.channel.commitChains.Local.tip(),
			)
		})
	}
}

// TestCommitSigHtlcSigHashCustomOnly asserts that the HTLC sighash record is
// a matter of custom channels only. The commitment_signed of any other
// channel never carries it, and a record under that type on a message such a
// channel receives is not interpreted: it stays in the custom records, as
// every custom record did before.
func TestCommitSigHtlcSigHashCustomOnly(t *testing.T) {
	t.Parallel()

	chanType := channeldb.SimpleTaprootFeatureBit
	aliceChannel, bobChannel, err := CreateTestChannels(t, chanType)
	require.NoError(t, err)
	require.False(t, aliceChannel.statesHtlcSigHashType())

	htlc := &lnwire.UpdateAddHTLC{
		PaymentHash: sha256.Sum256(bytes.Repeat([]byte{1}, 32)),
		Amount:      lnwire.NewMSatFromSatoshis(100_000),
		Expiry:      uint32(10),
	}
	_, err = aliceChannel.AddHTLC(htlc, nil)
	require.NoError(t, err)
	_, err = bobChannel.ReceiveHTLC(htlc)
	require.NoError(t, err)

	commit, err := aliceChannel.SignNextCommitment(ctxb)
	require.NoError(t, err)
	require.False(t, statesSigHashDefault(t, commit.AuxSigBlob))

	// A record under our type changes nothing for Bob: the commitment is
	// verified with the standard sighash of the channel type and the
	// records are passed on untouched.
	withRecord := withSigHashRecord(
		t, commit.AuxSigBlob, []byte{byte(txscript.SigHashDefault)},
	)
	sigHashDefault, auxSigBlob, err := bobChannel.receivedSigHashDefault(
		withRecord,
	)
	require.NoError(t, err)
	require.False(t, sigHashDefault)
	require.Equal(t, withRecord, auxSigBlob)

	commit.AuxSigBlob = withRecord
	require.NoError(
		t, bobChannel.ReceiveNewCommitment(commit.CommitSigs),
	)

	// A custom channel without an aux signer cannot use anything but the
	// standard sighash either.
	customType := chanType | channeldb.TapscriptRootBit
	customChannel, _, err := CreateTestChannels(t, customType)
	require.NoError(t, err)
	require.True(t, customChannel.statesHtlcSigHashType())
	customChannel.auxSigner = fn.None[AuxSigner]()
	require.False(t, customChannel.statesHtlcSigHashType())
}

// TestComputeViewSigHashMode asserts that the view of a commitment is
// evaluated with the sighash mode it is given, for the HTLCs of both parties.
// An HTLC between the two dust thresholds adds its weight to a legacy
// commitment and none to a SigHashDefault one. The mode the channel would use
// for its next commitment must not leak into it.
func TestComputeViewSigHashMode(t *testing.T) {
	t.Parallel()

	alice, bob := newSigHashParties(t)
	_, between := sigHashHtlcAmounts(t, alice.channel)
	addSigHashHtlc(t, alice, bob, between, 1)

	// The amount sits between the dust thresholds of Bob's commitment,
	// which is the remote chain for Alice and the local one for Bob.
	testCases := []struct {
		name  string
		party *sigHashParty
		chain lntypes.ChannelParty
	}{{
		// The HTLC is one of Alice's own updates.
		name:  "offered htlc",
		party: alice,
		chain: lntypes.Remote,
	}, {
		// The HTLC is one of the remote party's updates for Bob.
		name:  "received htlc",
		party: bob,
		chain: lntypes.Local,
	}}

	for _, tc := range testCases {
		channel := tc.party.channel
		weight := func(sigHashDefault bool) lntypes.WeightUnit {
			view := channel.fetchHTLCView(
				channel.updateLogs.Remote.logIndex,
				channel.updateLogs.Local.logIndex,
			)
			_, _, weight, _, err := channel.computeView(
				view, tc.chain, false,
				fn.None[chainfee.SatPerKWeight](),
				sigHashDefault,
			)
			require.NoError(t, err, tc.name)

			return weight
		}

		for _, live := range []fn.Option[txscript.SigHashType]{
			fn.None[txscript.SigHashType](),
			fn.Some(txscript.SigHashDefault),
		} {
			tc.party.negotiate(live)

			require.Equal(
				t, weight(true)+input.HTLCWeight,
				weight(false), tc.name,
			)
		}
	}
}
