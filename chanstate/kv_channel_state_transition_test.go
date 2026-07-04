package chanstate

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

const (
	dummyLocalOutputIndex = uint32(0)
	dummyRemoteOutIndex   = uint32(1)
)

var testWireSig, _ = lnwire.NewSigFromSignature(testSig)

func loadKVFwdPkgs(t *testing.T, db kvdb.Backend,
	packager FwdPackager) []*FwdPkg {

	t.Helper()

	var fwdPkgs []*FwdPkg
	err := kvdb.View(db, func(tx kvdb.RTx) error {
		var err error
		fwdPkgs, err = packager.LoadFwdPkgs(tx)
		return err
	}, func() {
		fwdPkgs = nil
	})
	require.NoError(t, err, "unable to load fwd pkgs")

	return fwdPkgs
}

func assertRevocationLogEntryEqual(t *testing.T, c *ChannelCommitment,
	r *RevocationLog) {

	t.Helper()

	require.EqualValues(
		t, r.CommitTxHash.Val, c.CommitTx.TxHash(), "CommitTx mismatch",
	)
	require.Equal(t, len(r.HTLCEntries), len(c.Htlcs), "HTLCs len mismatch")

	for i, rHtlc := range r.HTLCEntries {
		cHtlc := c.Htlcs[i]
		require.Equal(t, rHtlc.RHash.Val[:], cHtlc.RHash[:], "RHash")
		require.Equal(
			t, rHtlc.Amt.Val.Int(), cHtlc.Amt.ToSatoshis(), "Amt",
		)
		require.Equal(
			t, rHtlc.RefundTimeout.Val, cHtlc.RefundTimeout,
			"RefundTimeout",
		)
		require.EqualValues(
			t, rHtlc.OutputIndex.Val, cHtlc.OutputIndex,
			"OutputIndex",
		)
		require.Equal(
			t, rHtlc.Incoming.Val, cHtlc.Incoming, "Incoming",
		)
	}
}

func TestChannelStateTransition(t *testing.T) {
	t.Parallel()

	backend, cleanup := makeTestBackend(t)
	t.Cleanup(cleanup)

	store := NewKVStore(backend)

	channel := createTestChannel(t, store)

	var (
		htlcs   []HTLC
		htlcAmt lnwire.MilliSatoshi
	)
	for i := uint32(0); i < 10; i++ {
		incoming := i > 5
		htlc := HTLC{
			Signature:     testSig.Serialize(),
			Incoming:      incoming,
			Amt:           10,
			RHash:         key,
			RefundTimeout: i,
			OutputIndex:   int32(i * 3),
			LogIndex:      uint64(i * 2),
			HtlcIndex:     uint64(i),
		}
		copy(
			htlc.OnionBlob[:],
			bytes.Repeat([]byte{2}, lnwire.OnionPacketSize),
		)
		htlcs = append(htlcs, htlc)
		htlcAmt += htlc.Amt
	}

	newSequence := uint32(129498)
	newSig := bytes.Repeat([]byte{3}, 71)
	newTx := channel.LocalCommitment.CommitTx.Copy()
	newTx.TxIn[0].Sequence = newSequence
	commitment := ChannelCommitment{
		CommitHeight:    1,
		LocalLogIndex:   2,
		LocalHtlcIndex:  1,
		RemoteLogIndex:  2,
		RemoteHtlcIndex: 1,
		LocalBalance:    lnwire.MilliSatoshi(1e8),
		RemoteBalance:   lnwire.MilliSatoshi(1e8),
		CommitFee:       55,
		FeePerKw:        99,
		CommitTx:        newTx,
		CommitSig:       newSig,
		Htlcs:           htlcs,
		CustomBlob:      fn.Some([]byte{4, 5, 6}),
	}

	unsignedAckedUpdates := []LogUpdate{
		{
			LogIndex: 2,
			UpdateMsg: &lnwire.UpdateAddHTLC{
				ChanID: lnwire.ChannelID{1, 2, 3},
			},
		},
	}

	_, err := channel.UpdateCommitment(
		&commitment, unsignedAckedUpdates,
	)
	require.NoError(t, err, "unable to update commitment")

	dbUnsignedAckedUpdates, err := channel.UnsignedAckedUpdates()
	require.NoError(t, err, "unable to fetch dangling remote updates")
	require.Equal(t, unsignedAckedUpdates, dbUnsignedAckedUpdates)

	updatedChannel, err := store.FetchOpenChannels(channel.IdentityPub)
	require.NoError(t, err, "unable to fetch updated channel")
	require.Equal(t, commitment, updatedChannel[0].LocalCommitment)

	numDiskUpdates, err := updatedChannel[0].CommitmentHeight()
	require.NoError(t, err, "unable to read commitment height from disk")
	require.Equal(t, commitment.CommitHeight, numDiskUpdates)

	_, err = channel.RemoteCommitChainTip()
	require.ErrorIs(t, err, ErrNoPendingCommit)

	remoteCommit := commitment
	remoteCommit.LocalBalance = lnwire.MilliSatoshi(2e8)
	remoteCommit.RemoteBalance = lnwire.MilliSatoshi(3e8)
	remoteCommit.CommitHeight = 1
	commitDiff := &CommitDiff{
		Commitment: remoteCommit,
		CommitSig: &lnwire.CommitSig{
			ChanID:    lnwire.ChannelID(key),
			CommitSig: testWireSig,
			HtlcSigs: []lnwire.Sig{
				testWireSig,
				testWireSig,
			},
		},
		LogUpdates: []LogUpdate{
			{
				LogIndex: 1,
				UpdateMsg: &lnwire.UpdateAddHTLC{
					ID:     1,
					Amount: lnwire.NewMSatFromSatoshis(100),
					Expiry: 25,
				},
			},
			{
				LogIndex: 2,
				UpdateMsg: &lnwire.UpdateAddHTLC{
					ID:     2,
					Amount: lnwire.NewMSatFromSatoshis(200),
					Expiry: 50,
				},
			},
		},
		OpenedCircuitKeys: []models.CircuitKey{},
		ClosedCircuitKeys: []models.CircuitKey{},
	}
	update1, ok := commitDiff.LogUpdates[0].UpdateMsg.(*lnwire.
		UpdateAddHTLC)
	require.True(t, ok)
	copy(update1.PaymentHash[:], bytes.Repeat([]byte{1}, 32))

	update2, ok := commitDiff.LogUpdates[1].UpdateMsg.(*lnwire.
		UpdateAddHTLC)
	require.True(t, ok)
	copy(update2.PaymentHash[:], bytes.Repeat([]byte{2}, 32))
	err = channel.AppendRemoteCommitChain(commitDiff)
	require.NoError(t, err, "unable to add to commit chain")

	diskCommitDiff, err := channel.RemoteCommitChainTip()
	require.NoError(t, err, "unable to fetch commit diff")
	require.Equal(t, commitDiff, diskCommitDiff)

	oldRemoteCommit := channel.RemoteCommitment
	channel.RemoteCurrentRevocation = channel.RemoteNextRevocation
	newPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err, "unable to generate key")
	channel.RemoteNextRevocation = newPriv.PubKey()

	fwdPkg := NewFwdPkg(
		channel.ShortChanID(), oldRemoteCommit.CommitHeight,
		diskCommitDiff.LogUpdates, nil,
	)
	err = channel.AdvanceCommitChainTail(
		fwdPkg, nil, dummyLocalOutputIndex, dummyRemoteOutIndex,
	)
	require.NoError(t, err, "unable to append to revocation log")

	_, err = channel.RemoteCommitChainTip()
	require.ErrorIs(t, err, ErrNoPendingCommit)

	diskPrevCommit, _, err := channel.FindPreviousState(
		oldRemoteCommit.CommitHeight,
	)
	require.NoError(t, err, "unable to fetch past delta")
	require.EqualValues(
		t, dummyLocalOutputIndex, diskPrevCommit.OurOutputIndex.Val,
	)
	require.EqualValues(
		t, dummyRemoteOutIndex, diskPrevCommit.TheirOutputIndex.Val,
	)
	assertRevocationLogEntryEqual(t, &oldRemoteCommit, diskPrevCommit)

	logTailHeight, err := store.RevocationLogTailCommitHeight(channel)
	require.NoError(t, err, "unable to retrieve log")
	require.Equal(t, oldRemoteCommit.CommitHeight, logTailHeight)

	oldRemoteCommit = channel.RemoteCommitment
	commitDiff.Commitment.CommitHeight = 2
	commitDiff.Commitment.LocalBalance -= htlcAmt
	commitDiff.Commitment.RemoteBalance += htlcAmt
	commitDiff.LogUpdates = []LogUpdate{}
	err = channel.AppendRemoteCommitChain(commitDiff)
	require.NoError(t, err, "unable to add to commit chain")

	fwdPkg = NewFwdPkg(
		channel.ShortChanID(), oldRemoteCommit.CommitHeight, nil, nil,
	)
	err = channel.AdvanceCommitChainTail(
		fwdPkg, nil, dummyLocalOutputIndex, dummyRemoteOutIndex,
	)
	require.NoError(t, err, "unable to append to revocation log")

	prevCommit, _, err := channel.FindPreviousState(
		oldRemoteCommit.CommitHeight,
	)
	require.NoError(t, err, "unable to fetch past delta")
	require.EqualValues(
		t, dummyLocalOutputIndex, diskPrevCommit.OurOutputIndex.Val,
	)
	require.EqualValues(
		t, dummyRemoteOutIndex, diskPrevCommit.TheirOutputIndex.Val,
	)
	assertRevocationLogEntryEqual(t, &oldRemoteCommit, prevCommit)

	logTailHeight, err = store.RevocationLogTailCommitHeight(channel)
	require.NoError(t, err, "unable to retrieve log")
	require.Equal(t, oldRemoteCommit.CommitHeight, logTailHeight)

	updatedChannel, err = store.FetchOpenChannels(channel.IdentityPub)
	require.NoError(t, err, "unable to fetch updated channel")
	require.True(t, channel.RemoteCurrentRevocation.IsEqual(
		updatedChannel[0].RemoteCurrentRevocation,
	))
	require.True(t, channel.RemoteNextRevocation.IsEqual(
		updatedChannel[0].RemoteNextRevocation,
	))

	fwdPkgs := loadKVFwdPkgs(
		t, store.backend, NewChannelPackager(channel.ShortChanID()),
	)
	require.Len(t, fwdPkgs, 2, "wrong number of forwarding packages")

	closeSummary := &ChannelCloseSummary{
		ChanPoint:         channel.FundingOutpoint,
		RemotePub:         channel.IdentityPub,
		SettledBalance:    btcutil.Amount(500),
		TimeLockedBalance: btcutil.Amount(10000),
		IsPending:         false,
		CloseType:         RemoteForceClose,
	}
	err = updatedChannel[0].CloseChannel(closeSummary)
	require.NoError(t, err, "unable to delete updated channel")

	channels, err := store.FetchOpenChannels(channel.IdentityPub)
	require.NoError(t, err, "unable to fetch updated channels")
	require.Empty(t, channels)

	_, _, err = updatedChannel[0].FindPreviousState(
		oldRemoteCommit.CommitHeight,
	)
	require.Error(t, err, "revocation log search should fail")

	fwdPkgs = loadKVFwdPkgs(
		t, store.backend, NewChannelPackager(channel.ShortChanID()),
	)
	require.Empty(t, fwdPkgs, "no forwarding packages should exist")
}
