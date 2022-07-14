package lnrpc

import "regexp"

var (
	// LndClientStreamingURIs is a list of all lnd RPCs that use a request-
	// streaming interface. Those request-streaming RPCs need to be handled
	// differently in the WebsocketProxy because of how the request body
	// parsing is implemented in the grpc-gateway library. Unfortunately
	// there is no straightforward way of obtaining this information on
	// runtime so we need to keep a hard coded list here.
	LndClientStreamingURIs = []*regexp.Regexp{
		regexp.MustCompile("^/v1/channels/acceptor$"),
		regexp.MustCompile("^/v1/channels/transaction-stream$"),
		regexp.MustCompile("^/v2/router/htlcinterceptor$"),
		regexp.MustCompile("^/v1/middleware$"),
	}

	// MaxGrpcMsgSize is used when we configure both server and clients to
	// allow sending/receiving at most 200 MiB GRPC messages.
	MaxGrpcMsgSize = 200 * 1024 * 1024
)
