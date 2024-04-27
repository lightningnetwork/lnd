package fn

import (
	"context"
	"runtime"
	"sync"

	"golang.org/x/exp/constraints"
	"golang.org/x/sync/semaphore"
)

// Number is a type constraint for all numeric types in Go (integers,
// float and complex numbers)
type Number interface {
	constraints.Integer | constraints.Float | constraints.Complex
}

// All returns true when the supplied predicate evaluates to true for all of
// the values in the slice.
func All[A any](pred func(A) bool, s []A) bool {
	for _, val := range s {
		if !pred(val) {
			return false
		}
	}

	return true
}

// Any returns true when the supplied predicate evaluates to true for any of
// the values in the slice.
func Any[A any](pred func(A) bool, s []A) bool {
	for _, val := range s {
		if pred(val) {
			return true
		}
	}

	return false
}

// Map applies the function argument to all members of the slice and returns a
// slice of those return values.
func Map[A, B any](f func(A) B, s []A) []B {
	res := make([]B, 0, len(s))

	for _, val := range s {
		res = append(res, f(val))
	}

	return res
}

// Filter creates a new slice of values where all the members of the returned
// slice pass the predicate that is supplied in the argument.
func Filter[A any](pred Pred[A], s []A) []A {
	res := make([]A, 0)

	for _, val := range s {
		if pred(val) {
			res = append(res, val)
		}
	}

	return res
}

// Foldl iterates through all members of the slice left to right and reduces
// them pairwise with an accumulator value that is seeded with the seed value in
// the argument.
func Foldl[A, B any](f func(B, A) B, seed B, s []A) B {
	acc := seed

	for _, val := range s {
		acc = f(acc, val)
	}

	return acc
}

// Foldr, is exactly like Foldl except that it iterates over the slice from
// right to left.
func Foldr[A, B any](f func(A, B) B, seed B, s []A) B {
	acc := seed

	for i := range s {
		acc = f(s[len(s)-1-i], acc)
	}

	return acc
}

// Find returns the first value that passes the supplied predicate, or None if
// the value wasn't found.
func Find[A any](pred Pred[A], s []A) Option[A] {
	for _, val := range s {
		if pred(val) {
			return Some(val)
		}
	}

	return None[A]()
}

// FindIdx returns the first value that passes the supplied predicate along with
// its index in the slice. If no satisfactory value is found, None is returned.
func FindIdx[A any](pred Pred[A], s []A) Option[T2[int, A]] {
	for i, val := range s {
		if pred(val) {
			return Some(NewT2[int, A](i, val))
		}
	}

	return None[T2[int, A]]()
}

// Elem returns true if the element in the argument is found in the slice
func Elem[A comparable](a A, s []A) bool {
	return Any(Eq(a), s)
}

// Flatten takes a slice of slices and returns a concatenation of those slices.
func Flatten[A any](s [][]A) []A {
	sz := Foldr(
		func(l []A, acc uint64) uint64 {
			return uint64(len(l)) + acc
		}, 0, s,
	)

	res := make([]A, 0, sz)

	for _, val := range s {
		res = append(res, val...)
	}

	return res
}

// Replicate generates a slice of values initialized by the prototype value.
func Replicate[A any](n uint, val A) []A {
	res := make([]A, n)

	for i := range res {
		res[i] = val
	}

	return res
}

// Span, applied to a predicate and a slice, returns two slices where the first
// element is the longest prefix (possibly empty) of slice elements that
// satisfy the predicate and second element is the remainder of the slice.
func Span[A any](pred func(A) bool, s []A) ([]A, []A) {
	for i := range s {
		if !pred(s[i]) {
			fst := make([]A, i)
			snd := make([]A, len(s)-i)

			copy(fst, s[:i])
			copy(snd, s[i:])

			return fst, snd
		}
	}

	res := make([]A, len(s))
	copy(res, s)

	return res, []A{}
}

// SplitAt(n, s) returns a tuple where first element is s prefix of length n
// and second element is the remainder of the list.
func SplitAt[A any](n uint, s []A) ([]A, []A) {
	fst := make([]A, n)
	snd := make([]A, len(s)-int(n))

	copy(fst, s[:n])
	copy(snd, s[n:])

	return fst, snd
}

// ZipWith combines slice elements with the same index using the function
// argument, returning a slice of the results.
func ZipWith[A, B, C any](f func(A, B) C, a []A, b []B) []C {
	var l uint

	if la, lb := len(a), len(b); la < lb {
		l = uint(la)
	} else {
		l = uint(lb)
	}

	res := make([]C, l)

	for i := 0; i < int(l); i++ {
		res[i] = f(a[i], b[i])
	}

	return res
}

// SliceToMap converts a slice to a map using the provided key and value
// functions.
func SliceToMap[A any, K comparable, V any](s []A, keyFunc func(A) K,
	valueFunc func(A) V) map[K]V {

	res := make(map[K]V, len(s))
	for _, val := range s {
		key := keyFunc(val)
		value := valueFunc(val)
		res[key] = value
	}

	return res
}

// Sum calculates the sum of a slice of numbers, `items`.
func Sum[B Number](items []B) B {
	return Foldl(func(a, b B) B {
		return a + b
	}, 0, items)
}

// HasDuplicates checks if the given slice contains any duplicate elements.
// It returns false if there are no duplicates in the slice (i.e., all elements
// are unique), otherwise returns false.
func HasDuplicates[A comparable](items []A) bool {
	return len(NewSet(items...)) != len(items)
}

// ForEachConc maps the argument function over the slice, spawning a new
// goroutine for each element in the slice and then awaits all results before
// returning them.
func ForEachConc[A, B any](f func(A) B,
	as []A) []B {

	var wait sync.WaitGroup
	ctx := context.Background()

	sem := semaphore.NewWeighted(int64(runtime.NumCPU()))

	bs := make([]B, len(as))

	for i, a := range as {
		i, a := i, a
		sem.Acquire(ctx, 1)
		wait.Add(1)
		go func() {
			bs[i] = f(a)
			wait.Done()
			sem.Release(1)
		}()
	}

	wait.Wait()

	return bs
}
