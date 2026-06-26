package lnd

import (
	"encoding/binary"
	"testing"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/htlcswitch/hop"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/reputation"
	"github.com/stretchr/testify/require"
)

// fakeCircuitSource is a test double for the switch's open-circuit snapshot.
type fakeCircuitSource struct {
	circuits []*htlcswitch.PaymentCircuit
}

func (f *fakeCircuitSource) ActiveCircuits() []*htlcswitch.PaymentCircuit {
	return f.circuits
}

// fakeChannelSource is a test double for the open-channel source.
type fakeChannelSource struct {
	chans []*channeldb.OpenChannel
	err   error
}

func (f *fakeChannelSource) FetchAllOpenChannels() (
	[]*channeldb.OpenChannel, error) {

	return f.chans, f.err
}

// makeHTLC builds an active commitment HTLC. The onion blob is made unique per
// (incoming, htlcIndex) so that ActiveHtlcs' both-commitments onion-hash match
// works and distinct HTLCs don't collide.
func makeHTLC(incoming bool, htlcIndex uint64, cltv uint32,
	accountable bool) channeldb.HTLC {

	var onion [lnwire.OnionPacketSize]byte
	binary.BigEndian.PutUint64(onion[:8], htlcIndex)
	if incoming {
		onion[8] = 1
	}

	records := lnwire.CustomRecords{}
	if accountable {
		records[uint64(lnwire.ExperimentalAccountableType)] =
			[]byte{lnwire.ExperimentalAccountable}
	} else {
		records[uint64(lnwire.ExperimentalAccountableType)] =
			[]byte{lnwire.ExperimentalUnaccountable}
	}

	return channeldb.HTLC{
		Incoming:      incoming,
		HtlcIndex:     htlcIndex,
		RefundTimeout: cltv,
		OnionBlob:     onion,
		CustomRecords: records,
	}
}

// makeChannel builds an open channel with the given scid whose active HTLC set
// is the provided list (placed on both local and remote commitments so that
// ActiveHtlcs returns them).
func makeChannel(scid uint64, htlcs ...channeldb.HTLC) *channeldb.OpenChannel {
	c := &channeldb.OpenChannel{
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

// TestAssembleInFlightHTLCs checks that the circuit map ∪ channel HTLC join
// recovers cltv and the accountable bit, and filters non-forwards.
func TestAssembleInFlightHTLCs(t *testing.T) {
	t.Parallel()

	const (
		inScid  = uint64(100)
		outScid = uint64(200)
	)

	// A real forward: incoming HTLC index 5 on inScid, keystoned out on
	// outScid, accountable, cltv 800000.
	fwd := &htlcswitch.PaymentCircuit{
		Incoming:       inKey(inScid, 5),
		Outgoing:       outKey(outScid, 9),
		IncomingAmount: lnwire.MilliSatoshi(10_000),
		OutgoingAmount: lnwire.MilliSatoshi(9_000),
	}

	// A locally-initiated payment: incoming side is our own switch. Must be
	// filtered out.
	localPay := &htlcswitch.PaymentCircuit{
		Incoming: models.CircuitKey{ChanID: hop.Source, HtlcID: 1},
		Outgoing: outKey(outScid, 10),
	}

	// A half-circuit with no keystone (not yet forwarded). Filtered out.
	noKeystone := &htlcswitch.PaymentCircuit{
		Incoming: inKey(inScid, 6),
		Outgoing: nil,
	}

	// A forward whose incoming HTLC is no longer live on the commitment
	// (e.g. concurrently resolved). Skipped because cltv/accountable can't
	// be recovered.
	stale := &htlcswitch.PaymentCircuit{
		Incoming: inKey(inScid, 99),
		Outgoing: outKey(outScid, 11),
	}

	channels := []*channeldb.OpenChannel{
		makeChannel(
			inScid,
			makeHTLC(true, 5, 800000, true),
			// An outgoing HTLC on the same channel must be ignored
			// (only incoming HTLCs key the index).
			makeHTLC(false, 7, 700000, true),
		),
	}

	circuits := []*htlcswitch.PaymentCircuit{
		fwd, localPay, noKeystone, stale,
	}

	got := assembleInFlightHTLCs(circuits, channels)

	require.Len(t, got, 1)
	require.Equal(t, reputation.InFlightHTLC{
		Incoming:     inKey(inScid, 5),
		Outgoing:     *outKey(outScid, 9),
		IncomingAmt:  lnwire.MilliSatoshi(10_000),
		OutgoingAmt:  lnwire.MilliSatoshi(9_000),
		IncomingCltv: 800000,
		Accountable:  true,
	}, got[0])
}

// TestAssembleInFlightHTLCsUnaccountable checks the accountable bit is
// recovered as false when the incoming HTLC carries the unaccountable value (or
// no record).
func TestAssembleInFlightHTLCsUnaccountable(t *testing.T) {
	t.Parallel()

	const inScid, outScid = uint64(100), uint64(200)

	fwd := &htlcswitch.PaymentCircuit{
		Incoming: inKey(inScid, 3),
		Outgoing: outKey(outScid, 4),
	}

	channels := []*channeldb.OpenChannel{
		makeChannel(inScid, makeHTLC(true, 3, 650000, false)),
	}

	got := assembleInFlightHTLCs(
		[]*htlcswitch.PaymentCircuit{fwd}, channels,
	)

	require.Len(t, got, 1)
	require.False(t, got[0].Accountable)
	require.Equal(t, uint32(650000), got[0].IncomingCltv)
}

// TestReconstructInFlightHTLCs checks the source wiring and error propagation.
func TestReconstructInFlightHTLCs(t *testing.T) {
	t.Parallel()

	const inScid, outScid = uint64(100), uint64(200)

	switchSrc := &fakeCircuitSource{
		circuits: []*htlcswitch.PaymentCircuit{
			{
				Incoming: inKey(inScid, 1),
				Outgoing: outKey(outScid, 2),
			},
		},
	}
	chanSrc := &fakeChannelSource{
		chans: []*channeldb.OpenChannel{
			makeChannel(inScid, makeHTLC(true, 1, 700000, true)),
		},
	}

	got, err := reconstructInFlightHTLCs(switchSrc, chanSrc)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, inKey(inScid, 1), got[0].Incoming)
	require.True(t, got[0].Accountable)

	// Errors from the channel source propagate.
	errSrc := &fakeChannelSource{err: errFetch}
	_, err = reconstructInFlightHTLCs(switchSrc, errSrc)
	require.ErrorIs(t, err, errFetch)
}

var errFetch = errTest("fetch failed")

type errTest string

func (e errTest) Error() string { return string(e) }
