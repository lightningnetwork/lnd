package htlcswitch

import (
	"encoding/binary"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/htlcswitch/hop"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// TestProcessRemoteAddsSparseAckFilterPreservesIndex asserts that replay
// retains each add's original forwarding-package index. Add 0 has resolved
// while add 1 remains forwarded, so compacting the unacked adds must not make
// add 1 use index 0 for its FwdFilter lookup or AddRef.
func TestProcessRemoteAddsSparseAckFilterPreservesIndex(t *testing.T) {
	const (
		chanAmt = btcutil.SatoshiPerBitcoin * 5
		height  = uint64(42)
	)

	harness, err := newSingleLinkTestHarness(t, chanAmt, 0)
	require.NoError(t, err)
	require.NoError(t, harness.start())
	t.Cleanup(func() {
		harness.aliceLink.Stop()
	})

	coreLink, ok := harness.aliceLink.(*channelLink)
	require.True(t, ok)
	nextHop := lnwire.NewShortChanIDFromInt(99)
	add0 := makeSparseReplayTestAdd(t, 100, nextHop)
	add1 := makeSparseReplayTestAdd(t, 101, nextHop)

	pkg := channeldb.NewFwdPkg(
		coreLink.ShortChanID(), height,
		[]channeldb.LogUpdate{
			{UpdateMsg: add0},
			{UpdateMsg: add1},
		}, nil,
	)
	pkg.State = channeldb.FwdStateProcessed
	pkg.AckFilter.Set(0)
	pkg.FwdFilter.Set(1)

	var forwarded []*htlcPacket
	coreLink.cfg.ForwardPackets = func(_ <-chan struct{}, _ bool,
		packets ...*htlcPacket) error {

		forwarded = append(forwarded, packets...)
		return nil
	}

	coreLink.processRemoteAdds(pkg)

	require.Len(t, forwarded, 1)
	require.Equal(t, add1.ID, forwarded[0].incomingHTLCID)
	require.NotNil(t, forwarded[0].sourceRef)
	require.Equal(t, uint16(1), forwarded[0].sourceRef.Index)
}

func makeSparseReplayTestAdd(t *testing.T, id uint64,
	nextHop lnwire.ShortChannelID) *lnwire.UpdateAddHTLC {

	t.Helper()

	amt := lnwire.NewMSatFromSatoshis(10_000)
	expiry := uint32(testStartingHeight + testInvoiceCltvExpiry)

	var nextAddress [8]byte
	binary.BigEndian.PutUint64(nextAddress[:], nextHop.ToUint64())

	blob, err := generateRoute(hop.NewLegacyPayload(&sphinx.HopData{
		NextAddress:   nextAddress,
		ForwardAmount: uint64(amt),
		OutgoingCltv:  expiry,
	}))
	require.NoError(t, err)

	_, htlc, _, err := generatePayment(amt, amt, expiry, blob)
	require.NoError(t, err)
	htlc.ID = id

	return htlc
}
