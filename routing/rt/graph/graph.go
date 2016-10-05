// Copyright (c) 2016 Bitfury Group Limited
// Distributed under the MIT software license, see the accompanying
// file LICENSE or http://www.opensource.org/licenses/mit-license.php

package graph

import (
	"errors"
	"fmt"
	"math"
	"bytes"
	"github.com/roasbeef/btcd/wire"
)

var (
	// CyclicDataError represents internal error. When trying to
	// obtain explicit path after BFS. Following parents resulted
	// in a loop.
	CyclicDataError         = errors.New("Cyclic data")

	// PathNotFoundError represents not existing path
	// after search
	PathNotFoundError       = errors.New("Path not found")

	// NodeNotFoundError represents that required vertex
	// does not exist in the graph
	NodeNotFoundError       = errors.New("Node not found")

	// EdgeNotFoundError represents that requested edge
	// does not exist in the graph
	EdgeNotFoundError       = errors.New("Edge not found")
)

// Vertex represent graph vertex(node) in a
// lightning network
type Vertex struct {
	PubKey [33]byte
}

// NilVertex represents not existing vertex.
// This value should be used similar to nil
var NilVertex = Vertex{PubKey: [33]byte{}}

// NewVertex creates a new vertex from a compressed public key.
func NewVertex(v []byte) Vertex {
	x := Vertex{}
	copy(x.PubKey[:], v)
	return x
}

// String creates a string representation of a Vertex.
// Note: it does not hex encode.
func (s Vertex) String() string {
	return string(s.PubKey[:])
}


// ToByte33 returns [33]byte
// 33 - is usual byte length of a compressed
// public key
func (s Vertex) ToByte33() [33]byte {
	var rez [33]byte
	copy(rez[:], s.PubKey[:])
	return rez
}

// ToByte() returns byte representation of
// the vertex
func (s Vertex) ToByte() []byte {
	return s.PubKey[:]
}

// IsNil compares vertex to nil vertex
func (s Vertex) IsNil() bool {
	var z [33]byte
	return bytes.Equal(s.PubKey[:], z[:])
}

// EdgeID represent edge unique identifier.
type EdgeID wire.OutPoint

// NilEdgeID represent not existing EdgeID
var NilEdgeID = EdgeID{wire.ShaHash{}, 0}

// NewEdgeID returns new EdgeID
func NewEdgeID(v wire.OutPoint) EdgeID {
	return EdgeID(v)
}

// String returns string representation of EdgeID
func (e EdgeID) String() string {
	return wire.OutPoint(e).String()
}

// ChannelInfo contains information about edge(channel)
// like capacity or weight
type ChannelInfo struct {
	// Capacity in satoshi of the channel
	Cpt int64
	// Weight of the channel
	Wgt float64
}

// Copy() creates a copy of a ChannelInfo struct.
// If c==nil than it returns nil.
// This method is used to safely create copy of a structure
// given by a pointer which may be nil.
func (c *ChannelInfo) Copy() *ChannelInfo {
	if c == nil {
		return nil
	} else {
		c1 := *c
		return &c1
	}
}

// Edge represents edge in a graph
type Edge struct {
	// Source and Target
	Src, Tgt Vertex

	// Edge identifier
	Id       EdgeID

	// Additional information about edge
	Info     *ChannelInfo
}

// NilEdge represents nil (not-existing) edge
var NilEdge = Edge{NilVertex, NilVertex, NilEdgeID, nil}

// String returns string of an Edge
func (e Edge) String() string {
	return fmt.Sprintf("edge[%v %v %v %v]", e.Src, e.Tgt, e.Id, e.Info)
}

// NewEdge create a new edge
func NewEdge(src, tgt Vertex, id EdgeID, info *ChannelInfo) Edge {
	return Edge{
		Src:      src,
		Tgt:      tgt,
		Id:       id,
		Info: info,
	}
}

// Graph is multigraph implementation.
type Graph struct {
	adjacencyList map[Vertex]map[Vertex]map[EdgeID]*ChannelInfo
}

// NewGraph creates a new empty graph.
func NewGraph() *Graph {
	g := new(Graph)
	g.adjacencyList = make(map[Vertex]map[Vertex]map[EdgeID]*ChannelInfo)
	return g
}

// GetVertexCount returns number of vertexes in a graph.
func (g *Graph) GetVertexCount() int {
	return len(g.adjacencyList)
}

// GetVertexes returns all vertexes in a graph.
func (g *Graph) GetVertexes() []Vertex {
	IDs := make([]Vertex, 0, g.GetVertexCount())
	for ID := range g.adjacencyList {
		IDs = append(IDs, ID)
	}
	return IDs
}

// GetEdges return all edges in a graph.
// For undirected graph it returns each edge twice (for each direction)
// To get all edges in undirected graph use GetUndirectedEdges
func (g *Graph) GetEdges() []Edge {
	edges := make([]Edge, 0)
	for v1 := range g.adjacencyList {
		for v2, multiedges := range g.adjacencyList[v1] {
			for id, edge := range multiedges {
				edges = append(edges, NewEdge(v1, v2, id, edge))
			}
		}
	}
	return edges
}

// GetUndirectedEdges returns all edges in an undirected graph.
func (g *Graph) GetUndirectedEdges() []Edge {
	edges := make([]Edge, 0)
	for v1 := range g.adjacencyList {
		for v2, multiedges := range g.adjacencyList[v1] {
			if v1.String() <= v2.String() {
				for id, edge := range multiedges {
					edges = append(edges, NewEdge(v1, v2, id, edge))
				}
			}
		}
	}
	return edges
}

// HasVertex check if graph contain the given vertex.
func (g *Graph) HasVertex(v Vertex) bool {
	_, ok := g.adjacencyList[v]
	return ok
}

// HasEdge check if there is edge with  a given EdgeID
// between two given vertexes.
func (g *Graph) HasEdge(v1, v2 Vertex, edgeID EdgeID) bool {
	if _, ok := g.adjacencyList[v1]; ok {
		if multiedges, ok := g.adjacencyList[v1][v2]; ok {
			if _, ok := multiedges[edgeID]; ok {
				return true
			}
		}
	}
	return false
}

// AddVertex adds vertex  to a graph.
// If graph already contains this vertex it does nothing.
// Returns true if vertex previously doesnt't exist.
func (g *Graph) AddVertex(v Vertex) bool {
	if g.HasVertex(v) {
		return false
	}
	g.adjacencyList[v] = make(map[Vertex]map[EdgeID]*ChannelInfo)
	return true
}

// RemoveVertex removes vertex from a graph
// If a graph already does not contain this vertex it does nothing
// Returns true if previously existed vertex got deleted
// BUG(mkl): does it correctly deletes edges with this vertex
func (g *Graph) RemoveVertex(v Vertex) bool {
	if !g.HasVertex(v) {
		return false
	}
	delete(g.adjacencyList, v)
	return true
}

// AddEdge adds directed edge to the graph
// v1, v2 must exist. If they do not exist this function do nothing
// and return false. If edge with given vertexes and id already exists it
// gets overwritten
func (g *Graph) AddEdge(v1, v2 Vertex, edgeID EdgeID, info *ChannelInfo) bool {
	if !g.HasVertex(v1) || !g.HasVertex(v2) {
		return false
	}
	tmap := g.adjacencyList[v1]
	if tmap[v2] == nil {
		tmap[v2] = make(map[EdgeID]*ChannelInfo)
	}
	tmap[v2][edgeID] = info
	return true
}

// AddUndirectedEdge adds an undirected edge to the graph.
// Vertexes should exists.
func (g *Graph) AddUndirectedEdge(v1, v2 Vertex, edgeID EdgeID, info *ChannelInfo) bool {
	ok1 := g.AddEdge(v1, v2, edgeID, info)
	ok2 := g.AddEdge(v2, v1, edgeID, info)
	return ok1 && ok2
}

// ReplaceEdge replaces directed edge in the graph
func (g *Graph) ReplaceEdge(v1, v2 Vertex, edgeID EdgeID, info *ChannelInfo) bool {
	if tmap, ok := g.adjacencyList[v1]; ok {
		if _, ok := tmap[v2]; ok {
			if _, ok := tmap[v2][edgeID]; ok {
				tmap[v2][edgeID] = info
				return true
			}
		}
	}
	return false
}

// ReplaceUndirectedEdge replaces undirected edge in the graph.
func (g *Graph) ReplaceUndirectedEdge(v1, v2 Vertex, edgeID EdgeID, info *ChannelInfo) bool {
	ok1 := g.ReplaceEdge(v1, v2, edgeID, info)
	ok2 := g.ReplaceEdge(v2, v1, edgeID, info)
	return ok1 && ok2
}

// RemoveEdge removes directed edge in a graph.
func (g *Graph) RemoveEdge(v1, v2 Vertex, edgeID EdgeID) bool {
	if _, ok := g.adjacencyList[v1]; ok {
		if _, ok := g.adjacencyList[v1][v2]; ok {
			tmap := g.adjacencyList[v1][v2]
			if _, ok := tmap[edgeID]; ok {
				delete(tmap, edgeID)
				if len(tmap) == 0 {
					delete(g.adjacencyList[v1], v2)
				}
				return true
			}
		}
	}
	return false
}

// RemoveUndirectedEdge removes undirected edge in a graph.
func (g *Graph) RemoveUndirectedEdge(v1, v2 Vertex, edgeID EdgeID) bool {
	ok1 := g.RemoveEdge(v1, v2, edgeID)
	ok2 := g.RemoveEdge(v2, v1, edgeID)
	return ok1 && ok2
}

// GetInfo returns info about edge in a graph.
func (g *Graph) GetInfo(v1, v2 Vertex, edgeID EdgeID) (*ChannelInfo, error) {
	if tmap, ok := g.adjacencyList[v1]; ok {
		if _, ok := tmap[v2]; ok {
			if _, ok := tmap[v2][edgeID]; ok {
				return tmap[v2][edgeID], nil
			}
		}
	}
	return nil, EdgeNotFoundError
}

// GetNeighbors returns neighbors of a given vertex in a graph.
// Note: output should not be modified because it will change
// original graph
func (g *Graph) GetNeighbors(v Vertex) (map[Vertex]map[EdgeID]*ChannelInfo, error) {
	if !g.HasVertex(v) {
		return nil, NodeNotFoundError
	}
	return g.adjacencyList[v], nil
}

// MinCostChannel return channel with minimal weight between two vertexes
func (g *Graph) MinCostChannel(v1, v2 Vertex) (*ChannelInfo, error) {
	if !g.HasVertex(v1) || !g.HasVertex(v2) {
		return nil, NodeNotFoundError
	}
	if _, ok := g.adjacencyList[v1][v2]; !ok {
		return nil, EdgeNotFoundError
	}
	wgt := math.MaxFloat64
	var easiest *ChannelInfo
	for _, edge := range g.adjacencyList[v1][v2] {
		if edge.Wgt < wgt {
			wgt, easiest = edge.Wgt, edge
		}
	}
	return easiest, nil
}


// Bfs do breadth-first search starting from a given vertex
func (g *Graph) Bfs(source Vertex) (map[Vertex]int, map[Vertex]Vertex, error) {
	return bfs(g, source, NilVertex)
}
