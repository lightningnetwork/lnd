package onionmessage

import (
	"context"
	"encoding/hex"

	"github.com/btcsuite/btcd/btcec/v2"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/lnwire"
)

type GraphNodeResolver struct {
	Graph  *graphdb.ChannelGraph
	OurPub *btcec.PublicKey
}

// RemotePubFromSCID resolves a node public key from a short channel ID.
func (r *GraphNodeResolver) RemotePubFromSCID(ctx context.Context,
	scid lnwire.ShortChannelID) (*btcec.PublicKey, error) {

	log.Tracef("Resolving node public key for SCID %v", scid)

	edge, _, _, err := r.Graph.FetchChannelEdgesByID(ctx, scid.ToUint64())
	if err != nil {
		log.Debugf("Failed to fetch channel edges for SCID %v: %v",
			scid, err)

		return nil, err
	}

	otherNodeKeyBytes, err := edge.OtherNodeKeyBytes(
		r.OurPub.SerializeCompressed(),
	)
	if err != nil {
		log.Debugf("Failed to get other node key for SCID %v: %v",
			scid, err)

		return nil, err
	}

	pubKey, err := btcec.ParsePubKey(otherNodeKeyBytes[:])
	if err != nil {
		log.Debugf("Failed to parse public key for SCID %v: %v",
			scid, err)

		return nil, err
	}

	log.Tracef("Resolved SCID %v to node %s", scid,
		hex.EncodeToString(pubKey.SerializeCompressed()))

	return pubKey, nil
}
