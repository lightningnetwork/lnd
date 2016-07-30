// Copyright (c) 2016 Bitfury Group Limited
// Distributed under the MIT software license, see the accompanying
// file LICENSE or http://www.opensource.org/licenses/mit-license.php

package lnwire

import (
	"io"
	"github.com/BitfuryLightning/tools/rt"
	"encoding/gob"
	"fmt"
)

type NeighborHelloMessage struct{
	RoutingMessageBase
	RT *rt.RoutingTable
}

func (msg *NeighborHelloMessage) Decode(r io.Reader, pver uint32) error{
	decoder := gob.NewDecoder(r)
	rt1 := rt.NewRoutingTable()
	err := decoder.Decode(rt1.G)
	msg.RT = rt1
	return err
}

func (msg *NeighborHelloMessage) Encode(w io.Writer, pver uint32) error{
	encoder := gob.NewEncoder(w)
	err := encoder.Encode(msg.RT.G)
	return err
}

func (msg *NeighborHelloMessage) Command() uint32{
	return CmdNeighborHelloMessage
}

func (msg *NeighborHelloMessage) MaxPayloadLength(uint32) uint32{
	// TODO: Insert some estimations
	return 1000000
}

func (msg *NeighborHelloMessage) Validate() error{
	// TODO: Add validation
	return nil
}

func (msg *NeighborHelloMessage) String() string{
	return fmt.Sprintf("NeighborHelloMessage{%v %v %v}", msg.SenderID, msg.ReceiverID, msg.RT)
}


var _  Message = (*NeighborHelloMessage)(nil)
