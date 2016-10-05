// Copyright (c) 2016 Bitfury Group Limited
// Distributed under the MIT software license, see the accompanying
// file LICENSE or http://www.opensource.org/licenses/mit-license.php

package graph

import (
	"reflect"
	"testing"
)

func TestDijkstraPath(t *testing.T) {
	g, v := newTestGraph()

	tests := []struct {
		Source, Target Vertex
		ExpectedPath   []Vertex
		ExpectedWeight float64
	}{
		{v[1], v[4], []Vertex{v[1], v[7], v[2], v[3], v[6], v[5], v[4]}, 13},
		{v[5], v[7], []Vertex{v[5], v[6], v[3], v[2], v[7]}, 10},
	}
	for _, test := range tests {
		// Test DijkstraPath
		path, err := DijkstraPath(g, test.Source, test.Target)
		if err != nil {
			t.Errorf("DijkstraPath(g, %v, %v ) returns not nil error: %v", test.Source, test.Target, err)
		}
		if !reflect.DeepEqual(path, test.ExpectedPath) {
			t.Errorf("DijkstraPath(g, %v, %v ) = %v, want %v", test.Source, test.Target, path, test.ExpectedPath)
		}
		// Test DijkstraPathWeight
		weight, err := DijkstraPathWeight(g, test.Source, test.Target)
		if err != nil {
			t.Errorf("DijkstraPathWeight(g, %v, %v ) returns not nil error: %v", test.Source, test.Target, err)
		}
		if weight != test.ExpectedWeight {
			t.Errorf("DijkstraPathWeight(g, %v, %v ) = %v, want %v", test.Source, test.Target, weight, test.ExpectedWeight)
		}
	}
}
