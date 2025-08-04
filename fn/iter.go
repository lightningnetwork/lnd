package fn

import "iter"

// Collect drains all of the elements from the passed iterator, returning a
// slice of the contents.
func Collect[T any](seq iter.Seq[T]) []T {

	var ret []T
	for i := range seq {
		ret = append(ret, i)
	}

	return ret
}
