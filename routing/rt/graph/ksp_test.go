// Copyright (c) 2016 Bitfury Group Limited
// Distributed under the MIT software license, see the accompanying
// file LICENSE or http://www.opensource.org/licenses/mit-license.php

package graph

import (
	"reflect"
	"testing"
)

func TestKSP(t *testing.T) {
	g, v := newTestGraph()

	tests := []struct {
		Source, Target Vertex
		K              int
		ExpectedPaths  [][]Vertex
	}{
		{v[1], v[4], 10, [][]Vertex{
			{v[1], v[7], v[3], v[6], v[5], v[4]},
			{v[1], v[7], v[2], v[3], v[6], v[5], v[4]},
		},
		},
		{v[5], v[7], 1, [][]Vertex{
			{v[5], v[6], v[3], v[7]},
		},
		},
	}
	for _, test := range tests {
		paths, err := KShortestPaths(g, test.Source, test.Target, test.K)
		if err != nil {
			t.Errorf("KShortestPaths(g, %v, %v, %v) returns not nil error: %v", test.Source, test.Target, test.K, err)
		}
		if !reflect.DeepEqual(paths, test.ExpectedPaths) {
			t.Errorf("KShortestPaths(g, %v, %v, %v) = %v, want %v", test.Source, test.Target, test.K, paths, test.ExpectedPaths)
		}
	}
}
