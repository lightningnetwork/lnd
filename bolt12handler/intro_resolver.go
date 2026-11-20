package bolt12handler

import (
	"context"
	"encoding/binary"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/lnwire"
)

// IntroNodeResolver finds the node of a sciddir introduction node.
type IntroNodeResolver interface {
	// ResolveSciddir returns the key of the node that intro names.
	ResolveSciddir(ctx context.Context,
		intro lnwire.SciddirIntro) (*btcec.PublicKey, error)
}

// ChannelEdgeFetcher looks up a channel in the channel graph.
type ChannelEdgeFetcher interface {
	// FetchChannelEdgesByID returns the channel with the given short
	// channel ID and its two policies.
	FetchChannelEdgesByID(ctx context.Context, chanID uint64) (
		*models.ChannelEdgeInfo, *models.ChannelEdgePolicy,
		*models.ChannelEdgePolicy, error)
}

// GraphIntroResolver resolves a sciddir introduction node through the channel
// graph. It knows only the channels in the graph of this node.
type GraphIntroResolver struct {
	graph ChannelEdgeFetcher
}

// NewGraphIntroResolver creates a GraphIntroResolver that reads graph.
func NewGraphIntroResolver(graph ChannelEdgeFetcher) *GraphIntroResolver {
	return &GraphIntroResolver{graph: graph}
}

// ResolveSciddir returns the key of the node at the side of the channel that
// intro names. Direction 0 names node_1, which has the lesser key, and
// direction 1 names node_2.
//
// NOTE: This is part of the IntroNodeResolver interface.
func (r *GraphIntroResolver) ResolveSciddir(ctx context.Context,
	intro lnwire.SciddirIntro) (*btcec.PublicKey, error) {

	scid := binary.BigEndian.Uint64(intro.SCID[:])

	edge, _, _, err := r.graph.FetchChannelEdgesByID(ctx, scid)
	if err != nil {
		return nil, fmt.Errorf("fetch channel %v: %w",
			lnwire.NewShortChanIDFromInt(scid), err)
	}

	nodeKey := edge.NodeKey1Bytes
	if intro.Direction == 1 {
		nodeKey = edge.NodeKey2Bytes
	}

	return btcec.ParsePubKey(nodeKey[:])
}

// resolveIntroNode returns path with a pubkey introduction node. It resolves
// a sciddir introduction node with resolver and returns any other path as it
// is.
func resolveIntroNode(ctx context.Context, path *lnwire.BlindedPath,
	resolver IntroNodeResolver) (*lnwire.BlindedPath, error) {

	sciddir, ok := path.IntroductionNode.(lnwire.SciddirIntro)
	if !ok {
		return path, nil
	}

	pubkey, err := resolver.ResolveSciddir(ctx, sciddir)
	if err != nil {
		return nil, fmt.Errorf("resolve sciddir introduction node: %w",
			err)
	}

	intro, err := lnwire.NewPubkeyIntro(pubkey)
	if err != nil {
		return nil, err
	}

	resolved := *path
	resolved.IntroductionNode = intro

	return &resolved, nil
}
