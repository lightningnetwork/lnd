package lnwire

import "fmt"

// Timestamp is an interface for node announcement update-ordering values. A
// timestamp can represent either unix time (v1) or block height (v2).
type Timestamp interface {
	// IsZero returns true if the timestamp has no value.
	IsZero() bool

	// Cmp compares this timestamp to the passed timestamp. Implementations
	// only support comparisons against the same concrete timestamp type.
	Cmp(other Timestamp) (CompareResult, error)
}

// UnixTimestamp is a unix-time based node announcement timestamp.
type UnixTimestamp uint64

// IsZero returns true if the timestamp has no value.
func (u UnixTimestamp) IsZero() bool {
	return u == 0
}

// Cmp compares this timestamp to another unix timestamp.
func (u UnixTimestamp) Cmp(other Timestamp) (CompareResult, error) {
	o, ok := other.(UnixTimestamp)
	if !ok {
		return 0, fmt.Errorf("expected UnixTimestamp, got: %T", other)
	}

	switch {
	case u < o:
		return LessThan, nil
	case u > o:
		return GreaterThan, nil
	default:
		return EqualTo, nil
	}
}

// BlockHeightTimestamp is a block-height based node announcement timestamp.
type BlockHeightTimestamp uint64

// IsZero returns true if the timestamp has no value.
func (b BlockHeightTimestamp) IsZero() bool {
	return b == 0
}

// Cmp compares this timestamp to another block-height timestamp.
func (b BlockHeightTimestamp) Cmp(other Timestamp) (CompareResult, error) {
	o, ok := other.(BlockHeightTimestamp)
	if !ok {
		return 0, fmt.Errorf("expected BlockHeightTimestamp, got: %T",
			other)
	}

	switch {
	case b < o:
		return LessThan, nil
	case b > o:
		return GreaterThan, nil
	default:
		return EqualTo, nil
	}
}
