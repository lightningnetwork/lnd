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

	// Response is the channel on which we will receive the result of the
	// remote computation.
	Response chan<- Output
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
		Response: responseChan,
	}, responseChan
}
