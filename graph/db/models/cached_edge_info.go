package models

import "github.com/btcsuite/btcd/btcutil"

// CachedEdgeInfo is a struct that only caches the information of a
// ChannelEdgeInfo that we actually use for pathfinding and therefore need to
// store in the cache.
type CachedEdgeInfo struct {
	// ChannelID is the unique channel ID for the channel. The first 3
	// bytes are the block height, the next 3 the index within the block,
	// and the last 2 bytes are the output index for the channel.
	ChannelID uint64

	// NodeKey1Bytes is the raw public key of the first node.
	NodeKey1Bytes [33]byte

	// NodeKey2Bytes is the raw public key of the second node.
	NodeKey2Bytes [33]byte

	// Capacity is the total capacity of the channel, this is determined by
	// the value output in the outpoint that created this channel.
	Capacity btcutil.Amount
}

// NewCachedEdge creates a new CachedEdgeInfo from the provided ChannelEdgeInfo.
func NewCachedEdge(edgeInfo *ChannelEdgeInfo) *CachedEdgeInfo {
	return &CachedEdgeInfo{
		ChannelID:     edgeInfo.ChannelID,
		NodeKey1Bytes: edgeInfo.NodeKey1Bytes,
		NodeKey2Bytes: edgeInfo.NodeKey2Bytes,
		Capacity:      edgeInfo.Capacity,
	}
}
