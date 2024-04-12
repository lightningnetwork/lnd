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

// Curry takes a two argument function and returns a function that accepts
// the first argument and then returns a function that accepts the second
// argument. This can be a useful utility when taking functions defined in a
// typical go style and adapting them to work with higher-order functions that
// expect functions of a single argument.
func Curry[A, B, C any](f func(A, B) C) func(A) func(B) C {
	return func(a A) func(B) C {
		return func(b B) C {
			return f(a, b)
		}
	}
}

// Uncurry inverts the Curry operation, turning a function that accepts one
// argument and returns a function accepting the second argument into a
// function that accepts both arguments up front. This is included for
// completeness, although you should expect to use it rarely.
func Uncurry[A, B, C any](f func(A) func(B) C) func(A, B) C {
	return func(a A, b B) C {
		return f(a)(b)
	}
}

// Const is a function that accepts an argument and returns a function that
// always returns that value irrespective of the returned function's argument.
// This is also quite useful in conjunction with higher order functions.
func Const[A, B any](a A) func(B) A {
	return func(_ B) A {
		return a
	}
}
