package routing

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// describeGraphInboundFixture is a two node, one channel describegraph
// snapshot in the exact shape `lncli describegraph` emits: 64 bit fields as
// strings, the int32 inbound fee pair as bare numbers, and one direction
// announcing a discount while the other announces nothing at all.
const describeGraphInboundFixture = `{
  "nodes": [
    {"pub_key": "02a1633cafcc01ebfb6d78e39f687a1f0995c62fc95f51ead10a02ee0be551b5dc", "alias": "n1"},
    {"pub_key": "03a1633cafcc01ebfb6d78e39f687a1f0995c62fc95f51ead10a02ee0be551b5dc", "alias": "n2"}
  ],
  "edges": [
    {
      "channel_id": "1234",
      "capacity": "1000000",
      "node1_pub": "02a1633cafcc01ebfb6d78e39f687a1f0995c62fc95f51ead10a02ee0be551b5dc",
      "node2_pub": "03a1633cafcc01ebfb6d78e39f687a1f0995c62fc95f51ead10a02ee0be551b5dc",
      "node1_policy": {
        "time_lock_delta": 80,
        "min_htlc": "1000",
        "fee_base_msat": "0",
        "fee_rate_milli_msat": "1825",
        "max_htlc_msat": "990000000",
        "disabled": false,
        "inbound_fee_base_msat": -1000,
        "inbound_fee_rate_milli_msat": -1006
      },
      "node2_policy": {
        "time_lock_delta": 40,
        "min_htlc": "1000",
        "fee_base_msat": "1000",
        "fee_rate_milli_msat": "1",
        "max_htlc_msat": "990000000",
        "disabled": false
      }
    }
  ]
}`

// TestLoadInboundFees checks that the describegraph loader keeps the inbound
// fee pair it used to discard, and keeps it on the policy of the node that
// announced it. Getting the side wrong here would misprice every hub in the
// snapshot without failing a single test that looks only at totals.
func TestLoadInboundFees(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "graph.json")
	require.NoError(t, os.WriteFile(
		path, []byte(describeGraphInboundFixture), 0600,
	))

	graph, err := LoadSimGraphFromFile(path)
	require.NoError(t, err)

	channel, ok := graph.channels[1234]
	require.True(t, ok)

	// The announcing end carries the discount it announced, and the
	// end it faces carries nothing: an absent pair parses as the zero
	// fee, which is what 55,000 of the snapshot's policies announce.
	node1 := channel.ends[0]
	require.Equal(t, int32(-1000), node1.policy.InboundBaseMsat)
	require.Equal(t, int32(-1006), node1.policy.InboundRatePPM)
	require.True(t, node1.policy.hasInboundFee())

	node2 := channel.ends[1]
	require.Zero(t, node2.policy.InboundBaseMsat)
	require.Zero(t, node2.policy.InboundRatePPM)
	require.False(t, node2.policy.hasInboundFee())

	// Nothing else moved: the outbound fields are still the ones the
	// snapshot announced.
	require.EqualValues(t, 1825, node1.policy.FeeRatePPM)
	require.EqualValues(t, 1000, node2.policy.BaseFeeMsat)
}

// TestSimPolicyInboundFeeArithmetic pins the sim's inbound fee against the
// worked examples in lnd's own link test (TestChannelLinkInboundFee), which
// is the oracle for this arithmetic. The amount passed in is always the
// outgoing amount plus the outgoing fee, never the incoming amount.
func TestSimPolicyInboundFeeArithmetic(t *testing.T) {
	t.Parallel()

	// Bob forwards 1,000,000 msat and charges a 1,000 msat outbound fee,
	// so his inbound fee is computed on 1,001,000.
	const bobBase = 1_001_000

	cases := []struct {
		name     string
		base     int32
		rate     int32
		amt      int64
		expected int64
	}{{
		// A -500 msat base and a -100 ppm rate on 1,001,000 gives
		// -500 - 100 = -600, the rate component rounding up.
		name:     "negative",
		base:     -500,
		rate:     -100,
		amt:      bobBase,
		expected: -600,
	}, {
		// A discount larger than the outbound fee it nets against.
		// The policy still reports the full discount; capping it is
		// the forwarding check's job, not this method's.
		name:     "negative total",
		base:     -5_000,
		amt:      bobBase,
		expected: -5_000,
	}, {
		name:     "positive",
		base:     1_000,
		rate:     100_000,
		amt:      bobBase,
		expected: 101_100,
	}, {
		name: "absent",
		amt:  bobBase,
	}}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			policy := SimPolicy{
				InboundBaseMsat: testCase.base,
				InboundRatePPM:  testCase.rate,
			}

			require.Equal(t, testCase.expected, policy.inboundFee(
				lnwire.MilliSatoshi(testCase.amt),
			))
		})
	}
}

// describeGraphBalanceFixture is a three node, two channel graph in
// describegraph shape carrying the two fields a modelled graph adds: a
// node1-side balance and the modeller's certainty about it. The second
// channel names its ends in the opposite order to the first, so that a loader
// that assumed node1 is always the lexicographically smaller pubkey would put
// its balance on the wrong side here.
const describeGraphBalanceFixture = `{
  "nodes": [
    {"pub_key": "02aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "alias": "a"},
    {"pub_key": "03bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "alias": "b"},
    {"pub_key": "02cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", "alias": "c"}
  ],
  "edges": [
    {
      "channel_id": "1234",
      "capacity": "1000000",
      "balance": "250000",
      "balance_certainty": 0.75,
      "node1_pub": "02aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
      "node2_pub": "02cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
      "node1_policy": {
        "time_lock_delta": 40,
        "min_htlc": "1000",
        "fee_base_msat": "1000",
        "fee_rate_milli_msat": "1",
        "max_htlc_msat": "990000000"
      },
      "node2_policy": {
        "time_lock_delta": 40,
        "min_htlc": "1000",
        "fee_base_msat": "1000",
        "fee_rate_milli_msat": "1",
        "max_htlc_msat": "990000000"
      }
    },
    {
      "channel_id": "5678",
      "capacity": "1000000",
      "balance": "700000",
      "balance_certainty": 0,
      "node1_pub": "03bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
      "node2_pub": "02aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
      "node1_policy": {
        "time_lock_delta": 40,
        "min_htlc": "1000",
        "fee_base_msat": "1000",
        "fee_rate_milli_msat": "1",
        "max_htlc_msat": "990000000"
      },
      "node2_policy": {
        "time_lock_delta": 40,
        "min_htlc": "1000",
        "fee_base_msat": "1000",
        "fee_rate_milli_msat": "1",
        "max_htlc_msat": "990000000"
      }
    }
  ]
}`

// writeSimGraphFixture writes a graph fixture to a temporary file and loads
// it, which is the only way a graph balance can reach the simulator.
func writeSimGraphFixture(t *testing.T, fixture string) *SimGraph {
	t.Helper()

	path := filepath.Join(t.TempDir(), "graph.json")
	require.NoError(t, os.WriteFile(path, []byte(fixture), 0600))

	graph, err := LoadSimGraphFromFile(path)
	require.NoError(t, err)

	return graph
}

// TestLoadGraphBalances checks that the loader keeps the balances a modelled
// graph carries, on the side of the channel the file put them on, and that it
// keeps them somewhere the live network does not see: loading a graph must
// not assign its liquidity, or every scenario that names its own liquidity
// model would silently be scored on two.
func TestLoadGraphBalances(t *testing.T) {
	t.Parallel()

	graph := writeSimGraphFixture(t, describeGraphBalanceFixture)

	const capacityMsat = lnwire.MilliSatoshi(1_000_000_000)

	// The first channel names the lexicographically smaller pubkey as
	// node1, so its balance belongs to end zero.
	first, ok := graph.channels[1234]
	require.True(t, ok)
	require.True(t, first.ends[0].hasGraphBalance)
	require.True(t, first.ends[1].hasGraphBalance)
	require.EqualValues(t, 250_000_000, first.ends[0].graphBalance)
	require.EqualValues(t, 750_000_000, first.ends[1].graphBalance)
	require.Equal(t, 0.75, first.graphCertainty)

	// The second names them the other way around, so its balance belongs
	// to end one.
	second, ok := graph.channels[5678]
	require.True(t, ok)
	require.EqualValues(t, 300_000_000, second.ends[0].graphBalance)
	require.EqualValues(t, 700_000_000, second.ends[1].graphBalance)
	require.Zero(t, second.graphCertainty)

	// Nothing above is live yet. Both channels are still holding the
	// 50/50 split every freshly loaded channel holds.
	for _, channel := range []*SimChannel{first, second} {
		require.Equal(t, capacityMsat/2, channel.ends[0].balance)
		require.Equal(t, capacityMsat/2, channel.ends[1].balance)
	}
}

// TestLoadWithoutGraphBalances checks that a snapshot carrying no balances,
// which is every snapshot the program has scored so far, still loads and
// simply carries none.
func TestLoadWithoutGraphBalances(t *testing.T) {
	t.Parallel()

	graph := writeSimGraphFixture(t, describeGraphInboundFixture)

	channel, ok := graph.channels[1234]
	require.True(t, ok)
	require.False(t, channel.ends[0].hasGraphBalance)
	require.False(t, channel.ends[1].hasGraphBalance)
	require.Zero(t, channel.graphCertainty)
}

// TestLoadBalanceOverCapacity checks that a balance larger than the channel
// it sits in is refused at load time. A modelled graph is only worth scoring
// on if its balances mean what they say, so an impossible one is a corrupt
// file rather than an unusual world.
func TestLoadBalanceOverCapacity(t *testing.T) {
	t.Parallel()

	fixture := strings.Replace(
		describeGraphBalanceFixture, `"balance": "250000"`,
		`"balance": "1000001"`, 1,
	)

	path := filepath.Join(t.TempDir(), "graph.json")
	require.NoError(t, os.WriteFile(path, []byte(fixture), 0600))

	_, err := LoadSimGraphFromFile(path)
	require.ErrorContains(t, err, "outside its 1000000 sat capacity")
}
