package lnnet

import (
	"net"
)

// LightningNet is an interface housing a Dial function and several DNS functions.
type LightningNet interface {
	Dial(string, string) (net.Conn, error)
	LookupHost(string) ([]string, error)
	LookupSRV(string, string, string) (string, []*net.SRV, error)
	ResolveTCPAddr(string, string) (*net.TCPAddr, error)
}
