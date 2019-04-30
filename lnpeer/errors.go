package lnpeer

import "fmt"

var (
	// ErrPeerExiting signals that the peer received a disconnect request.
	ErrPeerExiting = fmt.Errorf("peer exiting")
)
