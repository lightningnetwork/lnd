package lntypes

import "golang.org/x/exp/constraints"

// Number defines a type constraint for numbers.
type Number interface {
	constraints.Integer | constraints.Float
}

// Max returns the greater of the two inputs.
func Max[N Number](op1 N, op2 N) N {
	if op1 > op2 {
		return op1
	}

	return op2
}

// Min returns the lesser of the two inputs.
func Min[N Number](op1 N, op2 N) N {
	if op1 < op2 {
		return op1
	}

	return op2
}
