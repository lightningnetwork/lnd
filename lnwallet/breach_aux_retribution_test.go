package lnwallet

import (
	"errors"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// recordingAuxResolver is an AuxContractResolver that records the requests
// it receives and answers them with a canned blob or error.
type recordingAuxResolver struct {
	reqs []ResolutionReq
	blob tlv.Blob
	err  error
}

// ResolveContract records the request and returns the canned answer.
func (r *recordingAuxResolver) ResolveContract(
	req ResolutionReq) fn.Result[tlv.Blob] {

	r.reqs = append(r.reqs, req)
	if r.err != nil {
		return fn.Err[tlv.Blob](r.err)
	}

	return fn.Ok(r.blob)
}

// newAuxBreachHtlcEntry returns the revocation log entry of an HTLC on an aux
// channel, with the given custom records stored for it.
func newAuxBreachHtlcEntry(t *testing.T, incoming bool, htlcIndex uint64,
	records lnwire.CustomRecords) *channeldb.HTLCEntry {

	t.Helper()

	htlc := &channeldb.HTLCEntry{
		Amt: tlv.NewRecordT[tlv.TlvType4](
			tlv.NewBigSizeT(btcutil.Amount(20_000)),
		),
		Incoming:    tlv.NewPrimitiveRecord[tlv.TlvType3](incoming),
		OutputIndex: tlv.NewPrimitiveRecord[tlv.TlvType2, uint16](1),
		HtlcIndex: tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType6](
				tlv.NewBigSizeT(htlcIndex),
			),
		),
	}

	if len(records) > 0 {
		blob, err := records.Serialize()
		require.NoError(t, err)

		htlc.CustomBlob = tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType5](blob),
		)
	}

	return htlc
}

// newAuxBreachTestChannel creates a custom (tapscript root) channel and the
// dummy parameters createHtlcRetribution takes.
func newAuxBreachTestChannel(t *testing.T) (*LightningChannel,
	*btcec.PrivateKey) {

	t.Helper()

	chanType := channeldb.SimpleTaprootFeatureBit |
		channeldb.TapscriptRootBit

	aliceChannel, _, err := CreateTestChannels(t, chanType)
	require.NoError(t, err)

	dummyPrivate, _ := btcec.PrivKeyFromBytes([]byte{1})

	return aliceChannel, dummyPrivate
}

// TestCreateHtlcRetributionAuxDirection asserts that the resolution request
// of a breached HTLC on an aux channel states the HTLC's direction, both in
// the request sent to the aux resolver and in the template kept for a later
// second-level resolution. The HTLC index alone does not identify the HTLC,
// as both parties count the HTLCs they offer from zero.
func TestCreateHtlcRetributionAuxDirection(t *testing.T) {
	t.Parallel()

	aliceChannel, dummyPrivate := newAuxBreachTestChannel(t)
	chanState := aliceChannel.channelState
	leaseExpiry, keyRing, commitHash := deriveDummyRetributionParams(
		chanState,
	)

	for _, incoming := range []bool{true, false} {
		resolver := &recordingAuxResolver{blob: tlv.Blob{0x01}}
		htlc := newAuxBreachHtlcEntry(t, incoming, 0, nil)

		hr, err := createHtlcRetribution(
			chanState, keyRing, commitHash, dummyPrivate,
			leaseExpiry, htlc, fn.None[CommitAuxLeaves](), nil,
			fn.Some[AuxContractResolver](resolver), nil, 0,
		)
		require.NoError(t, err)

		// Both HTLCs share index 0. Only the direction tells them
		// apart.
		require.Len(t, resolver.reqs, 1)
		require.Equal(
			t, fn.Some(incoming), resolver.reqs[0].HtlcIncoming,
		)
		require.Equal(
			t, fn.Some[uint64](0), resolver.reqs[0].HtlcID,
		)

		require.NotNil(t, hr.ResolveReq)
		require.Equal(t, fn.Some(incoming), hr.ResolveReq.HtlcIncoming)
		require.Equal(t, incoming, hr.IsIncoming)
		require.Equal(t, fn.Some(tlv.Blob{0x01}), hr.ResolutionBlob)
	}
}

// TestCreateHtlcRetributionAuxResolveFailure asserts that a failure of the
// aux resolver for one breached HTLC does not fail the retribution. Nothing
// retries a breach handoff that errors out, so one HTLC the resolver cannot
// handle would otherwise leave the commitment outputs and every other HTLC
// of the revoked state unpunished. The HTLC is instead handed over without a
// blob, with its template intact for a later second-level resolution.
func TestCreateHtlcRetributionAuxResolveFailure(t *testing.T) {
	t.Parallel()

	aliceChannel, dummyPrivate := newAuxBreachTestChannel(t)
	chanState := aliceChannel.channelState
	leaseExpiry, keyRing, commitHash := deriveDummyRetributionParams(
		chanState,
	)

	resolver := &recordingAuxResolver{err: errors.New("cannot resolve")}
	htlc := newAuxBreachHtlcEntry(t, true, 3, nil)

	hr, err := createHtlcRetribution(
		chanState, keyRing, commitHash, dummyPrivate, leaseExpiry,
		htlc, fn.None[CommitAuxLeaves](), nil,
		fn.Some[AuxContractResolver](resolver), nil, 0,
	)
	require.NoError(t, err)
	require.Len(t, resolver.reqs, 1)

	// The HTLC is still part of the retribution, just without a blob.
	require.NotNil(t, hr.SignDesc.Output)
	require.EqualValues(t, 20_000, hr.SignDesc.Output.Value)
	require.True(t, hr.ResolutionBlob.IsNone())
	require.NotNil(t, hr.ResolveReq)
}

// TestAuxSigDescFromHTLCBlobRevocationSigOnly asserts that the aux sig handed
// to the aux resolver for a breached HTLC is the remote party's revocation
// aux sig and nothing else. The CommitSig aux sig that can sit in the same
// custom records covers the second-level HTLC of our own commitment, which is
// a different transaction: passing it on would make the resolver build a
// proof around a sig that can never verify, instead of resolving the HTLC
// without one.
func TestAuxSigDescFromHTLCBlobRevocationSigOnly(t *testing.T) {
	t.Parallel()

	aliceChannel, _ := newAuxBreachTestChannel(t)
	chanState := aliceChannel.channelState
	_, keyRing, _ := deriveDummyRetributionParams(chanState)

	revSigType := uint64(revocationAuxSigType.TypeVal())
	commitSigType := uint64(htlcCustomSigType.TypeVal())
	revSig := []byte{0x01, 0x02}
	commitSig := []byte{0x03, 0x04}

	testCases := []struct {
		name    string
		records lnwire.CustomRecords
		want    []byte
	}{{
		name: "no custom records",
	}, {
		name: "revocation sig",
		records: lnwire.CustomRecords{
			revSigType: revSig,
		},
		want: revSig,
	}, {
		name: "revocation sig next to a commit sig",
		records: lnwire.CustomRecords{
			revSigType:    revSig,
			commitSigType: commitSig,
		},
		want: revSig,
	}, {
		// A state that was revoked without revocation aux sigs must
		// not fall back to the sig made for our own commitment.
		name: "commit sig only",
		records: lnwire.CustomRecords{
			commitSigType: commitSig,
		},
	}}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			htlc := newAuxBreachHtlcEntry(t, true, 0, tc.records)
			desc := auxSigDescFromHTLCBlob(htlc, chanState, keyRing)

			if tc.want == nil {
				require.True(t, desc.IsNone())

				return
			}

			auxSigDesc, err := desc.UnwrapOrErr(
				errors.New("no aux sig desc"),
			)
			require.NoError(t, err)
			require.Equal(t, tc.want, auxSigDesc.AuxSig)
			require.Equal(
				t, keyRing.LocalHtlcKeyTweak,
				auxSigDesc.SignDetails.SignDesc.SingleTweak,
			)
		})
	}
}
