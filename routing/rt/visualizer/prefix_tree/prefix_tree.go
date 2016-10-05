// Copyright (c) 2016 Bitfury Group Limited
// Distributed under the MIT software license, see the accompanying
// file LICENSE or http://www.opensource.org/licenses/mit-license.php

package prefix_tree

import (
	"errors"
	"strconv"
)

var (
	CyclicDataError   = errors.New("Cyclic data")
	NodeNotFoundError = errors.New("Node not found")
	NotEnoughData     = errors.New("Not enough data")
)

type PrefixTree interface {
	Add(str string) error
	Shortcut(fullname string) (string, error)
	Autocomplete(prefix string) (string, error)
	Len() int
}

type prefixTree struct {
	root  Node
	nodes int
}

func NewPrefixTree() PrefixTree {
	root := NewNode("0", nil, "", false)
	return &prefixTree{root: root}
}

func (tree *prefixTree) Add(str string) error {
	var node Node = tree.root
	for _, symbolRune := range str {
		symbol := string(byte(symbolRune))
		child := node.Child(symbol)
		if child == nil {
			child = NewNode(strconv.Itoa(tree.Len()), node, symbol, false)
			err := node.AddChild(symbol, child)
			if err != nil {
				return err
			}
			tree.nodes++
		}
		node = child
	}
	node.Terminal()
	return nil
}

// Find shortcut for a given string. Shortcut is unique prefix for a given string.
// If there is only one string in prefix tree return first symbol
func (tree *prefixTree) Shortcut(fullname string) (string, error) {
	var node Node = tree.root
	for _, symbolRune := range fullname {
		symbol := string(byte(symbolRune))
		child := node.Child(symbol)
		if child == nil {
			return "", NodeNotFoundError
		}
		node = child
	}
	if !node.IsTerminal() {
		return "", errors.New("Node MUST be terminal")
	}
	if !node.IsLeaf() {
		return fullname, nil
	}
	highest := node
	for {
		if highest.Parent() == nil || highest.Parent().IsTerminal() || highest.Parent().Len() != 1 {
			break
		}
		if highest == highest.Parent() {
			return "", CyclicDataError
		}
		highest = highest.Parent()
	}
	path, err := highest.Path()
	if err != nil {
		return "", err
	}
	if path == "" {
		return fullname[0:1], nil
	}
	return path, nil
}

func (tree *prefixTree) Autocomplete(prefix string) (string, error) {
	var node Node = tree.root
	for _, symbolRune := range prefix {
		symbol := string(byte(symbolRune))
		child := node.Child(symbol)
		if child == nil {
			return "", NodeNotFoundError
		}
		node = child
	}
	for !node.IsLeaf() {
		if node.IsTerminal() || node.Len() != 1 {
			return "", NotEnoughData
		}
		childs := node.Childs()
		for _, value := range childs {
			node = value
		}
	}
	path, err := node.Path()
	if err != nil {
		return "", err
	}
	return path, nil
}

func (tree *prefixTree) Len() int {
	return tree.nodes
}
