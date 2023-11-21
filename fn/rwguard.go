package fn

import "sync"

type RwGuard[A any] struct {
	val A
	sync.RWMutex
}

func WithRLock[A, B any](g *RwGuard[A], f func(A) B) B {
	g.RWMutex.RLock()
	defer g.RWMutex.RUnlock()

	return f(g.val)
}

func WithWLock[A, B any](g *RwGuard[A], f func(A) (A, B)) B {
	g.RWMutex.Lock()
	defer g.RWMutex.Unlock()

	newVal, esc := f(g.val)
	g.val = newVal

	return esc
}
