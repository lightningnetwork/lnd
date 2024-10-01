// Copyright (c) 2009 The Go Authors. All rights reserved.
// Copyright (c) 2024 Lightning Labs and the Lightning Network Developers

// Redistribution and use in source and binary forms, with or without
// modification, are permitted provided that the following conditions are
// met:

//    * Redistributions of source code must retain the above copyright
// notice, this list of conditions and the following disclaimer.
//    * Redistributions in binary form must reproduce the above
// copyright notice, this list of conditions and the following disclaimer
// in the documentation and/or other materials provided with the
// distribution.
//    * Neither the name of Google Inc. nor the names of its
// contributors may be used to endorse or promote products derived from
// this software without specific prior written permission.

// THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS
// "AS IS" AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT
// LIMITED TO, THE IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR
// A PARTICULAR PURPOSE ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT
// OWNER OR CONTRIBUTORS BE LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL,
// SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT
// LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES; LOSS OF USE,
// DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND ON ANY
// THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT
// (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE
// OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.
package fn

type Node[A any] struct {
	// prev is a pointer to the previous node in the List.
	prev *Node[A]

	// next is a pointer to the next node in the List.
	next *Node[A]

	// list is the root pointer to the List in which this node is located.
	list *List[A]

	// Value is the actual data contained within the Node.
	Value A
}

// Next returns the next list node or nil.
func (e *Node[A]) Next() *Node[A] {
	if e.list == nil {
		return nil
	}

	if e.next == &e.list.root {
		return nil
	}

	return e.next
}

// Prev returns the previous list node or nil.
func (e *Node[A]) Prev() *Node[A] {
	if e.list == nil {
		return nil
	}

	if e.prev == &e.list.root {
		return nil
	}

	return e.prev
}

// List represents a doubly linked list.
// The zero value for List is an empty list ready to use.
type List[A any] struct {
	// root is a sentinal Node to identify the head and tail of the list.
	// root.prev is the tail, root.next is the head. For the purposes of
	// elegance, the absence of a next or prev node is encoded as the
	// address of the root node.
	root Node[A]

	// len is the current length of the list.
	len int
}

// Init intializes or clears the List l.
func (l *List[A]) Init() *List[A] {
	l.root.next = &l.root
	l.root.prev = &l.root
	l.len = 0

	return l
}

// lazyInit lazily initializes a zero List value. It is called by other public
// functions that could feasibly be called on a List that was initialized by the
// raw List[A]{} constructor.
func (l *List[A]) lazyInit() {
	if l.root.next == nil {
		l.Init()
	}
}

// insert inserts n after predecessor, increments l.len, and returns n.
func (l *List[A]) insert(n *Node[A], predecessor *Node[A]) *Node[A] {
	// Make n point to correct neighborhood.
	n.prev = predecessor
	n.next = predecessor.next

	// Make neighborhood point to n.
	n.prev.next = n
	n.next.prev = n

	// Make n part of the list.
	n.list = l

	// Increment list length.
	l.len++

	return n
}

// insertVal is a convenience wrapper for
// insert(&Node[A]{Value: v}, predecessor).
func (l *List[A]) insertVal(a A, predecessor *Node[A]) *Node[A] {
	return l.insert(&Node[A]{Value: a}, predecessor)
}

// move removes n from its current position and inserts it as the successor to
// predecessor.
func (l *List[A]) move(n *Node[A], predecessor *Node[A]) {
	if n == predecessor {
		return // Can't move after itself.
	}

	if predecessor.next == n {
		return // Nothing to be done.
	}

	// Bind previous and next to each other.
	n.prev.next = n.next
	n.next.prev = n.prev

	// Make n point to new neighborhood.
	n.prev = predecessor
	n.next = predecessor.next

	// Make new neighborhood point to n.
	n.prev.next = n
	n.next.prev = n
}

// New returns an initialized List.
func NewList[A any]() *List[A] {
	l := List[A]{}
	return l.Init()
}

// Len returns the number of elements of List l.
// The complexity is O(1).
func (l *List[A]) Len() int {
	return l.len
}

// Front returns the first Node of List l or nil if the list is empty.
func (l *List[A]) Front() *Node[A] {
	if l.len == 0 {
		return nil
	}

	return l.root.next
}

// Back returns the last Node of List l or nil if the list is empty.
func (l *List[A]) Back() *Node[A] {
	if l.len == 0 {
		return nil
	}

	return l.root.prev
}

// Remove removes Node n from List l if n is an element of List l.
// It returns the Node value e.Value.
// The Node must not be nil.
func (l *List[A]) Remove(n *Node[A]) A {
	if n.list == l {
		n.prev.next = n.next
		n.next.prev = n.prev
		l.len--

		v := n.Value
		// Set all node data to nil to prevent dangling references.
		*n = Node[A]{Value: v}

		return v
	}

	return n.Value
}

// PushFront inserts a new Node n with value a at the front of List l and
// returns n.
func (l *List[A]) PushFront(a A) *Node[A] {
	l.lazyInit()
	return l.insertVal(a, &l.root)
}

// PushBack inserts a new Node n with value a at the back of List l and returns
// n.
func (l *List[A]) PushBack(a A) *Node[A] {
	l.lazyInit()
	return l.insertVal(a, l.root.prev)
}

// InsertBefore inserts a new Node n with value a immediately before successor
// and returns n. If successor is not an element of l, the list is not
// modified. The successor must not be nil.
func (l *List[A]) InsertBefore(a A, successor *Node[A]) *Node[A] {
	if successor == nil {
		return l.insertVal(a, &l.root)
	}

	if successor.list != l {
		return nil
	}

	return l.insertVal(a, successor.prev)
}

// InsertAfter inserts a new Node n with value a immediately after  and returns
// e. If predecessor is not an element of l, the list is not modified. The
// predecessor must not be nil.
func (l *List[A]) InsertAfter(a A, predecessor *Node[A]) *Node[A] {
	if predecessor == nil {
		return l.insertVal(a, l.root.prev)
	}

	if predecessor.list != l {
		return nil
	}

	return l.insertVal(a, predecessor)
}

// MoveToFront moves Node n to the front of List l.
// If n is not an element of l, the list is not modified.
// The Node must not be nil.
func (l *List[A]) MoveToFront(n *Node[A]) {
	if n.list == l {
		l.move(n, &l.root)
	}
}

// MoveToBack moves Node n to the back of List l.
// If n is not an element of l, the list is not modified.
// The Node must not be nil.
func (l *List[A]) MoveToBack(n *Node[A]) {
	if n.list == l {
		l.move(n, l.root.prev)
	}
}

// MoveBefore moves Node n to its new position before successor.
// If n or successor is not an element of l, or n == successor, the list is not
// modified. The Node and successor must not be nil.
func (l *List[A]) MoveBefore(n, successor *Node[A]) {
	if n.list == l && successor.list == l {
		l.move(n, successor.prev)
	}
}

// MoveAfter moves Node n to its new position after predecessor.
// If n or predecessor is not an element of l, or n == predecessor, the list is
// not modified. The Node and predecessor must not be nil.
func (l *List[A]) MoveAfter(n, predecessor *Node[A]) {
	if n.list == l && predecessor.list == l {
		l.move(n, predecessor)
	}
}

// PushBackList inserts a copy of List other at the back of List l.
// The Lists l and other may be the same. They must not be nil.
func (l *List[A]) PushBackList(other *List[A]) {
	l.lazyInit()
	n := other.Front()
	sz := other.Len()
	for i := 0; i < sz; i++ {
		l.insertVal(n.Value, l.root.prev)
		n = n.Next()
	}
}

// PushFrontList inserts a copy of List other at the front of List l.
// The Lists l and other may be the same. They must not be nil.
func (l *List[A]) PushFrontList(other *List[A]) {
	l.lazyInit()
	n := other.Back()
	sz := other.Len()
	for i := 0; i < sz; i++ {
		l.insertVal(n.Value, &l.root)
		n = n.Prev()
	}
}

// Filter gives a slice of all of the node values that satisfy the given
// predicate.
func (l *List[A]) Filter(f Pred[A]) []A {
	var acc []A

	for cursor := l.Front(); cursor != nil; cursor = cursor.Next() {
		a := cursor.Value
		if f(a) {
			acc = append(acc, a)
		}
	}

	return acc
}
