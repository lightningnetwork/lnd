//go:build test_db_postgres || test_db_sqlite

package graphdb

import (
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// TestChanUpdatesInHorizonWithNoPolicies tests that we're able to properly
// retrieve channels with no policies within the time range.
func TestChanUpdatesInHorizonWithNoPolicies(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	graph := MakeTestGraph(t)

	// We'll start by creating two nodes which will seed our test graph.
	node1 := createTestVertex(t)
	require.NoError(t, graph.AddNode(ctx, node1))

	node2 := createTestVertex(t)
	require.NoError(t, graph.AddNode(ctx, node2))

	// Note: startTime and  endTime only works if the channels have
	// policies. If not, the channels are included irrespective of the
	// time range.
	startTime := time.Unix(1234, 0)
	endTime := startTime
	edges := make([]ChannelEdge, 0, 10)

	// We'll now create 10 channels between the two nodes, half of them will
	// have no policies, and the other half will have policies.
	const numChans = 10
	for i := range numChans {
		channel, chanID := createEdge(
			uint32(i*10), 0, 0, 0, node1, node2,
		)
		require.NoError(t, graph.AddChannelEdge(ctx, &channel))

		// The first half of channels will have no policies.
		if i < numChans/2 {
			edges = append(edges, ChannelEdge{
				Info: &channel,
			})

			continue
		}

		edge1UpdateTime := endTime
		edge2UpdateTime := edge1UpdateTime.Add(time.Second)
		endTime = endTime.Add(time.Second * 10)

		edge1 := newEdgePolicy(
			chanID.ToUint64(), edge1UpdateTime.Unix(),
		)
		edge1.ChannelFlags = 0
		edge1.ToNode = node2.PubKeyBytes
		edge1.SigBytes = testSig.Serialize()
		require.NoError(t, graph.UpdateEdgePolicy(ctx, edge1))

		edge2 := newEdgePolicy(
			chanID.ToUint64(), edge2UpdateTime.Unix(),
		)
		edge2.ChannelFlags = 1
		edge2.ToNode = node1.PubKeyBytes
		edge2.SigBytes = testSig.Serialize()
		require.NoError(t, graph.UpdateEdgePolicy(ctx, edge2))

		edges = append(edges, ChannelEdge{
			Info:    &channel,
			Policy1: edge1,
			Policy2: edge2,
		})
	}

	// With our channels loaded, we'll now start our series of queries.
	queryCases := []struct {
		start time.Time
		end   time.Time
		resp  []ChannelEdge
	}{
		// If we query for a time range that's strictly below our set
		// of updates, then we'll get only the half of channels with no
		// policies.
		{
			start: time.Unix(100, 0),
			end:   time.Unix(200, 0),
			resp:  edges[:numChans/2],
		},

		// If we query for a time range that's well beyond our set of
		// updates, we should get only the half of channels with no
		// policies.
		{
			start: time.Unix(99999, 0),
			end:   time.Unix(999999, 0),
			resp:  edges[:numChans/2],
		},

		// If we query for the start time, and 10 seconds directly
		// after it, we should only get the half of channels with no
		// policies and one channel with a policy.
		{
			start: time.Unix(1234, 0),
			end:   startTime.Add(time.Second * 10),
			resp:  edges[:numChans/2+1],
		},

		// If we use the start and end time as is, we should get the
		// entire range.
		{
			start: startTime,
			end:   endTime,

			resp: edges[:numChans],
		},
	}

	for _, queryCase := range queryCases {
		respIter := graph.ChanUpdatesInHorizon(
			queryCase.start, queryCase.end,
		)

		resp, err := fn.CollectErr(respIter)
		require.NoError(t, err)
		require.Equal(t, len(resp), len(queryCase.resp))

		for i := range len(resp) {
			chanExp := queryCase.resp[i]
			chanRet := resp[i]

			require.Equal(t, chanExp.Info, chanRet.Info)
		}
	}
}
