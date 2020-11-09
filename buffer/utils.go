package buffer

// RecycleSlice zeroes byte slice, making it fresh for another use.
// Zeroing the buffer using a logarithmic number of calls to the optimized copy
// method.  Benchmarking shows this to be ~30 times faster than a for loop that
// sets each index to 0 for ~65KB buffers use for wire messages. Inspired by:
// https://stackoverflow.com/questions/30614165/is-there-analog-of-memset-in-go
func RecycleSlice(b []byte) {
	if len(b) == 0 {
		return
	}

	b[0] = 0
	for i := 1; i < len(b); i *= 2 {
		copy(b[i:], b[:i])
	}
}
