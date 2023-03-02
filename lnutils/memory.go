package lnutils

// PTr returns the pointer of the given value. This is useful in instances
// where a function returns the value, but a pointer is wanted. Without this,
// then an intermediate variable is needed.
func Ptr[T any](v T) *T {
	return &v
}
