package graphdb

import (
	"time"

	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

// Config is a struct that holds all the necessary dependencies for a
// ChannelGraph.
type Config struct {
	// KVDB is the kvdb.Backend that will be used for initializing the
	// KVStore CRUD layer.
	KVDB kvdb.Backend

	// KVStoreOpts is a list of functional options that will be used when
	// initializing the KVStore.
	KVStoreOpts []KVStoreOptionModifier
}

// ChannelGraph is a layer above the graph's CRUD layer.
//
// NOTE: currently, this is purely a pass-through layer directly to the backing
// KVStore. Upcoming commits will move the graph cache out of the KVStore and
// into this layer so that the KVStore is only responsible for CRUD operations.
type ChannelGraph struct {
	graphCache *GraphCache

	*KVStore
}

// NewChannelGraph creates a new ChannelGraph instance with the given backend.
func NewChannelGraph(cfg *Config, options ...ChanGraphOption) (*ChannelGraph,
	error) {

	opts := defaultChanGraphOptions()
	for _, o := range options {
		o(opts)
	}

	store, err := NewKVStore(cfg.KVDB, cfg.KVStoreOpts...)
	if err != nil {
		return nil, err
	}

	if !opts.useGraphCache {
		return &ChannelGraph{
			KVStore: store,
		}, nil
	}

	// The graph cache can be turned off (e.g. for mobile users) for a
	// speed/memory usage tradeoff.
	graphCache := NewGraphCache(opts.preAllocCacheNumNodes)
	startTime := time.Now()
	log.Debugf("Populating in-memory channel graph, this might take a " +
		"while...")

	err = store.ForEachNodeCacheable(func(node route.Vertex,
		features *lnwire.FeatureVector) error {

		graphCache.AddNodeFeatures(node, features)

		return nil
	})
	if err != nil {
		return nil, err
	}

	err = store.ForEachChannel(func(info *models.ChannelEdgeInfo,
		policy1, policy2 *models.ChannelEdgePolicy) error {

		graphCache.AddChannel(info, policy1, policy2)

		return nil
	})
	if err != nil {
		return nil, err
	}

	log.Debugf("Finished populating in-memory channel graph (took %v, %s)",
		time.Since(startTime), graphCache.Stats())

	store.setGraphCache(graphCache)

	return &ChannelGraph{
		KVStore:    store,
		graphCache: graphCache,
	}, nil
}
