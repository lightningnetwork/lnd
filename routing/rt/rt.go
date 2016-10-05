// Copyright (c) 2016 Bitfury Group Limited
// Distributed under the MIT software license, see the accompanying
// file LICENSE or http://www.opensource.org/licenses/mit-license.php

package rt

import (
	"fmt"
	"github.com/lightningnetwork/lnd/routing/rt/graph"
)

type OperationType byte
const (
	AddChannelOP OperationType = iota
	RemoveChannelOp
)

// String returns string representation
func (t OperationType) String()string {
	switch t {
	case AddChannelOP:
		return "<ADD_CHANNEL>"
	case RemoveChannelOp:
		return "<REMOVE_CHANNEL>"
	default:
		return "<UNKNOWN_TYPE>"
	}
}

// ChannelOperation represent operation on a graph
// Such as add edge or remove edge
type ChannelOperation struct {
	graph.Edge
	Operation OperationType // One of ADD_CHANNEL, REMOVE_CHANNEL
}

// NewChannelOperation returns new ChannelOperation
func NewChannelOperation(src, tgt graph.Vertex, id graph.EdgeID, info *graph.ChannelInfo, opType OperationType) ChannelOperation{
	return ChannelOperation{
		Edge: graph.Edge{
			Src: src,
			Tgt: tgt,
			Id: id,
			Info: info.Copy(),
		},
		Operation: opType,
	}
}

// String return string representation
func (c ChannelOperation) String () string {
	return fmt.Sprintf(
		"ChannelOperation[%v %v %v %v %v]",
		c.Src,
		c.Tgt,
		c.Id,
		c.Info,
		c.Operation,
	)
}

// Copy returns copy of ChannelOperation
func (op *ChannelOperation) Copy() ChannelOperation {
	return ChannelOperation{
		Edge: graph.Edge{
			Src: op.Src,
			Tgt: op.Tgt,
			Id: op.Id,
			Info: op.Info.Copy(),
		},
		Operation: op.Operation,
	}
}

// DifferenceBuffer represent multiple changes in a graph
// such as adding or deleting edges
type DifferenceBuffer []ChannelOperation

// String returns string representation
func (d DifferenceBuffer) String() string {
	s := ""
	for i:=0; i<len(d); i++ {
		s += fmt.Sprintf("  %v\n", d[i])
	}
	s1 := ""
	if len(d) > 0 {
		s1 = "\n"
	}
	return fmt.Sprintf("DifferenceBuffer[%v%v]", s1, s)
}

// NewDifferenceBuffer create new empty DifferenceBuffer
func NewDifferenceBuffer() *DifferenceBuffer {
	d := make(DifferenceBuffer, 0)
	return &d
}

// IsEmpty checks if buffer is empty(contains no operations in it)
func (diffBuff *DifferenceBuffer) IsEmpty() bool {
	return len(*diffBuff) == 0
}

// Clear deletes all operations from DifferenceBuffer making it empty
func (diffBuff *DifferenceBuffer) Clear() {
	*diffBuff = make([]ChannelOperation, 0)
}

// Copy create copy of a DifferenceBuffer
func (diffBuff *DifferenceBuffer) Copy() *DifferenceBuffer {
	d := make([]ChannelOperation, 0, len(*diffBuff))
	for _, op := range *diffBuff {
		d = append(d, op.Copy())
	}
	return (*DifferenceBuffer)(&d)
}

// RoutingTable represent information about graph and neighbors
// TODO(mkl): better doc
// Methods of this struct is not thread safe
type RoutingTable struct {
	// Contains node's view of lightning network
	G    *graph.Graph
	// Contains changes to send to a different neighbors
	// Changing RT modifies DifferenceBuffer
	diff []*DifferenceBuffer
}

// ShortestPath find shortest path between two node in routing table
func (rt *RoutingTable) ShortestPath(src, dst graph.Vertex) ([]graph.Vertex, error) {
	return graph.ShortestPath(rt.G, src, dst)
}

// NewRoutingTable creates new empty routing table
func NewRoutingTable() *RoutingTable {
	return &RoutingTable{
		G:    graph.NewGraph(),
		diff: make([]*DifferenceBuffer, 0),
	}
}

// AddNodes add multiple nodes to the routing table
func (rt *RoutingTable) AddNodes(IDs ...graph.Vertex) {
	for _, ID := range IDs {
		rt.G.AddVertex(ID)
	}
}

// AddChannel adds channel to the routing table.
// It will add nodes if they are not present in RT.
// It will update all difference buffers
func (rt *RoutingTable) AddChannel(Node1, Node2 graph.Vertex, edgeID graph.EdgeID, info *graph.ChannelInfo) {
	rt.AddNodes(Node1, Node2)
	if !rt.HasChannel(Node1, Node2, edgeID) {
		rt.G.AddUndirectedEdge(Node1, Node2, edgeID, info)
		for _, diffBuff := range rt.diff {
			*diffBuff = append(*diffBuff, NewChannelOperation(Node1, Node2, edgeID, info, AddChannelOP))
		}
	}
}

// HasChannel check if channel between two nodes exist in routing table
func (rt *RoutingTable) HasChannel(node1, node2 graph.Vertex, edgeID graph.EdgeID) bool {
	return rt.G.HasEdge(node1, node2, edgeID)
}

// RemoveChannel removes channel from the routing table.
// It will do nothing if channel does not exist in RT.
// It will update all difference buffers
func (rt *RoutingTable) RemoveChannel(Node1, Node2 graph.Vertex, edgeID graph.EdgeID) {
	if rt.HasChannel(Node1, Node2, edgeID) {
		rt.G.RemoveUndirectedEdge(Node1, Node2, edgeID)
		for _, diffBuff := range rt.diff {
			*diffBuff = append(*diffBuff, NewChannelOperation(Node1, Node2, edgeID, nil, RemoveChannelOp))
		}
	}
}

// NewDiffBuff create a new difference buffer in a routing table
func (rt *RoutingTable) NewDiffBuff() *DifferenceBuffer {
	buff := NewDifferenceBuffer()
	rt.diff = append(rt.diff, buff)
	return buff
}

// ApplyDiffBuff applies difference buffer to the routing table.
// It will modify RoutingTable's difference buffers.
func (rt *RoutingTable) ApplyDiffBuff(diffBuff *DifferenceBuffer) {
	for _, op := range *diffBuff {
		if op.Operation == AddChannelOP {
			rt.AddChannel(op.Src, op.Tgt, op.Id, op.Info)
		} else if op.Operation == RemoveChannelOp {
			rt.RemoveChannel(op.Src, op.Tgt, op.Id)
		}
	}
}

// Nodes return all nodes from routing table
func (rt *RoutingTable) Nodes() []graph.Vertex {
	return rt.G.GetVertexes()
}

// NumberOfNodes returns number of nodes in routing table
func (rt *RoutingTable) NumberOfNodes() int {
	return rt.G.GetVertexCount()
}

// AllChannels returns all channels from routing table
func (rt *RoutingTable) AllChannels() []graph.Edge {
	edges := rt.G.GetUndirectedEdges()
	return edges
}

// AddTable adds other RoutingTable.
// Resulting table contains channels, nodes from both tables.
func (rt *RoutingTable) AddTable(rt1 *RoutingTable) {
	newChannels := rt1.AllChannels()
	for _, channel := range newChannels {
		rt.AddChannel(channel.Src, channel.Tgt, channel.Id, channel.Info.Copy())
	}
}

// Copy - creates a copy of RoutingTable
// It makes deep copy of routing table.
// Note: difference buffers are not copied!
func (rt *RoutingTable) Copy() *RoutingTable {
	// TODO: add tests
	channels := rt.AllChannels()
	newRT := NewRoutingTable()
	newRT.AddNodes(rt.Nodes()...)
	for _, channel := range channels {
		newRT.AddChannel(channel.Src, channel.Tgt, channel.Id, channel.Info.Copy())
	}
	return newRT
}

// String returns string representation of routing table
func (rt *RoutingTable) String() string {
	rez := ""
	edges := rt.G.GetUndirectedEdges()
	for _, edge := range edges {
		rez += fmt.Sprintf("%v %v %v", edge.Src, edge.Tgt, edge.Info)
	}
	return rez
}

// Find k-shortest path between source and destination
func (rt *RoutingTable) KShortestPaths(src, dst graph.Vertex, k int) ([][]graph.Vertex, error) {
	return graph.KShortestPaths(rt.G, src, dst, k)
}

// Returns copy of channel info for channel in routing table
func (rt *RoutingTable) GetChannelInfo(id1, id2 graph.Vertex, edgeId graph.EdgeID) (*graph.ChannelInfo, error) {
	info, err := rt.G.GetInfo(id1, id2, edgeId)
	if err != nil {
		return nil, err
	}
	return info.Copy(), nil
}