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
func All[A any](s []A, pred Pred[A]) bool {
	for _, val := range s {
		if !pred(val) {
			return false
		}
	}

	return true
}

// Any returns true when the supplied predicate evaluates to true for any of
// the values in the slice.
func Any[A any](s []A, pred Pred[A]) bool {
	for _, val := range s {
		if pred(val) {
			return true
		}
	}

	return false
}

// Map applies the function argument to all members of the slice and returns a
// slice of those return values.
func Map[A, B any](s []A, f func(A) B) []B {
	res := make([]B, 0, len(s))

	for _, val := range s {
		res = append(res, f(val))
	}

	return res
}

// Filter creates a new slice of values where all the members of the returned
// slice pass the predicate that is supplied in the argument.
func Filter[A any](s []A, pred Pred[A]) []A {
	res := make([]A, 0)

	for _, val := range s {
		if pred(val) {
			res = append(res, val)
		}
	}

	return res
}

// FilterMap takes a function argument that optionally produces a value and
// returns a slice of the 'Some' return values.
func FilterMap[A, B any](as []A, f func(A) Option[B]) []B {
	var bs []B

	for _, a := range as {
		f(a).WhenSome(func(b B) {
			bs = append(bs, b)
		})
	}

	return bs
}

// TrimNones takes a slice of Option values and returns a slice of the Some
// values in it.
func TrimNones[A any](as []Option[A]) []A {
	var somes []A

	for _, a := range as {
		a.WhenSome(func(b A) {
			somes = append(somes, b)
		})
	}

	return somes
}

// Foldl iterates through all members of the slice left to right and reduces
// them pairwise with an accumulator value that is seeded with the seed value in
// the argument.
func Foldl[A, B any](seed B, s []A, f func(B, A) B) B {
	acc := seed

	for _, val := range s {
		acc = f(acc, val)
	}

	return acc
}

// Foldr, is exactly like Foldl except that it iterates over the slice from
// right to left.
func Foldr[A, B any](seed B, s []A, f func(A, B) B) B {
	acc := seed

	for i := range s {
		acc = f(s[len(s)-1-i], acc)
	}

	return acc
}

// Find returns the first value that passes the supplied predicate, or None if
// the value wasn't found.
func Find[A any](s []A, pred Pred[A]) Option[A] {
	for _, val := range s {
		if pred(val) {
			return Some(val)
		}
	}

	return None[A]()
}

// FindIdx returns the first value that passes the supplied predicate along with
// its index in the slice. If no satisfactory value is found, None is returned.
func FindIdx[A any](s []A, pred Pred[A]) Option[T2[int, A]] {
	for i, val := range s {
		if pred(val) {
			return Some(NewT2[int, A](i, val))
		}
	}

	return None[T2[int, A]]()
}

// Elem returns true if the element in the argument is found in the slice
func Elem[A comparable](a A, s []A) bool {
	return Any(s, Eq(a))
}

// Flatten takes a slice of slices and returns a concatenation of those slices.
func Flatten[A any](s [][]A) []A {
	sz := Foldr(0, s, func(l []A, acc uint64) uint64 {
		return uint64(len(l)) + acc
	})

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
func Span[A any](s []A, pred Pred[A]) ([]A, []A) {
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
func ZipWith[A, B, C any](a []A, b []B, f func(A, B) C) []C {
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
	return Foldl(0, items, func(a, b B) B {
		return a + b
	})
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
func ForEachConc[A, B any](as []A, f func(A) B) []B {
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

// Head returns the first element of the slice, assuming it is non-empty.
func Head[A any](items []A) Option[A] {
	if len(items) == 0 {
		return None[A]()
	}

	return Some(items[0])
}

// Tail returns the slice without the first element, assuming the slice is not
// empty. Note this makes a copy of the slice.
func Tail[A any](items []A) Option[[]A] {
	if len(items) == 0 {
		return None[[]A]()
	}

	res := make([]A, len(items)-1)
	copy(res, items[1:])

	return Some(res)
}

// Init returns the slice without the last element, assuming the slice is not
// empty. Note this makes a copy of the slice.
func Init[A any](items []A) Option[[]A] {
	if len(items) == 0 {
		return None[[]A]()
	}

	res := make([]A, len(items)-1)
	copy(res, items[0:len(items)-1])

	return Some(res)
}

// Last returns the last element of the slice, assuming it is non-empty.
func Last[A any](items []A) Option[A] {
	if len(items) == 0 {
		return None[A]()
	}

	return Some(items[len(items)-1])
}

// Uncons splits a slice into a pair of its Head and Tail.
func Uncons[A any](items []A) Option[T2[A, []A]] {
	return LiftA2Option(NewT2[A, []A])(Head(items), Tail(items))
}

// Unsnoc splits a slice into a pair of its Init and Last.
func Unsnoc[A any](items []A) Option[T2[[]A, A]] {
	return LiftA2Option(NewT2[[]A, A])(Init(items), Last(items))
}

// Len is the len function that is defined in a way that makes it usable in
// higher-order contexts.
func Len[A any](items []A) uint {
	return uint(len(items))
}

// CollectOptions collects a list of Options into a single Option of the list of
// Some values in it. If there are any Nones present it will return None.
func CollectOptions[A any](options []Option[A]) Option[[]A] {
	// We intentionally do a separate None checking pass here to avoid
	// allocating a new slice for the values until we're sure we need to.
	for _, r := range options {
		if r.IsNone() {
			return None[[]A]()
		}
	}

	// Now that we're sure we have no Nones, we can just do an unchecked
	// index into the some value of the option.
	return Some(Map(options, func(o Option[A]) A { return o.some }))
}

// CollectResults collects a list of Results into a single Result of the list of
// Ok values in it. If there are any errors present it will return the first
// error encountered.
func CollectResults[A any](results []Result[A]) Result[[]A] {
	// We intentionally do a separate error checking pass here to avoid
	// allocating a new slice for the results until we're sure we need to.
	for _, r := range results {
		if r.IsErr() {
			return Err[[]A](r.right)
		}
	}

	// Now that we're sure we have no errors, we can just do an unchecked
	// index into the left side of the result.
	return Ok(Map(results, func(r Result[A]) A { return r.left }))
}

// TraverseOption traverses a slice of A values, applying the provided
// function to each, collecting the results into an Option of a slice of B
// values. If any of the results are None, the entire result is None.
func TraverseOption[A, B any](as []A, f func(A) Option[B]) Option[[]B] {
	var bs []B
	for _, a := range as {
		b := f(a)
		if b.IsNone() {
			return None[[]B]()
		}
		bs = append(bs, b.some)
	}

	return Some(bs)
}

// TraverseResult traverses a slice of A values, applying the provided
// function to each, collecting the results into a Result of a slice of B
// values. If any of the results are Err, the entire result is the first
// error encountered.
func TraverseResult[A, B any](as []A, f func(A) Result[B]) Result[[]B] {
	var bs []B
	for _, a := range as {
		b := f(a)
		if b.IsErr() {
			return Err[[]B](b.right)
		}
		bs = append(bs, b.left)
	}

	return Ok(bs)
}
