package fn

// Unit is a type alias for the empty struct to make it a bit less noisy to
// communicate the informationaless type.
type Unit = struct{}

// Comp is left to right function composition. Comp(f, g)(x) == g(f(x)). This
// can make it easier to create on the fly closures that we may use as
// arguments to other functions defined in this package (or otherwise).
func Comp[A, B, C any](f func(A) B, g func(B) C) func(A) C {
	return func(a A) C {
		return g(f(a))
	}
}

// Iden is the left and right identity of Comp. It is a function that simply
// returns its argument. The utility of this function is only apparent in
// conjunction with other functions in this package.
func Iden[A any](a A) A {
	return a
}

// Const is a function that accepts an argument and returns a function that
// always returns that value irrespective of the returned function's argument.
// This is also quite useful in conjunction with higher order functions.
func Const[B, A any](a A) func(B) A {
	return func(_ B) A {
		return a
	}
}

// Eq is a curried function that returns true if its eventual two arguments are
// equal.
func Eq[A comparable](x A) func(A) bool {
	return func(y A) bool {
		return x == y
	}
}

// Neq is a curried function that returns true if its eventual two arguments are
// not equal.
func Neq[A comparable](x A) func(A) bool {
	return func(y A) bool {
		return x != y
	}
}
