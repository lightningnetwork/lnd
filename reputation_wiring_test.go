package lnd

import (
	"errors"
	"testing"

	"github.com/lightningnetwork/lnd/chanstate"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/htlcswitch/hop"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/reputation"
	"github.com/stretchr/testify/require"
)

// fakeCircuitSource is a test double for the switch's open circuits.
type fakeCircuitSource struct {
	circuits []*htlcswitch.PaymentCircuit
}

func (f *fakeCircuitSource) ActiveCircuits() []*htlcswitch.PaymentCircuit {
	return f.circuits
}

// fakeChannelSource is a test double for the open channel source.
type fakeChannelSource struct {
	chans []*chanstate.OpenChannel
	err   error
}

func (f *fakeChannelSource) FetchAllOpenChannels() ([]*chanstate.OpenChannel,
	error) {

	return f.chans, f.err
}

// makeHTLC builds a commitment HTLC with the given direction, index, expiry
// and accountable signal.
func makeHTLC(incoming bool, htlcIndex uint64, cltv uint32,
	accountable bool) chanstate.HTLC {

	records := lnwire.CustomRecords{}
	if accountable {
		records[uint64(lnwire.ExperimentalAccountableType)] =
			[]byte{lnwire.ExperimentalAccountable}
	}

	return chanstate.HTLC{
		Incoming:      incoming,
		HtlcIndex:     htlcIndex,
		RefundTimeout: cltv,
		CustomRecords: records,
	}
}

// makeChannel builds an open channel with the given scid whose HTLCs are
// locked in on both commitments, so ActiveHtlcs returns them.
func makeChannel(scid uint64, htlcs ...chanstate.HTLC) *chanstate.OpenChannel {
	c := &chanstate.OpenChannel{
		ShortChannelID: lnwire.NewShortChanIDFromInt(scid),
	}
	c.LocalCommitment.Htlcs = htlcs
	c.RemoteCommitment.Htlcs = htlcs

	return c
}

func inKey(scid, htlcID uint64) models.CircuitKey {
	return models.CircuitKey{
		ChanID: lnwire.NewShortChanIDFromInt(scid),
		HtlcID: htlcID,
	}
}

func outKey(scid, htlcID uint64) *models.CircuitKey {
	k := inKey(scid, htlcID)

	return &k
}

// TestAssembleInFlightHTLCs checks that open circuits are joined with the live
// incoming commitment HTLCs to recover the cltv expiry and accountable signal,
// that the fee is the offered fee, and that everything which is not a live
// forward is filtered out.
func TestAssembleInFlightHTLCs(t *testing.T) {
	t.Parallel()

	chans := []*chanstate.OpenChannel{
		// Channel 1 has two incoming HTLCs and one we offered, which
		// must not be mistaken for an incoming one with the same index.
		makeChannel(
			1,
			makeHTLC(true, 0, 500, true),
			makeHTLC(true, 1, 600, false),
			makeHTLC(false, 0, 700, true),
		),
		// Channel 3 has an incoming HTLC only locked in on one
		// commitment, so it is not active.
		func() *chanstate.OpenChannel {
			c := makeChannel(3)
			c.LocalCommitment.Htlcs = []chanstate.HTLC{
				makeHTLC(true, 0, 800, true),
			}

			return c
		}(),
	}

	circuits := []*htlcswitch.PaymentCircuit{
		// Accountable forward with a 100 msat fee.
		{
			Incoming: inKey(1, 0), Outgoing: outKey(2, 7),
			IncomingAmount: 1100, OutgoingAmount: 1000,
		},
		// Unaccountable forward whose sender underpaid (impossible for
		// an accepted forward, but the fee must not go negative).
		{
			Incoming: inKey(1, 1), Outgoing: outKey(2, 8),
			IncomingAmount: 900, OutgoingAmount: 1000,
		},
		// Locally initiated payment: not a forward.
		{
			Incoming: models.CircuitKey{
				ChanID: hop.Source, HtlcID: 3,
			},
			Outgoing:       outKey(2, 9),
			IncomingAmount: 1000, OutgoingAmount: 1000,
		},
		// Committed but never opened on an outgoing link.
		{
			Incoming: inKey(1, 5), IncomingAmount: 1000,
			OutgoingAmount: 1000,
		},
		// Incoming HTLC not active on the commitment any more.
		{
			Incoming: inKey(3, 0), Outgoing: outKey(2, 10),
			IncomingAmount: 1000, OutgoingAmount: 1000,
		},
		// Incoming channel unknown altogether.
		{
			Incoming: inKey(9, 0), Outgoing: outKey(2, 11),
			IncomingAmount: 1000, OutgoingAmount: 1000,
		},
	}

	got := assembleInFlightHTLCs(circuits, chans)
	require.Equal(t, []reputation.InFlightHTLC{
		{
			Incoming:     inKey(1, 0),
			Outgoing:     lnwire.NewShortChanIDFromInt(2),
			Fee:          100,
			IncomingCltv: 500,
			Accountable:  true,
		},
		{
			Incoming:     inKey(1, 1),
			Outgoing:     lnwire.NewShortChanIDFromInt(2),
			Fee:          0,
			IncomingCltv: 600,
			Accountable:  false,
		},
	}, got)
}

// TestReconstructInFlightHTLCs checks the sources are read through, and that a
// channel source error is surfaced rather than treated as no channels.
func TestReconstructInFlightHTLCs(t *testing.T) {
	t.Parallel()

	circuits := &fakeCircuitSource{
		circuits: []*htlcswitch.PaymentCircuit{{
			Incoming: inKey(1, 0), Outgoing: outKey(2, 0),
			IncomingAmount: 1010, OutgoingAmount: 1000,
		}},
	}
	chans := &fakeChannelSource{
		chans: []*chanstate.OpenChannel{
			makeChannel(1, makeHTLC(true, 0, 500, true)),
		},
	}

	got, err := reconstructInFlightHTLCs(circuits, chans)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.EqualValues(t, 10, got[0].Fee)
	require.True(t, got[0].Accountable)

	// An error reading the channels must not be mistaken for "no HTLCs in
	// flight".
	chans.err = errors.New("db closed")
	_, err = reconstructInFlightHTLCs(circuits, chans)
	require.ErrorIs(t, err, chans.err)

	// No open circuits means nothing to replay.
	chans.err = nil
	circuits.circuits = nil
	got, err = reconstructInFlightHTLCs(circuits, chans)
	require.NoError(t, err)
	require.Empty(t, got)
}
