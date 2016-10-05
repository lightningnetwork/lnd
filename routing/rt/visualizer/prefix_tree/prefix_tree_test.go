// Copyright (c) 2016 Bitfury Group Limited
// Distributed under the MIT software license, see the accompanying
// file LICENSE or http://www.opensource.org/licenses/mit-license.php

package prefix_tree

import "testing"

func TestPrefixTree(t *testing.T) {
	pt := NewPrefixTree()
	pt.Add("walk")
	pt.Add("hello")
	pt.Add("hi")
	pt.Add("hell")
	pt.Add("world")
	pt.Add("www")

	shortcut, _ := pt.Shortcut("world")
	if expected := "wo"; shortcut != expected {
		t.Errorf("expected: %s, actual: %s", expected, shortcut)
	}

	shortcut, _ = pt.Shortcut("www")
	if expected := "ww"; shortcut != expected {
		t.Errorf("expected: %s, actual: %s", expected, shortcut)
	}

	autocompleted, _ := pt.Autocomplete("wo")
	if expected := "world"; autocompleted != expected {
		t.Errorf("expected: %s, actual: %s", expected, autocompleted)
	}

	autocompleted, _ = pt.Autocomplete("ww")
	if expected := "www"; autocompleted != expected {
		t.Errorf("expected: %s, actual: %s", expected, autocompleted)
	}

	pt.Add("123")
	pt.Add("456")
	pt.Add("1234")

	shortcut, _ = pt.Shortcut("123")
	if expected := "123"; shortcut != expected {
		t.Errorf("expected: %s, actual: %s", expected, shortcut)
	}
}

func TestPrefixTreeOneNode(t *testing.T) {
	pt := NewPrefixTree()
	pt.Add("123")
	shortcut, err := pt.Shortcut("123")
	if err != nil {
		t.Errorf("error getting shortcut for 123: %v, want: %v", err, nil)
	}
	expectedShortcut := "1"
	if shortcut != expectedShortcut {
		t.Errorf("expected: %v, actual: %v", expectedShortcut, shortcut)
	}

	expectedAutocomplete := "123"
	autocomplete, err := pt.Autocomplete("123")

	if err != nil {
		t.Errorf("error getting autocomplete for 123: %v, want: %v", err, nil)
	}
	if autocomplete != expectedAutocomplete {
		t.Errorf("expected: %v, actual: %v", expectedAutocomplete, autocomplete)
	}

}
