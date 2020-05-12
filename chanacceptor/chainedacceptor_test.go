package chanacceptor

import (
	"errors"
	"testing"

	"github.com/btcsuite/btcd/btcec"
	"github.com/lightningnetwork/lnd/lnwire"
)

var (
	errUnwantedNode    = errors.New("unwanted node")
	errUnwantedChannel = errors.New("unwanted channel")
)

// unwantedNodeRejector rejects ChannelAcceptRequest's from unwanted node.
type unwantedNodeRejector struct {
	unwantedNodeID *btcec.PublicKey
}

func (a unwantedNodeRejector) Accept(req *ChannelAcceptRequest) error {
	if req.Node.IsEqual(a.unwantedNodeID) {
		return errUnwantedNode
	}

	return nil
}

// unwantedChannelRejector rejects ChannelAcceptRequest's for unwanted channels.
type unwantedChannelRejector struct {
	unwantedChannelID [32]byte
}

func (a unwantedChannelRejector) Accept(req *ChannelAcceptRequest) error {
	if req.OpenChanMsg.PendingChannelID == a.unwantedChannelID {
		return errUnwantedChannel
	}

	return nil
}

// customAcceptor accepts all ChannelAcceptRequest's.
type customAcceptor struct{}

func (a customAcceptor) Accept(req *ChannelAcceptRequest) error {
	return nil
}

// TestChainedAcceptorRejectingAcceptor tests that ChainedAcceptor containing
// an acceptor that rejects a ChannelAcceptRequest returns an error. If there
// are multiple acceptors that would reject a request, the error returned by
// first of them will be returned.
func TestChainedAcceptorRejectingAcceptor(t *testing.T) {
	var (
		nodeA = randKey(t)
		nodeB = randKey(t)

		reqNodeA = &ChannelAcceptRequest{
			Node: nodeA,
			OpenChanMsg: &lnwire.OpenChannel{
				PendingChannelID: [32]byte{0},
			},
		}

		reqNodeB = &ChannelAcceptRequest{
			Node: nodeB,
			OpenChanMsg: &lnwire.OpenChannel{
				PendingChannelID: [32]byte{1},
			},
		}
	)

	allowingAcceptor := customAcceptor{}
	rejectingAcceptor := unwantedNodeRejector{nodeB}
	rejectingAcceptor2 := unwantedChannelRejector{
		reqNodeA.OpenChanMsg.PendingChannelID}

	tests := []struct {
		name      string
		acceptors []ChannelAcceptor
		request   *ChannelAcceptRequest
		result    error
	}{
		// Channel acceptor accepts all channels. Open channel request
		// from NodeA is accepted.
		{
			name:      "success allow all nodeA",
			acceptors: []ChannelAcceptor{allowingAcceptor},
			request:   reqNodeA,
			result:    nil,
		},

		// Channel acceptor accepts all channels. Open channel request
		// from NodeB is accepted.
		{
			name:      "success allow all nodeB",
			acceptors: []ChannelAcceptor{allowingAcceptor},
			request:   reqNodeB,
			result:    nil,
		},

		// Channel acceptor rejects node by node ID. Open channel
		// request from NodeA is accepted.
		{
			name:      "success don't reject nodeA",
			acceptors: []ChannelAcceptor{rejectingAcceptor},
			request:   reqNodeA,
			result:    nil,
		},

		// Channel acceptor rejects node by node ID. Open channel
		// request from NodeB is rejected.
		{
			name:      "error reject unwanted node nodeB",
			acceptors: []ChannelAcceptor{rejectingAcceptor},
			request:   reqNodeB,
			result:    errUnwantedNode,
		},

		// Channel acceptor rejects channel by ID. Open channel request
		// from NodeA is rejected.
		{
			name:      "error reject unwanted channel nodeA",
			acceptors: []ChannelAcceptor{rejectingAcceptor2},
			request:   reqNodeA,
			result:    errUnwantedChannel,
		},

		// Channel acceptor rejects channel by ID. Open channel request
		// from NodeB is accepted.
		{
			name:      "success don't reject nodeB",
			acceptors: []ChannelAcceptor{rejectingAcceptor2},
			request:   reqNodeB,
			result:    nil,
		},

		// Multiple channel acceptors. First one allows all requests,
		// second one rejects by node ID but allows nodeA.
		{
			name: "success allow+reject nodeA",
			acceptors: []ChannelAcceptor{
				allowingAcceptor,
				rejectingAcceptor,
			},
			request: reqNodeA,
			result:  nil,
		},

		// Multiple channel acceptors. First one allows all requests,
		// second one rejects by node ID and rejects nodeB.
		{
			name: "error allow+reject nodeB",
			acceptors: []ChannelAcceptor{
				allowingAcceptor,
				rejectingAcceptor,
			},
			request: reqNodeB,
			result:  errUnwantedNode,
		},

		// Multiple channel acceptors:
		// 1. Allows all requests.
		// 2. Rejects requests by node ID.
		// 3. Rejects requests by channel ID.
		// Request from nodeB is rejected.
		{
			name: "error allow+reject+reject nodeB",
			acceptors: []ChannelAcceptor{
				allowingAcceptor,
				rejectingAcceptor,
				rejectingAcceptor2,
			},
			request: reqNodeB,
			result:  errUnwantedNode,
		},

		// Multiple channel acceptors:
		// 1. Allows all requests.
		// 2. Rejects requests by node ID.
		// 3. Rejects requests by channel ID.
		// Request from nodeA is rejected.
		{
			name: "error allow+reject+reject nodeA",
			acceptors: []ChannelAcceptor{
				allowingAcceptor,
				rejectingAcceptor2,
				rejectingAcceptor,
			},
			request: reqNodeA,
			result:  errUnwantedChannel,
		},
	}

	for _, test := range tests {
		t.Logf("Running test case: %v", test.name)

		chainedAcceptor := NewChainedAcceptor()

		for _, acceptor := range test.acceptors {
			chainedAcceptor.AddAcceptor(acceptor)
		}

		got := chainedAcceptor.Accept(test.request)
		if got != test.result {
			t.Errorf(
				"expected chainAcceptor.Accept to return: %v, "+
					" got: %v",
				test.result, got)
		}
	}
}
