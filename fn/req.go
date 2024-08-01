package fn

// Req is a type to encapsulate RPC-like calls wherein we send some data
// structure as a request, as well as a channel to receive the response on where
// the remote goroutine will send the result.
//
// NOTE: This construct should only be used for request/response patterns for
// which there is only a single response for the request.
type Req[Input any, Output any] struct {
	// Request is the data we are sending to the remote goroutine for
	// processing.
	Request Input

	// response is the channel on which we will receive the result of the
	// remote computation.
	response chan<- Output
}

// Dispatch is a convenience method that lifts a function that transforms the
// Input to the Output type into a full request handling cycle.
func (r *Req[Input, Output]) Dispatch(handler func(Input) Output) {
	r.Resolve(handler(r.Request))
}

// Resolve is a function that is used to send a value of the Output type back
// to the requesting thread.
func (r *Req[Input, Output]) Resolve(output Output) {
	select {
	case r.response <- output:
	default:
		// We do nothing here because the only situation in which this
		// case will fire is if the request handler attempts to resolve
		// a request more than once which is explicitly forbidden.
	}
}

// NewReq is the base constructor of the Req type. It returns both the packaged
// Req object as well as the receive side of the response channel that we will
// listen on for the response.
func NewReq[Input, Output any](input Input) (
	Req[Input, Output], <-chan Output) {

	// Always buffer the response channel so that the goroutine doing the
	// processing job does not block if the original requesting routine
	// takes an unreasonably long time to read the response.
	responseChan := make(chan Output, 1)

	return Req[Input, Output]{
		Request:  input,
		response: responseChan,
	}, responseChan
}
