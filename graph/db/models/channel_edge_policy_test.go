package models

import (
	"math"
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// TestChanEdgePolicyFromWireV2 tests the gossip v2 branch of
// ChanEdgePolicyFromWire: the inbound-fee Option semantics, the rejection of
// fees that do not fit the graph's signed fee model, the direction carried by
// the sciddir, and the htlc_maximum_msat default.
func TestChanEdgePolicyFromWireV2(t *testing.T) {
	t.Parallel()

	const (
		scid     = uint64(1234)
		capacity = btcutil.Amount(1_000_000)
	)

	halfCapacity := lnwire.NewMSatFromSatoshis(capacity) / 2

	// The inbound fee TLVs are Options so a case can tell an absent TLV
	// apart from one that the sender encoded with the default value of
	// zero. Both must read back as no inbound fee.
	newUpdate := func(direction byte, inboundBase,
		inboundRate fn.Option[uint32],
		maxHTLC fn.Option[lnwire.MilliSatoshi]) *lnwire.ChannelUpdate2 {

		upd := &lnwire.ChannelUpdate2{}
		upd.ShortChannelID.Val = lnwire.SciddirIntro{
			Direction: direction,
			SCID:      [8]byte{0, 0, 0, 0, 0, 0, 0, 1},
		}
		upd.BlockHeight.Val = 100

		inboundBase.WhenSome(func(base uint32) {
			upd.InboundFeeBaseMsat = tlv.SomeRecordT(
				tlv.NewPrimitiveRecord[tlv.TlvType20, uint32](
					base,
				),
			)
		})
		inboundRate.WhenSome(func(rate uint32) {
			upd.InboundFeeProportionalMillionths = tlv.SomeRecordT(
				tlv.NewPrimitiveRecord[tlv.TlvType22, uint32](
					rate,
				),
			)
		})

		maxHTLC.WhenSome(func(m lnwire.MilliSatoshi) {
			upd.HTLCMaximumMsat = tlv.SomeRecordT(
				tlv.NewRecordT[
					tlv.TlvType14, lnwire.MilliSatoshi,
				](m),
			)
		})

		return upd
	}

	tests := []struct {
		name        string
		direction   byte
		inboundBase fn.Option[uint32]
		inboundRate fn.Option[uint32]
		maxHTLC     fn.Option[lnwire.MilliSatoshi]

		wantSecondPeer bool
		wantInboundFee fn.Option[lnwire.Fee]
		wantMaxHTLC    lnwire.MilliSatoshi
		wantErr        string
	}{
		{
			// Both TLVs absent is the no-surcharge default, which
			// must read back as None rather than a zero Fee.
			name:        "absent inbound fees",
			maxHTLC:     fn.Some(halfCapacity),
			wantMaxHTLC: halfCapacity,
		},
		{
			// An explicit zero means the same thing, so it must
			// also collapse to None.
			name:        "explicit zero inbound fees",
			inboundBase: fn.Some[uint32](0),
			inboundRate: fn.Some[uint32](0),
			maxHTLC:     fn.Some(halfCapacity),
			wantMaxHTLC: halfCapacity,
		},
		{
			name:           "non-zero inbound base only",
			inboundBase:    fn.Some[uint32](7),
			maxHTLC:        fn.Some(halfCapacity),
			wantInboundFee: fn.Some(lnwire.Fee{BaseFee: 7}),
			wantMaxHTLC:    halfCapacity,
		},
		{
			name:        "non-zero inbound rate only",
			inboundRate: fn.Some[uint32](9),
			maxHTLC:     fn.Some(halfCapacity),
			wantInboundFee: fn.Some(
				lnwire.Fee{FeeRate: 9},
			),
			wantMaxHTLC: halfCapacity,
		},
		{
			name:        "both inbound fees",
			inboundBase: fn.Some[uint32](7),
			inboundRate: fn.Some[uint32](9),
			maxHTLC:     fn.Some(halfCapacity),
			wantInboundFee: fn.Some(
				lnwire.Fee{BaseFee: 7, FeeRate: 9},
			),
			wantMaxHTLC: halfCapacity,
		},
		{
			name:        "inbound base above int32",
			inboundBase: fn.Some[uint32](math.MaxInt32 + 1),
			maxHTLC:     fn.Some(halfCapacity),
			wantErr:     "exceed signed 32-bit range",
		},
		{
			name:        "inbound rate above int32",
			inboundRate: fn.Some[uint32](math.MaxInt32 + 1),
			maxHTLC:     fn.Some(halfCapacity),
			wantErr:     "exceed signed 32-bit range",
		},
		{
			name:           "direction one is the second peer",
			direction:      1,
			maxHTLC:        fn.Some(halfCapacity),
			wantSecondPeer: true,
			wantMaxHTLC:    halfCapacity,
		},
		{
			// An absent htlc_maximum_msat takes the spec default
			// of half the channel capacity.
			name:        "absent max htlc",
			wantMaxHTLC: halfCapacity,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			upd := newUpdate(
				test.direction, test.inboundBase,
				test.inboundRate, test.maxHTLC,
			)

			// Guard the fixture itself: a case that means to
			// encode an explicit zero must produce a present
			// TLV, not an absent one.
			require.Equal(
				t, test.inboundBase.IsSome(),
				upd.InboundFeeBaseMsat.IsSome(),
			)
			require.Equal(
				t, test.inboundRate.IsSome(),
				upd.InboundFeeProportionalMillionths.IsSome(),
			)

			policy, err := ChanEdgePolicyFromWire(
				scid, capacity, upd,
			)
			if test.wantErr != "" {
				require.ErrorContains(t, err, test.wantErr)

				return
			}
			require.NoError(t, err)

			require.Equal(t, lnwire.GossipVersion2, policy.Version)
			require.Equal(t, scid, policy.ChannelID)
			require.EqualValues(t, 100, policy.LastBlockHeight)
			require.Equal(t, test.wantSecondPeer, policy.SecondPeer)
			require.Equal(t, test.wantInboundFee, policy.InboundFee)
			require.Equal(t, test.wantMaxHTLC, policy.MaxHTLC)
		})
	}
}

// TestChanEdgePolicyFromWireV2UnknownCapacity tests that an absent
// htlc_maximum_msat with an unknown capacity stores no limit, and that the
// cached policy then reports that the edge has none.
func TestChanEdgePolicyFromWireV2UnknownCapacity(t *testing.T) {
	t.Parallel()

	upd := &lnwire.ChannelUpdate2{}
	upd.ShortChannelID.Val = lnwire.SciddirIntro{
		SCID: [8]byte{0, 0, 0, 0, 0, 0, 0, 1},
	}
	upd.BlockHeight.Val = 100

	policy, err := ChanEdgePolicyFromWire(1234, 0, upd)
	require.NoError(t, err)
	require.Zero(t, policy.MaxHTLC)

	require.False(t, NewCachedPolicy(policy).HasMaxHTLC)
}
