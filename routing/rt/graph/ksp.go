// Copyright (c) 2016 Bitfury Group Limited
// Distributed under the MIT software license, see the accompanying
// file LICENSE or http://www.opensource.org/licenses/mit-license.php

package graph

import "math"

// KShortestPaths finds k shortest paths
// Note: this implementation finds k path not necessary shortest
// It tries to make that distinct and shortest at the same time
func KShortestPaths(g *Graph, source, target Vertex, k int) ([][]Vertex, error) {
	ksp := make([][]Vertex, 0, k)
	DRY := make(map[string]bool)
	actualNodeWeight := make(map[Vertex]float64, g.GetVertexCount())
	for _, id := range g.GetVertexes() {
		actualNodeWeight[id] = 1
	}
	const UselessIterations = 200
	for cnt := 0; len(ksp) < k && cnt < UselessIterations; cnt++ {
		if err := modifyEdgeWeight(g, actualNodeWeight); err != nil {
			return nil, err
		}
		path, err := DijkstraPath(g, source, target)
		if err != nil {
			return nil, err
		}
		for _, v := range path {
			actualNodeWeight[v]++
		}
		key := ""
		for _, v := range path {
			key += v.String()
		}
		if !DRY[key] {
			DRY[key] = true
			ksp = append(ksp, path)
			cnt = 0
		}
	}
	return ksp, nil
}

func modifyEdgeWeight(g *Graph, actualNodeWeight map[Vertex]float64) error {
	for _, v1 := range g.GetVertexes() {
		targets, err := g.GetNeighbors(v1)
		if err != nil {
			return err
		}
		for v2, multiedges := range targets {
			for ID := range multiedges {
				wgt := calcEdgeWeight(actualNodeWeight, v1, v2)
				g.ReplaceUndirectedEdge(v1, v2, ID, &ChannelInfo{Wgt: wgt})
			}
		}
	}
	return nil
}

// Calculate new edge weight based on vertex weights.
// It uses empirical formulae
// weight(i, j) = (weight(i) + weight(j)) ^ 6
// Number 6 was choosen because it gives best results in several simulations
func calcEdgeWeight(actualNodeWeight map[Vertex]float64, v1, v2 Vertex) float64 {
	const ExperiementalNumber = 6.0
	wgt := math.Pow(actualNodeWeight[v1]+actualNodeWeight[v2], ExperiementalNumber)
	return wgt
}