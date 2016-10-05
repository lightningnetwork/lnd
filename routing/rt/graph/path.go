// Copyright (c) 2016 Bitfury Group Limited
// Distributed under the MIT software license, see the accompanying
// file LICENSE or http://www.opensource.org/licenses/mit-license.php

package graph

import "errors"

func Path(v1, v2 Vertex, parent map[Vertex]Vertex) ([]Vertex, error) {
	path, err := ReversePath(v1, v2, parent)
	if err != nil {
		return nil, err
	}
	Reverse(path)
	return path, nil
}

func ReversePath(v1, v2 Vertex, parent map[Vertex]Vertex) ([]Vertex, error) {
	path := []Vertex{v2}
	for v2 != v1 {
		if v2 == parent[v2] {
			return nil, CyclicDataError
		}
		var ok bool
		v2, ok = parent[v2]
		if !ok {
			return nil, errors.New("Invalid key")
		}
		path = append(path, v2)
	}
	return path, nil
}

func Reverse(path []Vertex) {
	length := len(path)
	for i := 0; i < length/2; i++ {
		path[i], path[length-1-i] = path[length-1-i], path[i]
	}
}
