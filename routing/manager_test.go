// Copyright (c) 2016 Bitfury Group Limited
// Distributed under the MIT software license, see the accompanying
// file LICENSE or http://www.opensource.org/licenses/mit-license.php

package routing

import (
	"fmt"
	"github.com/lightningnetwork/lnd/routing/rt/graph"
	"reflect"
	"testing"
	"time"
)

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func vertexFromInt(x int) graph.Vertex {
	s := fmt.Sprintf("%v", x)
	return graph.NewVertex([]byte(s))
}

func edgeIdFromString(s string) graph.EdgeID {
	e := graph.EdgeID{}
	copy(e.Hash[:], []byte(s))
	return e
}

var sampleEdgeId graph.EdgeID = edgeIdFromString("EdgeId")

func createLinearNetwork(n int) (*MockNetwork, []*RoutingManager) {
	// Creates linear graph 0->1->2->..->n-1
	nodes := make([]*RoutingManager, 0)
	net := NewMockNetwork(false)
	net.Start()
	for i := 0; i < n; i++ {
		node := NewRoutingManager(vertexFromInt(i), nil)
		nodes = append(nodes, node)
		node.Start()
		net.Add(node)
	}

	for i := 0; i < n-1; i++ {
		nodes[i].OpenChannel(nodes[i+1].Id, sampleEdgeId, nil)
		nodes[i+1].OpenChannel(nodes[i].Id, sampleEdgeId, nil)
	}

	return net, nodes
}

func createCompleteNetwork(n int) (*MockNetwork, []*RoutingManager) {
	nodes := make([]*RoutingManager, 0)
	net := NewMockNetwork(false)
	net.Start()
	for i := 0; i < n; i++ {
		node := NewRoutingManager(vertexFromInt(i), nil)
		nodes = append(nodes, node)
		node.Start()
		net.Add(node)
	}

	for i := 0; i < n-1; i++ {
		for j := i + 1; j < n; j++ {
			nodes[i].OpenChannel(nodes[j].Id, sampleEdgeId, nil)
			nodes[j].OpenChannel(nodes[i].Id, sampleEdgeId, nil)
		}
	}

	return net, nodes
}

func createNetwork(desc [][2]int, idFunc func(int) graph.Vertex) (*MockNetwork, map[int]*RoutingManager, []graph.Edge) {
	// Creates network of nodes from graph description
	net := NewMockNetwork(false)
	net.Start()
	// create unique nodes
	nodes := make(map[int]*RoutingManager)
	for i := 0; i < len(desc); i++ {
		for j := 0; j < 2; j++ {
			nodeId := desc[i][j]
			if _, ok := nodes[nodeId]; !ok {
				var id graph.Vertex
				if idFunc != nil {
					id = idFunc(nodeId)
				} else {
					id = vertexFromInt(nodeId)
				}
				node := NewRoutingManager(id, nil)
				nodes[nodeId] = node
				node.Start()
				net.Add(node)
			}
		}
	}
	edges := make([]graph.Edge, 0, len(desc))
	for i := 0; i < len(desc); i++ {
		edgeID := edgeIdFromString(fmt.Sprintf("edge-%v", i))
		nodes[desc[i][0]].OpenChannel(nodes[desc[i][1]].Id, edgeID, &graph.ChannelInfo{1, 1})
		nodes[desc[i][1]].OpenChannel(nodes[desc[i][0]].Id, edgeID, &graph.ChannelInfo{1, 1})
		edges = append(edges, graph.NewEdge(
			nodes[desc[i][0]].Id,
			nodes[desc[i][1]].Id,
			edgeID,
			&graph.ChannelInfo{1, 1},
		))
	}
	return net, nodes, edges
}

func TestNeighborsScanLinearGraph(t *testing.T) {
	n := 4
	net, nodes := createLinearNetwork(n)
	time.Sleep(10 * time.Millisecond)
	// Each node should know about all channels
	for i := 0; i < n; i++ {
		for j := 0; j < n; j++ {
			for k := 0; k < n; k++ {
				ans := nodes[i].HasChannel(nodes[j].Id, nodes[k].Id, sampleEdgeId)
				correctAns := abs(j-k) == 1
				if ans != correctAns {
					t.Errorf("nodes[%v].HasChannel(%v, %v)==%v, want %v", i, j, k, ans, correctAns)
				}
			}
		}
	}
	net.Stop()

}

func TestNeighborsScanCompleteGraph(t *testing.T) {
	n := 4
	net, nodes := createCompleteNetwork(n)
	time.Sleep(10 * time.Millisecond)
	// Each node should know about all channels
	for i := 0; i < n; i++ {
		for j := 0; j < n; j++ {
			for k := 0; k < n; k++ {
				ans := nodes[i].HasChannel(nodes[j].Id, nodes[k].Id, sampleEdgeId)
				correctAns := j != k
				if ans != correctAns {
					t.Errorf("nodes[%v].HasChannel(%v, %v)==%v, want %v", i, j, k, ans, correctAns)
				}
			}
		}
	}
	net.Stop()
}

func TestNeighborsRemoveChannel(t *testing.T) {
	// Create complete graph, than delete channels to make it linear
	n := 4
	net, nodes := createCompleteNetwork(n)
	time.Sleep(10 * time.Millisecond)
	for i := 0; i < n; i++ {
		for j := 0; j < n; j++ {
			if abs(i-j) != 1 {
				nodes[i].RemoveChannel(nodes[i].Id, nodes[j].Id, sampleEdgeId)
			}
		}
	}
	time.Sleep(10 * time.Millisecond)
	// Each node should know about all channels
	for i := 0; i < n; i++ {
		for j := 0; j < n; j++ {
			for k := 0; k < n; k++ {
				ans := nodes[i].HasChannel(nodes[j].Id, nodes[k].Id, sampleEdgeId)
				correctAns := abs(j-k) == 1
				if ans != correctAns {
					t.Errorf("nodes[%v].HasChannel(%v, %v)==%v, want %v", i, j, k, ans, correctAns)
				}
			}
		}
	}

	net.Stop()
}

func TestFindPath(t *testing.T) {
	// Create linear graph
	n := 6
	net, nodes := createLinearNetwork(n)
	time.Sleep(10 * time.Millisecond) // Each node should know about all channels

	path, err := nodes[0].FindPath(nodes[5].Id)
	if err != nil {
		t.Errorf("err = %v, want %v", err)
	}
	correctPath := []graph.Vertex{}
	for i := 0; i < n; i++ {
		correctPath = append(correctPath, nodes[i].Id)
	}
	if !reflect.DeepEqual(path, correctPath) {
		t.Errorf("path = %v, want %v", path, correctPath)
	}
	// Case when path do not exist
	path, err = nodes[0].FindPath(vertexFromInt(7))
	if path != nil {
		t.Errorf("path = %v, want %v", path, nil)
	}
	if err != graph.PathNotFoundError {
		t.Errorf("err = %v, want %v", err, graph.PathNotFoundError)
	}
	net.Stop()
}

func TestKShortestPaths(t *testing.T) {
	net, nodes, _ := createNetwork([][2]int{
		[2]int{0, 1},
		[2]int{1, 2},
		[2]int{2, 3},
		[2]int{1, 3},
		[2]int{4, 5},
	}, nil)
	time.Sleep(10 * time.Millisecond) // Each node should know about all channels
	// There was bug in lnd when second search of the same path leads to lncli/lnd freeze
	for iter := 1; iter <= 3; iter++ {
		paths, err := nodes[0].FindKShortestPaths(nodes[3].Id, 2)
		if err != nil {
			t.Errorf("err = %v, want %v", err, nil)
		}
		correctPaths := [][]graph.Vertex{
			[]graph.Vertex{vertexFromInt(0), vertexFromInt(1), vertexFromInt(3)},
			[]graph.Vertex{vertexFromInt(0), vertexFromInt(1), vertexFromInt(2), vertexFromInt(3)},
		}
		if !reflect.DeepEqual(paths, correctPaths) {
			t.Errorf("on iteration: %v paths = %v, want %v", iter, paths, correctPaths)
		}
	}
	// Case when path do not exist
	paths, _ := nodes[0].FindKShortestPaths(vertexFromInt(7), 3)
	if len(paths) != 0 {
		t.Errorf("path = %v, want %v", paths, []graph.Vertex{})
	}
	net.Stop()
}
