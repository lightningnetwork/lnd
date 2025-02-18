package graphdb

import "github.com/lightningnetwork/lnd/kvdb"

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
	*KVStore
}

// NewChannelGraph creates a new ChannelGraph instance with the given backend.
func NewChannelGraph(cfg *Config) (*ChannelGraph, error) {
	store, err := NewKVStore(cfg.KVDB, cfg.KVStoreOpts...)
	if err != nil {
		return nil, err
	}

	return &ChannelGraph{
		KVStore: store,
	}, nil
}
