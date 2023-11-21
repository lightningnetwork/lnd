package fn

import "sync"

type MutGuard[A any] struct {
	val A
	sync.Mutex
}

func (m *MutGuard[A]) Lock() {
	m.Mutex.Lock()
}

func (m *MutGuard[A]) Unlock() {
	m.Mutex.Unlock()
}

func (m *MutGuard[A]) UnsafeDeref() *A {
	return &m.val
}

func AtomicallyModify[A, B any](g *MutGuard[A], f func(A) (A, B)) B {
	g.Lock()
	defer g.Unlock()

	newVal, alt := f(g.val)
	g.val = newVal

	return alt
}
