// Copyright (c) 2016 Bitfury Group Limited
// Distributed under the MIT software license, see the accompanying
// file LICENSE or http://www.opensource.org/licenses/mit-license.php

package rt

import (
	"github.com/lightningnetwork/lnd/routing/rt/graph"
	"testing"
)

var (
	NewID = vertexFromString
)

func vertexFromString(s string) graph.Vertex {
	return graph.NewVertex([]byte(s))
}

func edgeIdFromString(s string) graph.EdgeID {
	e := graph.EdgeID{}
	copy(e.Hash[:], []byte(s))
	return e
}

func TestRT(t *testing.T) {
	r := NewRoutingTable()
	r.AddNodes(vertexFromString("1"), vertexFromString("2"))

	r.AddChannel(vertexFromString("1"), vertexFromString("2"), edgeIdFromString("EdgeID"), nil)
	if !r.HasChannel(vertexFromString("1"), vertexFromString("2"), edgeIdFromString("EdgeID")) {
		t.Error(`r.HasChannel(1, 2) == false, want true`)
	}
	if !r.HasChannel(vertexFromString("2"), vertexFromString("1"), edgeIdFromString("EdgeID")) {
		t.Error(`r.HasChannel(2, 1) == false, want true`)
	}
}

func TestDiff(t *testing.T) {
	r := NewRoutingTable()
	buff := r.NewDiffBuff()
	// Add 2 channels
	r.AddNodes(vertexFromString("1"), vertexFromString("2"), vertexFromString("3"))

	r.AddChannel(vertexFromString("1"), vertexFromString("2"), edgeIdFromString("EdgeID"), nil)
	r.AddChannel(vertexFromString("2"), vertexFromString("3"), edgeIdFromString("EdgeID"), nil)
	r1 := NewRoutingTable()
	r1.ApplyDiffBuff(buff)
	if !r.HasChannel(vertexFromString("1"), vertexFromString("2"), edgeIdFromString("EdgeID")) {
		t.Error(`r.HasChannel(1, 2) == false, want true`)
	}
	if !r.HasChannel(vertexFromString("2"), vertexFromString("3"), edgeIdFromString("EdgeID")) {
		t.Error(`r.HasChannel(2, 3) == false, want true`)
	}
	// Remove channel
	r.RemoveChannel(vertexFromString("1"), vertexFromString("2"), edgeIdFromString("EdgeID"))
	r1.ApplyDiffBuff(buff)
	if r.HasChannel(vertexFromString("1"), vertexFromString("2"), edgeIdFromString("EdgeID")) {
		t.Error(`r.HasChannel(1, 2) == true, want false`)
	}
	if r.HasChannel(vertexFromString("2"), vertexFromString("1"), edgeIdFromString("EdgeID")) {
		t.Error(`r.HasChannel(2, 1) == true, want false`)
	}
}

func TestRTChannels(t *testing.T) {
	rt := NewRoutingTable()
	rt.AddNodes(vertexFromString("1"), vertexFromString("2"), vertexFromString("3"))

	rt.AddChannel(vertexFromString("1"), vertexFromString("2"), edgeIdFromString("EdgeID"), nil)
	rt.AddChannel(NewID("2"), NewID("3"), edgeIdFromString("EdgeID"), nil)

	channels := rt.AllChannels()
	if len(channels) != 2 {
		t.Errorf(`rt.AllChannels == %v, want %v`, len(channels), 2)
	}
}

func TestRTAddTable(t *testing.T) {
	rt1 := NewRoutingTable()
	rt1.AddNodes(NewID("0"), NewID("1"), NewID("2"), NewID("3"), NewID("5"), NewID("6"))

	rt1.AddChannel(NewID("0"), NewID("1"), edgeIdFromString("EdgeID"), nil)
	rt1.AddChannel(NewID("1"), NewID("2"), edgeIdFromString("EdgeID"), nil)

	rt2 := NewRoutingTable()
	rt2.AddNodes(NewID("0"), NewID("1"), NewID("2"), NewID("3"), NewID("5"), NewID("6"))

	rt2.AddChannel(NewID("2"), NewID("3"), edgeIdFromString("EdgeID"), nil)
	rt2.AddChannel(NewID("5"), NewID("6"), edgeIdFromString("EdgeID"), nil)
	rt1.AddTable(rt2)
	expectedChannels := [][]graph.Vertex{
		[]graph.Vertex{NewID("0"), NewID("1")},
		[]graph.Vertex{NewID("1"), NewID("2")},
		[]graph.Vertex{NewID("2"), NewID("3")},
		[]graph.Vertex{NewID("5"), NewID("6")},
	}
	for _, c := range expectedChannels {
		if !rt1.HasChannel(c[0], c[1], edgeIdFromString("EdgeID")) {
			t.Errorf("After addition, channel between %v and %v is absent", c[0], c[1])
		}
	}
}

func TestChannelInfoCopy(t *testing.T) {
	c := &graph.ChannelInfo{Cpt: 10, Wgt: 10}
	c1 := c.Copy()
	if *c != *c1 {
		t.Errorf("*c.Copy() != *c, *c=%v, *c.Copy()=%v", *c, *c1)
	}
	c2 := (*graph.ChannelInfo)(nil)
	c2ans := c2.Copy()
	if c2ans != nil {
		t.Errorf("c.Copy()=%v, for c=nil, want nil", c2ans)
	}
}

func TestRTCopy(t *testing.T) {
	r := NewRoutingTable()
	// Add 2 channels
	r.AddNodes(NewID("1"), NewID("2"), NewID("3"))

	r.AddChannel(NewID("1"), NewID("2"), edgeIdFromString("EdgeID"), &graph.ChannelInfo{10, 11})
	r.AddChannel(NewID("2"), NewID("3"), edgeIdFromString("EdgeID"), &graph.ChannelInfo{20, 21})
	// Create copy of this rt and make sure they are equal
	rCopy := r.Copy()
	channelsExp := []struct {
		id1, id2 graph.Vertex
		edgeID   graph.EdgeID
		info     *graph.ChannelInfo
		isErrNil bool
	}{
		{NewID("1"), NewID("2"), edgeIdFromString("EdgeID"), &graph.ChannelInfo{10, 11}, true},
		{NewID("2"), NewID("3"), edgeIdFromString("EdgeID"), &graph.ChannelInfo{20, 21}, true},
		// Channel do not exist
		{NewID("3"), NewID("4"), edgeIdFromString("EdgeID"), nil, false},
	}
	for _, c := range channelsExp {
		info, err := rCopy.GetChannelInfo(c.id1, c.id2, c.edgeID)
		if (err != nil) && (info != nil) {
			t.Errorf("rCopy.GetChannelInfo give not nil result and err")
			continue
		}
		if (err == nil) != c.isErrNil {
			t.Errorf("err == nil: %v, want: %v, err: %v", err == nil, c.isErrNil, err)
			continue
		}
		if info == nil && c.info != nil {
			t.Errorf("rCopy.GetChannelInfo(%v, %v, %v)==nil, nil. want <not nil>", c.id1, c.id2, c.edgeID)
			continue
		}
		if info != nil && c.info == nil {
			t.Errorf("rCopy.GetChannelInfo(%v, %v, %v)==<not nil>, nil. want nil", c.id1, c.id2, c.edgeID)
			continue
		}
		if (info != nil) && c.info != nil && *info != *c.info {
			t.Errorf("*rCopy.GetChannelInfo(%v, %v, %v)==%v, nil. want %v", c.id1, c.id2, c.edgeID, *info, c.info)
		}
	}
}