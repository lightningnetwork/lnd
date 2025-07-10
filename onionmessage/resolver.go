package onionmessage

import (
	"github.com/btcsuite/btcd/btcec/v2"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/lnwire"
)

type GraphNodeResolver struct {
	Graph  *graphdb.ChannelGraph
	OurPub *btcec.PublicKey
}

func (r *GraphNodeResolver) RemotePubFromSCID(scid lnwire.ShortChannelID,
) (*btcec.PublicKey, error) {

	edge, _, _, err := r.Graph.FetchChannelEdgesByID(scid.ToUint64())
	if err != nil {
		return nil, err
	}

	otherNodeKeyBytes, err := edge.OtherNodeKeyBytes(
		r.OurPub.SerializeCompressed(),
	)
	if err != nil {
		return nil, err
	}

	return btcec.ParsePubKey(otherNodeKeyBytes[:])
}
