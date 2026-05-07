package routerrpc

import (
	"context"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

// QueryProbability returns the current success probability estimate for a
// given node pair and amount.
func (s *Server) QueryProbability(_ context.Context,
	req *QueryProbabilityRequest) (*QueryProbabilityResponse, error) {

	fromNode, err := route.NewVertexFromBytes(req.FromNode)
	if err != nil {
		return nil, err
	}

	toNode, err := route.NewVertexFromBytes(req.ToNode)
	if err != nil {
		return nil, err
	}

	amt := lnwire.MilliSatoshi(req.AmtMsat)

	// Compute the probability.
	var prob float64
	mc := s.cfg.RouterBackend.MissionControl
	capacity, err := s.cfg.RouterBackend.FetchAmountPairCapacity(
		fromNode, toNode, amt,
	)

	// If we cannot query the capacity this means that either we don't have
	// information available or that the channel fails min/maxHtlc
	// constraints, so we return a zero probability.
	if err != nil {
		log.Errorf("Cannot fetch capacity: %v", err)
	} else {
		prob = mc.GetProbability(fromNode, toNode, amt, capacity)
	}

	history := mc.GetPairHistorySnapshot(fromNode, toNode)

	return &QueryProbabilityResponse{
		Probability: prob,
		History:     toRPCPairData(&history),
	}, nil
}
