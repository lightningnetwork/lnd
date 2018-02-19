package main

import (
	"strings"
)

var (
	PubKey                 = "02c39955c1579afe4824dc0ef4493fdf7f3760b158cf6d367d8570b9f19683afb4"
	GoodAddress            = "02c39955c1579afe4824dc0ef4493fdf7f3760b158cf6d367d8570b9f19683afb4@bitcoin.org:1234"
	GoodAddressWithoutPort = "02c39955c1579afe4824dc0ef4493fdf7f3760b158cf6d367d8570b9f19683afb4@bitcoin.org"
	BadAddress             = "02c39955c1579afe4824dc0ef4493fdf7f3760b158cf6d367d8570b9f19683afb4"

	PeerIdInt      int32 = 321
	PeerId               = "321"
	LocalAmountInt int64 = 10000
	LocalAmount          = "10000"
	PushAmountInt  int64 = 5000
	PushAmount           = "5000"
)

type StringWriter struct {
	outputs []string
}

func (w *StringWriter) Write(p []byte) (n int, err error) {
	w.outputs = append(w.outputs, string(p))
	return len(p), nil
}

func (w *StringWriter) Join() string {
	return strings.Join(w.outputs, "\n")
}
