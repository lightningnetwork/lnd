package main

import (
	"fmt"
	"net"

	"github.com/lightningnetwork/lnd/lnwire"
)

// it listens for incoming messages on the lndc and hands it over
// to the OmniHandler via omnichan
func LNDCReceiver(l net.Conn, id [16]byte, r *rpcServer) error {
	for {
		msg := make([]byte, 65535)
		//	fmt.Printf("read message from %x\n", l.RemoteLNId)
		n, err := l.Read(msg)
		if err != nil {
			fmt.Printf("read error with %x: %s\n",
				id, err.Error())
			delete(r.CnMap, id)
			return l.Close()
		}
		msg = msg[:n]
		msg = append(id[:], msg...)
		r.OmniChan <- msg
	}
}

// handles stuff that comes in over the wire.  Not user-initiated.
func OmniHandler(r *rpcServer) {
	//	var err error
	var from [16]byte
	for {
		newdata := <-r.OmniChan // blocks here
		if len(newdata) < 17 {
			fmt.Printf("got too short message")
			continue
		}
		copy(from[:], newdata[:16])
		msg := newdata[16:]
		msgid := msg[0]
		_, ok := r.CnMap[from]
		if !ok {
			fmt.Printf("not connected to %x\n", from)
			continue
		}
		// TEXT MESSAGE.  SIMPLE
		if msgid == lnwire.MSGID_TEXTCHAT { //it's text
			fmt.Printf("msg from %x: %s\n", from, msg[1:])
			continue
		}
		// Based on MSGID, hand it off to functions
		fmt.Printf("Unknown message id byte %x", msgid)
		continue
	}
}
