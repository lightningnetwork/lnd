package onionmessage

import (
	"context"
	"encoding/hex"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/neutrino/cache/lru"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/lnwire"
)

const (
	// defaultSCIDCacheSize is the default number of SCID to pubkey mappings
	// to cache. This is relatively small since onion message forwarding via
	// SCID is expected to be infrequent compared to forwarding via explicit
	// node ID.
	defaultSCIDCacheSize = 1000
)

// cachedPubKey is a wrapper around a compressed public key that implements the
// cache.Value interface required by the LRU cache.
type cachedPubKey struct {
	pubKeyBytes [33]byte
}

// Size returns the "size" of an entry. We return 1 as we just want to limit
// the total number of entries rather than do accurate size accounting.
func (c *cachedPubKey) Size() (uint64, error) {
	return 1, nil
}

// GraphNodeResolver resolves node public keys from short channel IDs using the
// channel graph. It maintains an LRU cache to avoid repeated database lookups
// for frequently used SCIDs.
type GraphNodeResolver struct {
	graph  *graphdb.ChannelGraph
	ourPub *btcec.PublicKey

	// scidCache is an LRU cache mapping SCID (as uint64) to the remote
	// node's compressed public key bytes.
	scidCache *lru.Cache[uint64, *cachedPubKey]
}

// NewGraphNodeResolver creates a new GraphNodeResolver with the given channel
// graph and our node's public key. It initializes an LRU cache for SCID
// lookups.
func NewGraphNodeResolver(graph *graphdb.ChannelGraph,
	ourPub *btcec.PublicKey) *GraphNodeResolver {

	return &GraphNodeResolver{
		graph:  graph,
		ourPub: ourPub,
		scidCache: lru.NewCache[uint64, *cachedPubKey](
			defaultSCIDCacheSize,
		),
	}
}

// RemotePubFromSCID resolves a node public key from a short channel ID.
func (r *GraphNodeResolver) RemotePubFromSCID(ctx context.Context,
	scid lnwire.ShortChannelID) (*btcec.PublicKey, error) {

	scidInt := scid.ToUint64()

	// Check the cache first.
	if cached, err := r.scidCache.Get(scidInt); err == nil {
		pubKey, parseErr := btcec.ParsePubKey(cached.pubKeyBytes[:])
		if parseErr == nil {
			log.Tracef("Resolved SCID %v from cache to node %s",
				scid,
				hex.EncodeToString(cached.pubKeyBytes[:]))

			return pubKey, nil
		}

		// Cache contained invalid data, fall through to DB lookup.
		log.Debugf("Invalid cached pubkey for SCID %v: %v",
			scid, parseErr)
	}

	log.Tracef("Resolving node public key for SCID %v from graph", scid)

	edge, _, _, err := r.graph.FetchChannelEdgesByID(ctx, scid.ToUint64())
	if err != nil {
		log.Debugf("Failed to fetch channel edges for SCID %v: %v",
			scid, err)

		return nil, err
	}

	otherNodeKeyBytes, err := edge.OtherNodeKeyBytes(
		r.ourPub.SerializeCompressed(),
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

	// Cache the result for future lookups. We ignore the return values as
	// caching is best-effort and a failure just means the next lookup will
	// hit the database again.
	_, _ = r.scidCache.Put(scidInt, &cachedPubKey{
		pubKeyBytes: otherNodeKeyBytes,
	})

	log.Tracef("Resolved SCID %v to node %s", scid,
		hex.EncodeToString(pubKey.SerializeCompressed()))

	return pubKey, nil
}
