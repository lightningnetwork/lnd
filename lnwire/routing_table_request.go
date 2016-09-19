// Copyright (c) 2016 Bitfury Group Limited
// Distributed under the MIT software license, see the accompanying
// file LICENSE or http://www.opensource.org/licenses/mit-license.php

package lnwire

import (
	"fmt"
	"io"
)

type RoutingTableRequestMessage struct {
}

func (msg *RoutingTableRequestMessage) String() string {
	return fmt.Sprintf("RoutingTableRequestMessage{}")
}

func (msg *RoutingTableRequestMessage) Command() uint32 {
	return CmdRoutingTableRequestMessage
}

func (msg *RoutingTableRequestMessage) Encode(w io.Writer, pver uint32) error {
	_, err := w.Write([]byte("RoutingTableRequestMessage"))
	return err
}

func (msg *RoutingTableRequestMessage) Decode(r io.Reader, pver uint32) error {
	var b [26]byte
	_, err := r.Read(b[:])
	if string(b[:]) != "RoutingTableRequestMessage"{
		err = fmt.Errorf("Can't read RoutingTableRequestMessage message")
	}
	return err
}

func (msg *RoutingTableRequestMessage) MaxPayloadLength(uint32) uint32 {
	return 31
}

func (msg *RoutingTableRequestMessage) Validate() error {
	return nil
}

var _ Message = (*RoutingTableRequestMessage)(nil)
