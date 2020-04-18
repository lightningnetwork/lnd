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
// an acceptor that rejects a ChannelAcceptRequest returns an error. If there are
// multiple acceptors that would reject a request, the error returned by first
// of them will be returned.
func TestChainedAcceptorRejectingAcceptor(t *testing.T) {
	var (
		nodeA = randKey(t)
		nodeB = randKey(t)

		firstReq = &ChannelAcceptRequest{
			Node: nodeA,
			OpenChanMsg: &lnwire.OpenChannel{
				PendingChannelID: [32]byte{0},
			},
		}

		secondReq = &ChannelAcceptRequest{
			Node: nodeB,
			OpenChanMsg: &lnwire.OpenChannel{
				PendingChannelID: [32]byte{1},
			},
		}
	)

	allowingAcceptor := customAcceptor{}
	rejectingAcceptor := unwantedNodeRejector{nodeB}
	rejectingAcceptor2 := unwantedChannelRejector{firstReq.OpenChanMsg.PendingChannelID}

	tests := []struct {
		acceptors []ChannelAcceptor
		request   *ChannelAcceptRequest
		result    error
	}{
		{acceptors: []ChannelAcceptor{allowingAcceptor}, request: firstReq, result: nil},
		{acceptors: []ChannelAcceptor{allowingAcceptor}, request: secondReq, result: nil},
		{acceptors: []ChannelAcceptor{rejectingAcceptor}, request: firstReq, result: nil},
		{acceptors: []ChannelAcceptor{rejectingAcceptor}, request: secondReq, result: errUnwantedNode},
		{acceptors: []ChannelAcceptor{rejectingAcceptor2}, request: firstReq, result: errUnwantedChannel},
		{acceptors: []ChannelAcceptor{rejectingAcceptor2}, request: secondReq, result: nil},
		{
			acceptors: []ChannelAcceptor{
				allowingAcceptor,
				rejectingAcceptor,
			},
			request: firstReq,
			result:  nil},
		{
			acceptors: []ChannelAcceptor{
				allowingAcceptor,
				rejectingAcceptor,
			},
			request: secondReq,
			result:  errUnwantedNode},
		{
			acceptors: []ChannelAcceptor{
				allowingAcceptor,
				rejectingAcceptor,
				rejectingAcceptor2,
			},
			request: secondReq,
			result:  errUnwantedNode},
		{
			acceptors: []ChannelAcceptor{
				allowingAcceptor,
				rejectingAcceptor2,
				rejectingAcceptor,
			},
			request: firstReq,
			result:  errUnwantedChannel},
	}

	for _, test := range tests {
		chainedAcceptor := NewChainedAcceptor()

		for _, acceptor := range test.acceptors {
			chainedAcceptor.AddAcceptor(acceptor)
		}

		got := chainedAcceptor.Accept(test.request)
		if got != test.result {
			t.Errorf("expected chainAcceptor.Accept to return: %v, got: %v", test.result, got)
		}
	}
}
