// Copyright (c) 2016 Bitfury Group Limited
// Distributed under the MIT software license, see the accompanying
// file LICENSE or http://www.opensource.org/licenses/mit-license.php

package prefix_tree

import "errors"

// Node represents the general interface for node in prefix tree
type Node interface {
	Name() string
	Len() int
	Child(nextSymbol string) Node
	Childs() map[string]Node
	AddChild(symbol string, child Node) error
	Parent() Node
	PreviousSymbol() string
	IsRoot() bool
	IsLeaf() bool
	IsTerminal() bool
	Terminal()
	Path() (string, error)
}

type node struct {
	name           string
	childs         map[string]Node
	parent         Node
	previousSymbol string
	isTerminal     bool
}

// NewNode create new node
func NewNode(name string, parent Node, previousSymbol string, isTerminal bool) Node {
	return &node{
		name:           name,
		childs:         make(map[string]Node),
		parent:         parent,
		previousSymbol: previousSymbol,
		isTerminal:     isTerminal,
	}
}

func (nd *node) Name() string {
	return nd.name
}

func (nd *node) Len() int {
	return len(nd.childs)
}

func (nd *node) Child(nextSymbol string) Node {
	if _, ok := nd.childs[nextSymbol]; !ok {
		return nil
	}
	return nd.childs[nextSymbol]
}

func (nd *node) Childs() map[string]Node {
	return nd.childs
}

func (nd *node) AddChild(symbol string, child Node) error {
	if _, ok := nd.childs[symbol]; ok {
		return errors.New("Node already exists")
	}
	nd.childs[symbol] = child
	return nil
}

func (nd *node) Parent() Node {
	return nd.parent
}

func (nd *node) PreviousSymbol() string {
	return nd.previousSymbol
}

func (nd *node) IsRoot() bool {
	return nd.parent == nil
}

func (nd *node) IsLeaf() bool {
	return len(nd.childs) == 0
}

func (nd *node) IsTerminal() bool {
	return nd.isTerminal
}

func (nd *node) Terminal() {
	nd.isTerminal = true
}

func (nd *node) Path() (path string, err error) {
	var xNode Node = nd
	for {
		if xNode.IsRoot() {
			break
		}
		path += xNode.PreviousSymbol()
		if xNode == xNode.Parent() {
			return "", CyclicDataError
		}
		xNode = xNode.Parent()
	}
	return Reverse(path), nil
}

func Reverse(reversePath string) (path string) {
	length := len(reversePath)
	for i := length - 1; i >= 0; i-- {
		path += string(reversePath[i])
	}
	return path
}
