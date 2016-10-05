// Copyright (c) 2016 Bitfury Group Limited
// Distributed under the MIT software license, see the accompanying
// file LICENSE or http://www.opensource.org/licenses/mit-license.php.

package graph

import (
	"reflect"
	"testing"
)

func TestShortestPath(t *testing.T) {
	g, v := newTestGraph()

	tests := []struct{
		Source, Target Vertex
		ExpectedPath []Vertex
	}{
		{v[1], v[4], []Vertex{v[1], v[7], v[3], v[6], v[5], v[4]}},
		{v[5], v[7], []Vertex{v[5], v[6], v[3], v[7]}},
	}
	for _, test := range tests {
		path, err := ShortestPath(g, test.Source, test.Target)
		if err != nil {
			t.Errorf("ShortestPath(g, %v, %v ) returns not nil error: %v", test.Source, test.Target, err)
		}
		if !reflect.DeepEqual(path, test.ExpectedPath) {
			t.Errorf("ShortestPath(g, %v, %v ) = %v, want %v", test.Source, test.Target, path, test.ExpectedPath)
		}
	}
}
