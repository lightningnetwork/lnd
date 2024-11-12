package models

import "github.com/btcsuite/btcd/btcutil"

// NetworkStats represents various statistics about the state of the Lightning
// network graph.
type NetworkStats struct {
	// Diameter is the diameter of the graph, which is the length of the
	// longest shortest path between any two nodes in the graph.
	Diameter uint32

	// MaxChanOut is the maximum number of outgoing channels from a single
	// node.
	MaxChanOut uint32

	// NumNodes is the total number of nodes in the graph.
	NumNodes uint32

	// NumChannels is the total number of channels in the graph.
	NumChannels uint32

	// TotalNetworkCapacity is the total capacity of all channels in the
	// graph.
	TotalNetworkCapacity btcutil.Amount

	// MinChanSize is the smallest channel size in the graph.
	MinChanSize btcutil.Amount

	// MaxChanSize is the largest channel size in the graph.
	MaxChanSize btcutil.Amount

	// MedianChanSize is the median channel size in the graph.
	MedianChanSize btcutil.Amount

	// NumZombies is the number of zombie channels in the graph.
	NumZombies uint64
}
