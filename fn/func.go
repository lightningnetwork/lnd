package fn

// Reducer represents a function that takes an accumulator and the value, then
// returns a new accumulator.
type Reducer[T, V any] func(accum T, value V) T

// Reduce takes a slice of something, and a reducer, and produces a final
// accumulated value.
func Reduce[T any, V any, S []V](s S, f Reducer[T, V]) T {
	var accum T

	for _, x := range s {
		accum = f(accum, x)
	}

	return accum
}
