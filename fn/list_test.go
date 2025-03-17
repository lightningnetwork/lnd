package fn

import (
	"math/rand"
	"reflect"
	"slices"
	"testing"
	"testing/quick"

	"github.com/stretchr/testify/require"
)

func GenList(r *rand.Rand) *List[uint32] {
	size := int(r.Uint32() >> 24)
	l := NewList[uint32]()
	for i := 0; i < size; i++ {
		l.PushBack(rand.Uint32())
	}
	return l
}

func GetRandNode(l *List[uint32], r *rand.Rand) *Node[uint32] {
	if l.Len() == 0 {
		return nil
	}

	idx := r.Uint32() % uint32(l.Len())
	n := l.Front()
	for i := 0; i < int(idx); i++ {
		n = n.Next()
	}

	return n
}

func TestPushLenIncrement(t *testing.T) {
	err := quick.Check(
		func(l *List[uint32], x uint32, front bool) bool {
			sz := l.Len()
			if front {
				l.PushFront(x)
			} else {
				l.PushBack(x)
			}
			sz2 := l.Len()

			return sz2 == sz+1
		},
		&quick.Config{
			Values: func(vs []reflect.Value, r *rand.Rand) {
				vs[0] = reflect.ValueOf(GenList(r))
				vs[1] = reflect.ValueOf(r.Uint32())
				vs[2] = reflect.ValueOf(r.Uint32()%2 == 0)
			},
		},
	)

	if err != nil {
		t.Fatal(err)
	}
}

func TestRemoveLenDecrement(t *testing.T) {
	err := quick.Check(
		func(l *List[uint32], n *Node[uint32]) bool {
			if l.Len() == 0 {
				return true
			}

			sz := l.Len()
			l.Remove(n)
			sz2 := l.Len()

			return sz2 == sz-1
		},
		&quick.Config{
			Values: func(vs []reflect.Value, r *rand.Rand) {
				l := GenList(r)
				vs[0] = reflect.ValueOf(l)
				vs[1] = reflect.ValueOf(GetRandNode(l, r))
			},
		},
	)

	if err != nil {
		t.Fatal(err)
	}
}

func TestPushGetCongruence(t *testing.T) {
	err := quick.Check(
		func(l *List[uint32], x uint32, front bool) bool {
			if front {
				l.PushFront(x)
				return l.Front().Value == x
			} else {
				l.PushBack(x)
				return l.Back().Value == x
			}
		},
		&quick.Config{
			Values: func(vs []reflect.Value, r *rand.Rand) {
				vs[0] = reflect.ValueOf(GenList(r))
				vs[1] = reflect.ValueOf(r.Uint32())
				vs[2] = reflect.ValueOf(r.Uint32()%2 == 0)
			},
		},
	)

	if err != nil {
		t.Fatal(err)
	}
}

func TestInsertBeforeFrontIdentity(t *testing.T) {
	err := quick.Check(
		func(l *List[uint32], x uint32) bool {
			if l == nil {
				return true
			}

			nodeX := l.InsertBefore(x, l.Front())

			return nodeX == l.Front()
		},
		&quick.Config{
			Values: func(vs []reflect.Value, r *rand.Rand) {
				l := GenList(r)
				vs[0] = reflect.ValueOf(l)
				vs[1] = reflect.ValueOf(r.Uint32())
			},
		},
	)

	if err != nil {
		t.Fatal(err)
	}
}

func TestInsertAfterBackIdentity(t *testing.T) {
	err := quick.Check(
		func(l *List[uint32], x uint32) bool {
			if l == nil {
				return true
			}

			nodeX := l.InsertAfter(x, l.Back())

			return nodeX == l.Back()
		},
		&quick.Config{
			Values: func(vs []reflect.Value, r *rand.Rand) {
				l := GenList(r)
				vs[0] = reflect.ValueOf(l)
				vs[1] = reflect.ValueOf(r.Uint32())
			},
		},
	)

	if err != nil {
		t.Fatal(err)
	}
}

func TestInsertBeforeNextIdentity(t *testing.T) {
	err := quick.Check(
		func(l *List[uint32], n *Node[uint32], x uint32) bool {
			if n == nil {
				return true
			}

			nodeX := l.InsertBefore(x, n)
			return nodeX.Next() == n
		},
		&quick.Config{
			Values: func(vs []reflect.Value, r *rand.Rand) {
				l := GenList(r)
				vs[0] = reflect.ValueOf(l)
				vs[1] = reflect.ValueOf(GetRandNode(l, r))
				vs[2] = reflect.ValueOf(r.Uint32())
			},
		},
	)

	if err != nil {
		t.Fatal(err)
	}
}

func TestInsertAfterPrevIdentity(t *testing.T) {
	err := quick.Check(
		func(l *List[uint32], n *Node[uint32], x uint32) bool {
			if n == nil {
				return true
			}

			nodeX := l.InsertAfter(x, n)
			return nodeX.Prev() == n
		},
		&quick.Config{
			Values: func(vs []reflect.Value, r *rand.Rand) {
				l := GenList(r)
				vs[0] = reflect.ValueOf(l)
				vs[1] = reflect.ValueOf(GetRandNode(l, r))
				vs[2] = reflect.ValueOf(r.Uint32())
			},
		},
	)

	if err != nil {
		t.Fatal(err)
	}
}

func TestMoveToFrontFrontIdentity(t *testing.T) {
	err := quick.Check(
		func(l *List[uint32], n *Node[uint32]) bool {
			if n == nil {
				return true
			}

			l.MoveToFront(n)
			return l.Front() == n
		},
		&quick.Config{
			Values: func(vs []reflect.Value, r *rand.Rand) {
				l := GenList(r)
				vs[0] = reflect.ValueOf(l)
				vs[1] = reflect.ValueOf(GetRandNode(l, r))
			},
		},
	)

	if err != nil {
		t.Fatal(err)
	}
}

func TestMoveToBackBackIdentity(t *testing.T) {
	err := quick.Check(
		func(l *List[uint32], n *Node[uint32]) bool {
			if n == nil {
				return true
			}

			l.MoveToBack(n)
			return l.Back() == n
		},
		&quick.Config{
			Values: func(vs []reflect.Value, r *rand.Rand) {
				l := GenList(r)
				vs[0] = reflect.ValueOf(l)
				vs[1] = reflect.ValueOf(GetRandNode(l, r))
			},
		},
	)

	if err != nil {
		t.Fatal(err)
	}
}

func TestMoveBeforeFrontIsFront(t *testing.T) {
	err := quick.Check(
		func(l *List[uint32], n *Node[uint32]) bool {
			if n == nil {
				return true
			}

			l.MoveBefore(n, l.Front())
			return l.Front() == n
		},
		&quick.Config{
			Values: func(vs []reflect.Value, r *rand.Rand) {
				l := GenList(r)
				vs[0] = reflect.ValueOf(l)
				vs[1] = reflect.ValueOf(GetRandNode(l, r))
			},
		},
	)

	if err != nil {
		t.Fatal(err)
	}
}

func TestMoveAfterBackIsBack(t *testing.T) {
	err := quick.Check(
		func(l *List[uint32], n *Node[uint32]) bool {
			if n == nil {
				return true
			}

			l.MoveAfter(n, l.Back())
			return l.Back() == n
		},
		&quick.Config{
			Values: func(vs []reflect.Value, r *rand.Rand) {
				l := GenList(r)
				vs[0] = reflect.ValueOf(l)
				vs[1] = reflect.ValueOf(GetRandNode(l, r))
			},
		},
	)

	if err != nil {
		t.Fatal(err)
	}
}

func TestMultiMoveErasure(t *testing.T) {
	err := quick.Check(
		func(l *List[uint32], n *Node[uint32], m *Node[uint32]) bool {
			if n == nil || m == nil || n == m {
				return true
			}

			l.MoveToFront(n)
			l.MoveToBack(n)
			l.MoveBefore(n, m)
			l.MoveAfter(n, m)
			return m.Next() == n
		},
		&quick.Config{
			Values: func(vs []reflect.Value, r *rand.Rand) {
				l := GenList(r)
				vs[0] = reflect.ValueOf(l)
				vs[1] = reflect.ValueOf(GetRandNode(l, r))
				vs[2] = reflect.ValueOf(GetRandNode(l, r))
			},
		},
	)

	if err != nil {
		t.Fatal(err)
	}
}

func TestPushListSymmetry(t *testing.T) {
	copyList := func(l *List[uint32]) *List[uint32] {
		c := NewList[uint32]()
		for n := l.Front(); n != nil; n = n.Next() {
			c.PushBack(n.Value)
		}
		return c
	}

	err := quick.Check(
		func(l1 *List[uint32], l2 *List[uint32]) bool {
			if l1.Len() == 0 || l2.Len() == 0 {
				return true
			}

			l1Copy := copyList(l1)
			l2Copy := copyList(l2)

			l1.PushBackList(l2Copy)
			l2.PushFrontList(l1Copy)

			iter1 := l1.Front()
			iter2 := l2.Front()

			for i := 0; i < l1Copy.Len()+l2Copy.Len()-1; i++ {
				if iter1.Value != iter2.Value {
					return false
				}

				iter1 = iter1.Next()
				iter2 = iter2.Next()
			}

			return true
		},
		&quick.Config{
			Values: func(vs []reflect.Value, r *rand.Rand) {
				l := GenList(r)
				l2 := GenList(r)
				vs[0] = reflect.ValueOf(l)
				vs[1] = reflect.ValueOf(l2)
			},
		},
	)

	if err != nil {
		t.Fatal(err)
	}
}

func TestIssue4103(t *testing.T) {
	l1 := NewList[int]()
	l1.PushBack(1)
	l1.PushBack(2)

	l2 := NewList[int]()
	l2.PushBack(3)
	l2.PushBack(4)

	e := l1.Front()
	l2.Remove(e) // l2 should not change because e is not an element of l2
	if n := l2.Len(); n != 2 {
		t.Errorf("l2.Len() = %d, want 2", n)
	}

	l1.InsertBefore(8, e)
	if n := l1.Len(); n != 3 {
		t.Errorf("l1.Len() = %d, want 3", n)
	}
}

func TestIssue6349(t *testing.T) {
	l := NewList[int]()
	l.PushBack(1)
	l.PushBack(2)

	e := l.Front()
	l.Remove(e)
	if e.Value != 1 {
		t.Errorf("e.value = %d, want 1", e.Value)
	}
	if e.Next() != nil {
		t.Errorf("e.Next() != nil")
	}
	if e.Prev() != nil {
		t.Errorf("e.Prev() != nil")
	}
}

func checkListLen[V any](t *testing.T, l *List[V], length int) bool {
	if n := l.Len(); n != length {
		t.Errorf("l.Len() = %d, want %d", n, length)
		return false
	}
	return true
}

func checkListPointers[V any](t *testing.T, l *List[V], es []*Node[V]) {
	root := &l.root

	if !checkListLen(t, l, len(es)) {
		return
	}

	// zero length lists must be the zero value or properly initialized
	// (sentinel circle)
	if len(es) == 0 {
		if l.root.next != nil && l.root.next != root ||
			l.root.prev != nil && l.root.prev != root {

			t.Errorf("l.root.next = %p, l.root.prev = %p;"+
				"both should both be nil or %p", l.root.next,
				l.root.prev, root)
		}
		return
	}
	// len(es) > 0

	// check internal and external prev/next connections
	for i, e := range es {
		prev := root
		Prev := (*Node[V])(nil)
		if i > 0 {
			prev = es[i-1]
			Prev = prev
		}
		if p := e.prev; p != prev {
			t.Errorf("elt[%d](%p).prev = %p, want %p", i, e, p,
				prev)
		}
		if p := e.Prev(); p != Prev {
			t.Errorf("elt[%d](%p).Prev() = %p, want %p", i, e, p,
				Prev)
		}

		next := root
		Next := (*Node[V])(nil)
		if i < len(es)-1 {
			next = es[i+1]
			Next = next
		}
		if n := e.next; n != next {
			t.Errorf("elt[%d](%p).next = %p, want %p", i, e, n,
				next)
		}
		if n := e.Next(); n != Next {
			t.Errorf("elt[%d](%p).Next() = %p, want %p", i, e, n,
				Next)
		}
	}
}

func TestList(t *testing.T) {
	l := NewList[int]()
	checkListPointers(t, l, []*Node[int]{})

	// Single element list
	e := l.PushFront(5)
	checkListPointers(t, l, []*Node[int]{e})
	l.MoveToFront(e)
	checkListPointers(t, l, []*Node[int]{e})
	l.MoveToBack(e)
	checkListPointers(t, l, []*Node[int]{e})
	l.Remove(e)
	checkListPointers(t, l, []*Node[int]{})

	// Bigger list
	e2 := l.PushFront(2)
	e1 := l.PushFront(1)
	e3 := l.PushBack(3)
	e4 := l.PushBack(0)
	checkListPointers(t, l, []*Node[int]{e1, e2, e3, e4})

	l.Remove(e2)
	checkListPointers(t, l, []*Node[int]{e1, e3, e4})

	l.MoveToFront(e3) // move from middle
	checkListPointers(t, l, []*Node[int]{e3, e1, e4})

	l.MoveToFront(e1)
	l.MoveToBack(e3) // move from middle
	checkListPointers(t, l, []*Node[int]{e1, e4, e3})

	l.MoveToFront(e3) // move from back
	checkListPointers(t, l, []*Node[int]{e3, e1, e4})
	l.MoveToFront(e3) // should be no-op
	checkListPointers(t, l, []*Node[int]{e3, e1, e4})

	l.MoveToBack(e3) // move from front
	checkListPointers(t, l, []*Node[int]{e1, e4, e3})
	l.MoveToBack(e3) // should be no-op
	checkListPointers(t, l, []*Node[int]{e1, e4, e3})

	e2 = l.InsertBefore(2, e1) // insert before front
	checkListPointers(t, l, []*Node[int]{e2, e1, e4, e3})
	l.Remove(e2)
	e2 = l.InsertBefore(2, e4) // insert before middle
	checkListPointers(t, l, []*Node[int]{e1, e2, e4, e3})
	l.Remove(e2)
	e2 = l.InsertBefore(2, e3) // insert before back
	checkListPointers(t, l, []*Node[int]{e1, e4, e2, e3})
	l.Remove(e2)

	e2 = l.InsertAfter(2, e1) // insert after front
	checkListPointers(t, l, []*Node[int]{e1, e2, e4, e3})
	l.Remove(e2)
	e2 = l.InsertAfter(2, e4) // insert after middle
	checkListPointers(t, l, []*Node[int]{e1, e4, e2, e3})
	l.Remove(e2)
	e2 = l.InsertAfter(2, e3) // insert after back
	checkListPointers(t, l, []*Node[int]{e1, e4, e3, e2})
	l.Remove(e2)

	// Check standard iteration.
	sum := 0
	for e := l.Front(); e != nil; e = e.Next() {
		sum += e.Value
	}
	if sum != 4 {
		t.Errorf("sum over l = %d, want 4", sum)
	}

	// Clear all elements by iterating
	var next *Node[int]
	for e := l.Front(); e != nil; e = next {
		next = e.Next()
		l.Remove(e)
	}
	checkListPointers(t, l, []*Node[int]{})
}

func checkList[V comparable](t *testing.T, l *List[V], es []V) {
	if !checkListLen(t, l, len(es)) {
		return
	}

	i := 0
	for e := l.Front(); e != nil; e = e.Next() {
		le := e.Value
		if le != es[i] {
			t.Errorf("elt[%d].Value = %v, want %v", i, le, es[i])
		}
		i++
	}
}

func TestExtending(t *testing.T) {
	l1 := NewList[int]()
	l2 := NewList[int]()

	l1.PushBack(1)
	l1.PushBack(2)
	l1.PushBack(3)

	l2.PushBack(4)
	l2.PushBack(5)

	l3 := NewList[int]()
	l3.PushBackList(l1)
	checkList(t, l3, []int{1, 2, 3})
	l3.PushBackList(l2)
	checkList(t, l3, []int{1, 2, 3, 4, 5})

	l3 = NewList[int]()
	l3.PushFrontList(l2)
	checkList(t, l3, []int{4, 5})
	l3.PushFrontList(l1)
	checkList(t, l3, []int{1, 2, 3, 4, 5})

	checkList(t, l1, []int{1, 2, 3})
	checkList(t, l2, []int{4, 5})

	l3 = NewList[int]()
	l3.PushBackList(l1)
	checkList(t, l3, []int{1, 2, 3})
	l3.PushBackList(l3)
	checkList(t, l3, []int{1, 2, 3, 1, 2, 3})

	l3 = NewList[int]()
	l3.PushFrontList(l1)
	checkList(t, l3, []int{1, 2, 3})
	l3.PushFrontList(l3)
	checkList(t, l3, []int{1, 2, 3, 1, 2, 3})

	l3 = NewList[int]()
	l1.PushBackList(l3)
	checkList(t, l1, []int{1, 2, 3})
	l1.PushFrontList(l3)
	checkList(t, l1, []int{1, 2, 3})
}

func TestRemove(t *testing.T) {
	l := NewList[int]()
	e1 := l.PushBack(1)
	e2 := l.PushBack(2)
	checkListPointers(t, l, []*Node[int]{e1, e2})
	e := l.Front()
	l.Remove(e)
	checkListPointers(t, l, []*Node[int]{e2})
	l.Remove(e)
	checkListPointers(t, l, []*Node[int]{e2})
}

func TestMove(t *testing.T) {
	l := NewList[int]()
	e1 := l.PushBack(1)
	e2 := l.PushBack(2)
	e3 := l.PushBack(3)
	e4 := l.PushBack(4)

	l.MoveAfter(e3, e3)
	checkListPointers(t, l, []*Node[int]{e1, e2, e3, e4})
	l.MoveBefore(e2, e2)
	checkListPointers(t, l, []*Node[int]{e1, e2, e3, e4})

	l.MoveAfter(e3, e2)
	checkListPointers(t, l, []*Node[int]{e1, e2, e3, e4})
	l.MoveBefore(e2, e3)
	checkListPointers(t, l, []*Node[int]{e1, e2, e3, e4})

	l.MoveBefore(e2, e4)
	checkListPointers(t, l, []*Node[int]{e1, e3, e2, e4})
	e2, e3 = e3, e2

	l.MoveBefore(e4, e1)
	checkListPointers(t, l, []*Node[int]{e4, e1, e2, e3})
	e1, e2, e3, e4 = e4, e1, e2, e3

	l.MoveAfter(e4, e1)
	checkListPointers(t, l, []*Node[int]{e1, e4, e2, e3})
	e2, e3, e4 = e4, e2, e3

	l.MoveAfter(e2, e3)
	checkListPointers(t, l, []*Node[int]{e1, e3, e2, e4})
}

// Test PushFront, PushBack, PushFrontList, PushBackList with uninitialized List
func TestZeroList(t *testing.T) {
	var l1 = new(List[int])
	l1.PushFront(1)
	checkList(t, l1, []int{1})

	var l2 = new(List[int])
	l2.PushBack(1)
	checkList(t, l2, []int{1})

	var l3 = new(List[int])
	l3.PushFrontList(l1)
	checkList(t, l3, []int{1})

	var l4 = new(List[int])
	l4.PushBackList(l2)
	checkList(t, l4, []int{1})
}

// Test that a list l is not modified when calling InsertBefore with a mark
// that is not an element of l.
func TestInsertBeforeUnknownMark(t *testing.T) {
	var l List[int]
	l.PushBack(1)
	l.PushBack(2)
	l.PushBack(3)
	l.InsertBefore(1, new(Node[int]))
	checkList(t, &l, []int{1, 2, 3})
}

// Test that a list l is not modified when calling InsertAfter with a mark that
// is not an element of l.
func TestInsertAfterUnknownMark(t *testing.T) {
	var l List[int]
	l.PushBack(1)
	l.PushBack(2)
	l.PushBack(3)
	l.InsertAfter(1, new(Node[int]))
	checkList(t, &l, []int{1, 2, 3})
}

// Test that a list l is not modified when calling MoveAfter or MoveBefore with
// a mark that is not an element of l.
func TestMoveUnknownMark(t *testing.T) {
	var l1 List[int]
	e1 := l1.PushBack(1)

	var l2 List[int]
	e2 := l2.PushBack(2)

	l1.MoveAfter(e1, e2)
	checkList(t, &l1, []int{1})
	checkList(t, &l2, []int{2})

	l1.MoveBefore(e1, e2)
	checkList(t, &l1, []int{1})
	checkList(t, &l2, []int{2})
}

// TestFilterIdempotence ensures that the slice coming out of List.Filter is
// the same as that slice filtered again by the same predicate.
func TestFilterIdempotence(t *testing.T) {
	require.NoError(
		t, quick.Check(
			func(l *List[uint32], modSize uint32) bool {
				pred := func(a uint32) bool {
					return a%modSize != 0
				}

				filtered := l.Filter(pred)

				filteredAgain := Filter(filtered, pred)

				return slices.Equal(filtered, filteredAgain)
			},
			&quick.Config{
				Values: func(vs []reflect.Value, r *rand.Rand) {
					l := GenList(r)
					vs[0] = reflect.ValueOf(l)
					vs[1] = reflect.ValueOf(
						r.Uint32()%5 + 1,
					)
				},
			},
		),
	)
}

// TestFilterShrinks ensures that the length of the slice returned from
// List.Filter is never larger than the length of the List.
func TestFilterShrinks(t *testing.T) {
	require.NoError(
		t, quick.Check(
			func(l *List[uint32], modSize uint32) bool {
				pred := func(a uint32) bool {
					return a%modSize != 0
				}

				filteredSize := len(l.Filter(pred))

				return filteredSize <= l.Len()
			},
			&quick.Config{
				Values: func(vs []reflect.Value, r *rand.Rand) {
					l := GenList(r)
					vs[0] = reflect.ValueOf(l)
					vs[1] = reflect.ValueOf(
						r.Uint32()%5 + 1,
					)
				},
			},
		),
	)
}

// TestFilterLawOfExcludedMiddle ensures that if we intersect a List.Filter
// with its negation that the intersection is the empty set.
func TestFilterLawOfExcludedMiddle(t *testing.T) {
	require.NoError(
		t, quick.Check(
			func(l *List[uint32], modSize uint32) bool {
				pred := func(a uint32) bool {
					return a%modSize != 0
				}

				negatedPred := func(a uint32) bool {
					return !pred(a)
				}

				positive := NewSet(l.Filter(pred)...)
				negative := NewSet(l.Filter(negatedPred)...)

				return positive.Intersect(negative).Equal(
					NewSet[uint32](),
				)
			},
			&quick.Config{
				Values: func(vs []reflect.Value, r *rand.Rand) {
					l := GenList(r)
					vs[0] = reflect.ValueOf(l)
					vs[1] = reflect.ValueOf(
						r.Uint32()%5 + 1,
					)
				},
			},
		),
	)
}
