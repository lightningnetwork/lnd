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

// CollectErr drains all of the elements from the passed iterator with error
// handling, returning a slice of the contents and the first error encountered.
func CollectErr[T any](seq iter.Seq2[T, error]) ([]T, error) {
	var ret []T
	for val, err := range seq {
		if err != nil {
			return ret, err
		}
		ret = append(ret, val)
	}

	return ret, nil
}
