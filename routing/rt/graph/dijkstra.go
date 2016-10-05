// Copyright (c) 2016 Bitfury Group Limited
// Distributed under the MIT software license, see the accompanying
// file LICENSE or http://www.opensource.org/licenses/mit-license.php

package graph

import "math"

func DijkstraPathWeight(g *Graph, source, target Vertex) (float64, error) {
	dist, _, err := dijkstra(g, source, target)
	if err != nil {
		return 0, err
	}
	if _, ok := dist[target]; !ok {
		return 0, PathNotFoundError
	}
	return dist[target], nil
}

func DijkstraPath(g *Graph, source, target Vertex) ([]Vertex, error) {
	_, parent, err := dijkstra(g, source, target)
	if err != nil {
		return nil, err
	}
	if _, ok := parent[target]; !ok {
		return nil, PathNotFoundError
	}
	path, err := Path(source, target, parent)
	if err != nil {
		return nil, err
	}
	return path, nil
}

func dijkstra(g *Graph, source, target Vertex) (map[Vertex]float64, map[Vertex]Vertex, error) {
	dist := make(map[Vertex]float64)
	colored := make(map[Vertex]bool)
	parent := make(map[Vertex]Vertex)
	dist[source] = 0
	for {
		bestDist := math.MaxFloat64
		var bestID Vertex
		for id, val := range dist {
			if val < bestDist && !colored[id] {
				bestDist = val
				bestID = id
			}
		}
		if bestID == target {
			break
		}
		colored[bestID] = true
		targets, err := g.GetNeighbors(bestID)
		if err != nil {
			return nil, nil, err
		}
		for id, multiedges := range targets {
			for _, edge := range multiedges {
				if have, ok := dist[id]; !ok || have > dist[bestID]+edge.Wgt {
					dist[id] = dist[bestID] + edge.Wgt
					parent[id] = bestID
				}
			}
		}
	}
	return dist, parent, nil
}
