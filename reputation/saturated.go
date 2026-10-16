package reputation

import "math"

// saturatedI64 wraps an int64 that clamps to the int64 range on overflow
// instead of wrapping, so that an accumulating value can never silently flip
// sign once it grows past the int64 bound. It is a struct rather than a named
// int64 so that the raw +/- operators cannot be used on it by accident: all
// arithmetic has to go through Add and Sub.
type saturatedI64 struct {
	v int64
}

// satFromInt returns a saturatedI64 holding the given value.
func satFromInt(v int64) saturatedI64 {
	return saturatedI64{v: v}
}

// satFromUint converts an unsigned value, clamping to the maximum when it does
// not fit.
func satFromUint(v uint64) saturatedI64 {
	if v > math.MaxInt64 {
		return saturatedI64{v: math.MaxInt64}
	}

	return saturatedI64{v: int64(v)}
}

// satFromFloat converts a float, clamping to the int64 range. A plain
// float-to-int conversion is undefined out of range in Go (in practice it
// yields MinInt64), so a value that rounds above the maximum would flip
// negative without this guard.
func satFromFloat(f float64) saturatedI64 {
	switch {
	case f >= float64(math.MaxInt64):
		return saturatedI64{v: math.MaxInt64}

	case f <= float64(math.MinInt64):
		return saturatedI64{v: math.MinInt64}

	default:
		return saturatedI64{v: int64(f)}
	}
}

// Add returns the saturating sum of the two values.
func (s saturatedI64) Add(o saturatedI64) saturatedI64 {
	sum := s.v + o.v
	switch {
	case s.v > 0 && o.v > 0 && sum < 0:
		return saturatedI64{v: math.MaxInt64}

	case s.v < 0 && o.v < 0 && sum >= 0:
		return saturatedI64{v: math.MinInt64}

	default:
		return saturatedI64{v: sum}
	}
}

// Sub returns the saturating difference of the two values.
func (s saturatedI64) Sub(o saturatedI64) saturatedI64 {
	diff := s.v - o.v
	switch {
	// Underflow: subtracting a positive from a negative can only go more
	// negative, so a non-negative result means it wrapped.
	case s.v < 0 && o.v > 0 && diff > 0:
		return saturatedI64{v: math.MinInt64}

	// Overflow: subtracting a negative from a non-negative can only go more
	// positive, so a negative result means it wrapped.
	case s.v >= 0 && o.v < 0 && diff < 0:
		return saturatedI64{v: math.MaxInt64}

	default:
		return saturatedI64{v: diff}
	}
}

// Int64 returns the underlying int64.
func (s saturatedI64) Int64() int64 {
	return s.v
}
