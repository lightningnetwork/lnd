package lnutils

// Map takes an input slice, and applies the function f to each element,
// yielding a new slice.
func Map[T1, T2 any](s []T1, f func(T1) T2) []T2 {
	r := make([]T2, len(s))

	for i, v := range s {
		r[i] = f(v)
	}

	return r
}
