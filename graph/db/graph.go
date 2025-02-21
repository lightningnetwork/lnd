package graphdb

import "github.com/lightningnetwork/lnd/kvdb"

// ChannelGraph is a layer above the graph's CRUD layer.
//
// NOTE: currently, this is purely a pass-through layer directly to the backing
// KVStore. Upcoming commits will move the graph cache out of the KVStore and
// into this layer so that the KVStore is only responsible for CRUD operations.
type ChannelGraph struct {
	*KVStore
}

// NewChannelGraph creates a new ChannelGraph instance with the given backend.
func NewChannelGraph(db kvdb.Backend, options ...OptionModifier) (*ChannelGraph,
	error) {

	store, err := NewKVStore(db, options...)
	if err != nil {
		return nil, err
	}

	return &ChannelGraph{
		KVStore: store,
	}, nil
}
