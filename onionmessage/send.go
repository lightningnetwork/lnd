package onionmessage

import (
	"bytes"
	"context"
	"errors"

	"github.com/btcsuite/btcd/btcec/v2"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/fn/v2"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/routing/route"
)

const (
	// defaultMaxOnionMessageHops is the maximum hop count passed to
	// FindPath. Matches the BOLT 4 onion message limit.
	defaultMaxOnionMessageHops = 20
)

// Sender constructs and delivers onion messages to a destination using
// graph-based BFS pathfinding. If the graph yields no route
// (ErrNoPathFound), it falls back to a direct send to the destination peer.
type Sender struct {
	// graph provides read-only graph sessions for BFS pathfinding.
	graph GraphSessionProvider

	// selfKey is this node's pubkey, used as the BFS source.
	selfKey route.Vertex

	// peerSender delivers the constructed onion message to the first hop.
	peerSender PeerMessageSender
}

// NewSender creates a new Sender.
func NewSender(graph GraphSessionProvider, selfKey route.Vertex,
	peerSender PeerMessageSender) *Sender {

	return &Sender{
		graph:      graph,
		selfKey:    selfKey,
		peerSender: peerSender,
	}
}

// Send finds a path to destination and delivers the onion message.
// finalHopTLVs and replyPath may be nil.
//
// Error handling:
//   - ErrDestinationNoOnionSupport: returned as-is; no fallback attempted.
//   - ErrNoPathFound: destination supports onion messages but no graph route
//     exists; a direct send to the destination peer is attempted.
//   - Other graph errors: returned as-is.
func (s *Sender) Send(ctx context.Context, destination route.Vertex,
	finalHopTLVs []*lnwire.FinalHopTLV,
	replyPath *sphinx.BlindedPath) error {

	if destination == s.selfKey {
		return ErrCannotSendToSelf
	}

	var path onionMessagePath
	err := s.graph.GraphSession(
		ctx,
		func(graph graphdb.NodeTraverser) error {
			var findErr error
			path, findErr = FindPath(
				ctx, graph, s.selfKey, destination,
				defaultMaxOnionMessageHops,
			)

			return findErr
		},
		func() { path = nil },
	)

	switch {
	case errors.Is(err, ErrNoPathFound),
		errors.Is(err, ErrNodeNotFound):
		// No graph route (or destination not in graph), attempt a
		// direct send to the destination peer.
		log.Debugf("No graph path to %v, attempting direct send",
			destination)

		return s.sendViaPath(
			[]route.Vertex{destination}, finalHopTLVs, replyPath,
		)

	case err != nil:
		return err
	}

	return s.sendViaPath(path, finalHopTLVs, replyPath)
}

// sendViaPath builds a blinded path from hops, wraps it in an onion packet,
// and delivers it to the first hop via peerSender.
func (s *Sender) sendViaPath(hops []route.Vertex,
	finalHopTLVs []*lnwire.FinalHopTLV,
	replyPath *sphinx.BlindedPath) error {

	if len(hops) == 0 {
		return ErrNoHopsProvided
	}

	hopInfos, err := buildHopInfos(hops)
	if err != nil {
		return err
	}

	sessionKey, err := btcec.NewPrivateKey()
	if err != nil {
		return err
	}

	blindedPathInfo, err := sphinx.BuildBlindedPath(sessionKey, hopInfos)
	if err != nil {
		return err
	}

	sphinxPath, err := route.OnionMessageBlindedPathToSphinxPath(
		blindedPathInfo.Path, replyPath, finalHopTLVs,
	)
	if err != nil {
		return err
	}

	onionSessionKey, err := btcec.NewPrivateKey()
	if err != nil {
		return err
	}

	onionPkt, err := sphinx.NewOnionPacket(
		sphinxPath, onionSessionKey, nil,
		sphinx.DeterministicPacketFiller,
		sphinx.WithMaxPayloadSize(sphinx.MaxRoutingPayloadSize),
	)
	if err != nil {
		return err
	}

	var buf bytes.Buffer
	if err := onionPkt.Encode(&buf); err != nil {
		return err
	}

	msg := lnwire.NewOnionMessage(
		blindedPathInfo.SessionKey.PubKey(), buf.Bytes(),
	)

	var firstHop [33]byte
	copy(firstHop[:], hops[0][:])

	return s.peerSender.SendToPeer(firstHop, msg)
}

// buildHopInfos converts a path into sphinx HopInfo entries for blinded path
// construction. Each intermediate hop carries the next node's pubkey as its
// route data; the final hop carries empty route data.
func buildHopInfos(hops []route.Vertex) ([]*sphinx.HopInfo, error) {
	hopInfos := make([]*sphinx.HopInfo, len(hops))

	for i, hop := range hops {
		pub, err := btcec.ParsePubKey(hop[:])
		if err != nil {
			return nil, err
		}

		var routeData *record.BlindedRouteData
		if i < len(hops)-1 {
			nextPub, err := btcec.ParsePubKey(hops[i+1][:])
			if err != nil {
				return nil, err
			}

			//nolint:ll
			routeData = record.NewNonFinalBlindedRouteDataOnionMessage(
				fn.NewLeft[*btcec.PublicKey, lnwire.ShortChannelID](
					nextPub,
				),
				nil, nil,
			)
		} else {
			routeData = &record.BlindedRouteData{}
		}

		plaintext, err := record.EncodeBlindedRouteData(routeData)
		if err != nil {
			return nil, err
		}

		hopInfos[i] = &sphinx.HopInfo{
			NodePub:   pub,
			PlainText: plaintext,
		}
	}

	return hopInfos, nil
}
