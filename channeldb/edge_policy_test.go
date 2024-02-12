package channeldb

import (
	"bytes"
	"math/rand"
	"reflect"
	"testing"
	"testing/quick"
	"time"

	"github.com/lightningnetwork/lnd/channeldb/models"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEdgePolicySerialisation tests the serialisation and deserialization logic
// for models.ChannelEdgePolicy.
func TestEdgePolicySerialisation(t *testing.T) {
	t.Parallel()

	mainScenario := func(info models.ChannelEdgePolicy) bool {
		var (
			b      bytes.Buffer
			toNode = info.GetToNode()
		)

		encodingInfo, err := encodingInfoFromEdgePolicy(info)
		require.NoError(t, err)

		err = serializeChanEdgePolicy(&b, encodingInfo, toNode[:])
		require.NoError(t, err)

		newInfo, err := deserializeChanEdgePolicy(&b)
		require.NoError(t, err)

		return assert.Equal(t, info, newInfo)
	}

	tests := []struct {
		name     string
		genValue func([]reflect.Value, *rand.Rand)
		scenario any
	}{
		{
			name: "ChannelEdgePolicy1",
			scenario: func(m models.ChannelEdgePolicy1) bool {
				return mainScenario(&m)
			},
			genValue: func(v []reflect.Value, r *rand.Rand) {
				//nolint:lll
				policy := &models.ChannelEdgePolicy1{
					ChannelID:                 r.Uint64(),
					LastUpdate:                time.Unix(r.Int63(), 0),
					MessageFlags:              lnwire.ChanUpdateMsgFlags(r.Uint32()),
					ChannelFlags:              lnwire.ChanUpdateChanFlags(r.Uint32()),
					TimeLockDelta:             uint16(r.Uint32()),
					MinHTLC:                   lnwire.MilliSatoshi(r.Uint64()),
					FeeBaseMSat:               lnwire.MilliSatoshi(r.Uint64()),
					FeeProportionalMillionths: lnwire.MilliSatoshi(r.Uint64()),
					ExtraOpaqueData:           make([]byte, 0),
				}

				policy.SigBytes = make([]byte, r.Intn(80))
				_, err := r.Read(policy.SigBytes)
				require.NoError(t, err)

				_, err = r.Read(policy.ToNode[:])
				require.NoError(t, err)

				numExtraBytes := r.Int31n(1000)
				if numExtraBytes > 0 {
					policy.ExtraOpaqueData = make(
						[]byte, numExtraBytes,
					)
					_, err := r.Read(
						policy.ExtraOpaqueData,
					)
					require.NoError(t, err)
				}

				// Sometimes add an MaxHTLC.
				if r.Intn(2)%2 == 0 {
					policy.MessageFlags |=
						lnwire.ChanUpdateRequiredMaxHtlc
					policy.MaxHTLC = lnwire.MilliSatoshi(
						r.Uint64(),
					)
				} else {
					policy.MessageFlags ^=
						lnwire.ChanUpdateRequiredMaxHtlc
				}

				v[0] = reflect.ValueOf(*policy)
			},
		},
		{
			name: "ChannelEdgePolicy2",
			scenario: func(m models.ChannelEdgePolicy2) bool {
				return mainScenario(&m)
			},
			genValue: func(v []reflect.Value, r *rand.Rand) {
				policy := &models.ChannelEdgePolicy2{
					//nolint:lll
					ChannelUpdate2: lnwire.ChannelUpdate2{
						Signature:       testSchnorrSig,
						ExtraOpaqueData: make([]byte, 0),
					},
					ToNode: [33]byte{},
				}

				policy.ShortChannelID.Val = lnwire.NewShortChanIDFromInt( //nolint:lll
					uint64(r.Int63()),
				)
				policy.BlockHeight.Val = r.Uint32()
				policy.HTLCMaximumMsat.Val = lnwire.MilliSatoshi( //nolint:lll
					r.Uint64(),
				)
				policy.HTLCMinimumMsat.Val = lnwire.MilliSatoshi( //nolint:lll
					r.Uint64(),
				)
				policy.CLTVExpiryDelta.Val = uint16(r.Int31())
				policy.FeeBaseMsat.Val = r.Uint32()
				policy.FeeProportionalMillionths.Val = r.Uint32() //nolint:lll

				if r.Intn(2) == 0 {
					policy.Direction.Val.B = true
				}

				// Sometimes set the incoming disabled flag.
				if r.Int31()%2 == 0 {
					policy.DisabledFlags.Val |=
						lnwire.ChanUpdateDisableIncoming
				}

				// Sometimes set the outgoing disabled flag.
				if r.Int31()%2 == 0 {
					policy.DisabledFlags.Val |=
						lnwire.ChanUpdateDisableOutgoing
				}

				_, err := r.Read(policy.ToNode[:])
				require.NoError(t, err)

				numExtraBytes := r.Int31n(1000)
				if numExtraBytes > 0 {
					policy.ExtraOpaqueData = make(
						[]byte, numExtraBytes,
					)
					_, err := r.Read(
						policy.ExtraOpaqueData,
					)
					require.NoError(t, err)
				}

				v[0] = reflect.ValueOf(*policy)
			},
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			config := &quick.Config{
				Values: test.genValue,
			}

			err := quick.Check(test.scenario, config)
			require.NoError(t, err)
		})
	}
}
