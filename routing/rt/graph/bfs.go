// Copyright (c) 2016 Bitfury Group Limited
// Distributed under the MIT software license, see the accompanying
// file LICENSE or http://www.opensource.org/licenses/mit-license.php

package graph

import "container/list"

func ShortestPathLen(g *Graph, v1, v2 Vertex) (int, error) {
	dist, _, err := bfs(g, v1, v2)
	if err != nil {
		return 0, err
	}
	if _, ok := dist[v2]; !ok {
		return 0, PathNotFoundError
	}
	return dist[v2], nil
}

func ShortestPath(g *Graph, v1, v2 Vertex) ([]Vertex, error) {
	_, parent, err := bfs(g, v1, v2)
	if err != nil {
		return nil, err
	}
	if _, ok := parent[v2]; !ok {
		return nil, PathNotFoundError
	}
	path, err := Path(v1, v2, parent)
	if err != nil {
		return nil, err
	}
	return path, nil
}

func bfs(g *Graph, v1, v2 Vertex) (map[Vertex]int, map[Vertex]Vertex, error) {
	var queue list.List
	queue.PushBack(v1)
	dist := make(map[Vertex]int)
	parent := make(map[Vertex]Vertex)
	dist[v1] = 0
	for queue.Len() != 0 {
		err := ibfs(g, queue.Front().Value.(Vertex), &queue, dist, parent)
		if err != nil {
			return nil, nil, err
		}
		if _, ok := dist[v2]; ok {
			break
		}
	}
	return dist, parent, nil
}

func ibfs(g *Graph, v Vertex, queue *list.List, dist map[Vertex]int, parent map[Vertex]Vertex) error {
	queue.Remove(queue.Front())
	targets, err := g.GetNeighbors(v)
	if err != nil {
		return err
	}
	for to := range targets {
		if _, ok := dist[to]; !ok {
			dist[to] = dist[v] + 1
			parent[to] = v
			queue.PushBack(to)
		}
	}
	return nil
}
