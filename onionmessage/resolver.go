package onionmessage

import (
	"context"
	"encoding/hex"
	"errors"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/neutrino/cache/lru"
	"github.com/lightningnetwork/lnd/aliasmgr"
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

// PrivateChannelLookup resolves the remote node of a channel from one of our
// local aliases. The resolver calls it only for a SCID in the alias range.
type PrivateChannelLookup func(scid lnwire.ShortChannelID) (
	*btcec.PublicKey, bool)

// GraphNodeResolver resolves node public keys from short channel IDs using the
// announced channels in the graph, then our local aliases. It maintains an LRU
// cache to avoid repeated database lookups for frequently used SCIDs.
type GraphNodeResolver struct {
	graph  *graphdb.ChannelGraph
	ourPub *btcec.PublicKey

	// privateChannels resolves our local aliases.
	privateChannels PrivateChannelLookup

	// scidCache is an LRU cache mapping SCID (as uint64) to the remote
	// node's compressed public key bytes.
	scidCache *lru.Cache[uint64, *cachedPubKey]
}

// NewGraphNodeResolver creates a new GraphNodeResolver with the given channel
// graph and our node's public key. It initializes an LRU cache for SCID
// lookups.
func NewGraphNodeResolver(graph *graphdb.ChannelGraph,
	ourPub *btcec.PublicKey,
	privateChannels PrivateChannelLookup) *GraphNodeResolver {

	return &GraphNodeResolver{
		graph:           graph,
		ourPub:          ourPub,
		privateChannels: privateChannels,
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

	// The graph also holds our own unannounced channels. An edge without
	// an auth proof is one of them, so it resolves only by local alias.
	if errors.Is(err, graphdb.ErrEdgeNotFound) ||
		(err == nil && edge.AuthProof == nil) {

		return r.resolveLocalAlias(scid)
	}
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

	var keyBytes [33]byte
	copy(keyBytes[:], otherNodeKeyBytes[:])
	r.cachePubKey(scidInt, keyBytes)

	log.Tracef("Resolved SCID %v to node %s", scid,
		hex.EncodeToString(pubKey.SerializeCompressed()))

	return pubKey, nil
}

// resolveLocalAlias resolves a SCID that is not an announced channel. BOLT 4
// resolves a next-hop short_channel_id only if it "corresponds to an announced
// short_channel_id or a local alias for a channel". The confirmed SCID of an
// unannounced channel is neither, so it is refused. A non-alias miss returns
// before the local lookup, so a random SCID does not scan any channel set.
func (r *GraphNodeResolver) resolveLocalAlias(
	scid lnwire.ShortChannelID) (*btcec.PublicKey, error) {

	if !aliasmgr.IsAlias(scid) || r.privateChannels == nil {
		log.Debugf("SCID %v is not an announced channel or a local "+
			"alias", scid)

		return nil, ErrSCIDNotResolved
	}

	pubKey, ok := r.privateChannels(scid)
	if !ok || pubKey == nil {
		log.Debugf("SCID %v is not a local alias of an active channel",
			scid)

		return nil, ErrSCIDNotResolved
	}

	var keyBytes [33]byte
	copy(keyBytes[:], pubKey.SerializeCompressed())
	r.cachePubKey(scid.ToUint64(), keyBytes)

	return pubKey, nil
}

// cachePubKey caches the remote node key of a resolved SCID. We ignore the
// return values as caching is best-effort and a failure just means the next
// lookup will hit the database again.
func (r *GraphNodeResolver) cachePubKey(scid uint64, keyBytes [33]byte) {
	_, _ = r.scidCache.Put(scid, &cachedPubKey{pubKeyBytes: keyBytes})
}
