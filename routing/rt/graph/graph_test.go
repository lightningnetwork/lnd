// Copyright (c) 2016 Bitfury Group Limited
// Distributed under the MIT software license, see the accompanying
// file LICENSE or http://www.opensource.org/licenses/mit-license.php

package graph

import (
	"reflect"
	"testing"
	"fmt"
)

func vertexFromInt(x int) Vertex {
	s := fmt.Sprintf("%v", x)
	return NewVertex([]byte(s))
}

func edgeIdFromString(s string) EdgeID {
	e := EdgeID{}
	copy(e.Hash[:], []byte(s))
	return e
}

// newTestGraph returns new graph for testing purposes
// Each time it returns new graph
func newTestGraph() (*Graph, []Vertex) {
	g := NewGraph()
	v := make([]Vertex, 8)
	for i:=1; i<8; i++ {
		v[i] = vertexFromInt(i)
		g.AddVertex(v[i])
	}
	edges := []Edge{
		{v[1], v[7], edgeIdFromString("2"), &ChannelInfo{Wgt:2}},
		{v[7], v[2], edgeIdFromString("1"), &ChannelInfo{Wgt:1}},
		{v[3], v[7], edgeIdFromString("10"), &ChannelInfo{Wgt:10}},
		{v[2], v[3], edgeIdFromString("2"), &ChannelInfo{Wgt:2}},
		{v[3], v[6], edgeIdFromString("4"), &ChannelInfo{Wgt:4}},
		{v[5], v[6], edgeIdFromString("3"), &ChannelInfo{Wgt:3}},
		{v[4], v[5], edgeIdFromString("1"), &ChannelInfo{Wgt:1}},
	}
	for _, e := range edges {
		g.AddUndirectedEdge(e.Src, e.Tgt, e.Id, e.Info)
	}
	return g, v
}

func TestNodeManipulation(t *testing.T) {
	g := NewGraph()
	if g.HasVertex(vertexFromInt(2)) {
		t.Errorf("expected: %t, actual: %t", false, true)
	}
	if !g.AddVertex(vertexFromInt(2)) {
		t.Errorf("expected: %t, actual: %t", true, false)
	}
	if g.AddVertex(vertexFromInt(2)) {
		t.Errorf("expected: %t, actual: %t", false, true)
	}
	if !g.HasVertex(vertexFromInt(2)) {
		t.Errorf("expected: %t, actual: %t", true, false)
	}
	if !g.RemoveVertex(vertexFromInt(2)) {
		t.Errorf("expected: %t, actual: %t", true, false)
	}
	if g.RemoveVertex(vertexFromInt(2)) {
		t.Errorf("expected: %t, actual: %t", false, true)
	}
	if g.HasVertex(vertexFromInt(2)) {
		t.Errorf("expected: %t, actual: %t", false, true)
	}
}

func TestEdgeManipulation(t *testing.T) {
	g := NewGraph()
	if g.AddEdge(vertexFromInt(2), vertexFromInt(3), NilEdgeID, nil) {
		t.Errorf("expected: %t, actual: %t", false, true)
	}
	g.AddVertex(vertexFromInt(2))
	g.AddVertex(vertexFromInt(3))
	if g.HasEdge(vertexFromInt(2), vertexFromInt(3), NilEdgeID) {
		t.Errorf("expected: %t, actual: %t", false, true)
	}
	if g.ReplaceEdge(vertexFromInt(2), vertexFromInt(3), NilEdgeID, &ChannelInfo{1, 2}) {
		t.Errorf("expected: %t, actual: %t", false, true)
	}
	if !g.AddEdge(vertexFromInt(2), vertexFromInt(3), NilEdgeID, nil) {
		t.Errorf("expected: %t, actual: %t", true, false)
	}
	if !g.AddEdge(vertexFromInt(2), vertexFromInt(3), NilEdgeID, nil) {
		t.Errorf("expected: %t, actual: %t", true, false)
	}
	if !g.HasEdge(vertexFromInt(2), vertexFromInt(3), NilEdgeID) {
		t.Errorf("expected: %t, actual: %t", true, false)
	}
	if !g.ReplaceEdge(vertexFromInt(2), vertexFromInt(3), NilEdgeID, &ChannelInfo{1, 2}) {
		t.Errorf("expected: %t, actual: %t", true, false)
	}
	if info, err := g.GetInfo(vertexFromInt(2), vertexFromInt(3), NilEdgeID); err != nil {
		panic(err)
	} else if !reflect.DeepEqual(*info, ChannelInfo{1, 2}) {
		t.Errorf("expected: %v, actual: %v", ChannelInfo{1, 2}, *info)
	}
	if !g.RemoveEdge(vertexFromInt(2), vertexFromInt(3), NilEdgeID) {
		t.Errorf("expected: %t, actual: %t", true, false)
	}
	if g.RemoveEdge(vertexFromInt(2), vertexFromInt(3), NilEdgeID) {
		t.Errorf("expected: %t, actual: %t", false, true)
	}
	if g.HasEdge(vertexFromInt(2), vertexFromInt(3), NilEdgeID) {
		t.Errorf("expected: %t, actual: %t", false, true)
	}
}

func TestAllGetMethods(t *testing.T) {
	g := NewGraph()
	g.AddVertex(vertexFromInt(2))
	g.AddVertex(vertexFromInt(3))
	if vertexCount := g.GetVertexCount(); vertexCount != 2 {
		t.Errorf("expected: %d, actual: %d", 2, vertexCount)
	}
	if vs := g.GetVertexes(); !reflect.DeepEqual(vs, []Vertex{vertexFromInt(2), vertexFromInt(3)}) &&
		!reflect.DeepEqual(vs, []Vertex{vertexFromInt(3), vertexFromInt(2)}) {
		t.Errorf("expected: %v, actual: %v",
			[]Vertex{vertexFromInt(2), vertexFromInt(3)},
			vs,
		)
	}
	g.AddEdge(vertexFromInt(2), vertexFromInt(3), NilEdgeID, nil)
	if edges := g.GetEdges(); !reflect.DeepEqual(edges, []Edge{NewEdge(vertexFromInt(2), vertexFromInt(3), NilEdgeID, nil)}) {
		t.Errorf("expected: %v, actual: %v",
			[]Edge{NewEdge(vertexFromInt(2), vertexFromInt(3), NilEdgeID, nil)},
			edges,
		)
	}

	if targets, err := g.GetNeighbors(vertexFromInt(2)); err != nil {
		panic(err)
	} else if !reflect.DeepEqual(targets, map[Vertex]map[EdgeID]*ChannelInfo{vertexFromInt(3): map[EdgeID]*ChannelInfo{NilEdgeID: nil}}) {
		t.Errorf("expected: %v, actual: %v",
			map[Vertex]map[EdgeID]*ChannelInfo{vertexFromInt(3): map[EdgeID]*ChannelInfo{NilEdgeID: nil}},
			targets,
		)
	}

	g2 := NewGraph()
	g2.AddVertex(vertexFromInt(2))
	g2.AddVertex(vertexFromInt(3))
	g2.AddEdge(vertexFromInt(2), vertexFromInt(3), NilEdgeID, &ChannelInfo{Wgt: 42})
	if info, err := g2.GetInfo(vertexFromInt(2), vertexFromInt(3), NilEdgeID); err != nil {
		panic(err)
	} else if wgt := info.Wgt; wgt != 42 {
		t.Errorf("expected: %v, actual: %v", wgt, 42)
	}
}

func TestVertexToByte33(t *testing.T) {
	var b1 [33]byte
	for i := 0; i < 33; i++ {
		b1[i] = byte(i)
	}
	v := NewVertex(b1[:])
	b2 := v.ToByte33()
	if b1 != b2 {
		t.Errorf("Wrong result of ID.ToByte33()= %v, want %v", b2, b1)
	}
}
