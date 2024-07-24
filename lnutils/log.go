package lnutils

// LogClosure is used to provide a closure over expensive logging operations so
// don't have to be performed when the logging level doesn't warrant it.
type LogClosure func() string

// String invokes the underlying function and returns the result.
func (c LogClosure) String() string {
	return c()
}

// NewLogClosure returns a new closure over a function that returns a string
// which itself provides a Stringer interface so that it can be used with the
// logging system.
func NewLogClosure(c func() string) LogClosure {
	return LogClosure(c)
}
