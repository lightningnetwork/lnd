package fn

// Copyable is a generic interface for a type that's able to return a deep copy
// of itself.
type Copyable[T any] interface {
	Copy() T
}

// CopyAll creates a new slice where each item of the slice is a deep copy of
// the elements of the input slice.
func CopyAll[T Copyable[T]](xs []T) []T {
	newItems := make([]T, len(xs))
	for i := range xs {
		newItems[i] = xs[i].Copy()
	}

	return newItems
}

// CopyableErr is a generic interface for a type that's able to return a deep copy
// of itself. This is identical to Copyable, but should be used in cases where
// the copy method can return an error.
type CopyableErr[T any] interface {
	Copy() (T, error)
}

// CopyAllErr creates a new slice where each item of the slice is a deep copy of
// the elements of the input slice. This is identical to CopyAll, but should be
// used in cases where the copy method can return an error.
func CopyAllErr[T CopyableErr[T]](xs []T) ([]T, error) {
	var err error

	newItems := make([]T, len(xs))
	for i := range xs {
		newItems[i], err = xs[i].Copy()
		if err != nil {
			return nil, err
		}
	}

	return newItems, nil
}
