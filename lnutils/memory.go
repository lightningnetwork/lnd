package lnutils

// Ptr returns the pointer of the given value. This is useful in instances
// where a function returns the value, but a pointer is wanted. Without this,
// then an intermediate variable is needed.
func Ptr[T any](v T) *T {
	return &v
}

// ByteArray is a type constraint for type that reduces down to a fixed sized
// array.
type ByteArray interface {
	~[32]byte
}

// ByteSlice takes a byte array, and returns a slice. This is useful when a
// function returns an array, but a slice is wanted. Without this, then an
// intermediate variable is needed.
func ByteSlice[T ByteArray](v T) []byte {
	return v[:]
}
