package fn

import (
	"math/rand"
	"reflect"
	"testing"
	"testing/quick"
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
