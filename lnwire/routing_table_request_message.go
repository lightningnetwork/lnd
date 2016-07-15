// Copyright (c) 2016 Bitfury Group Limited
// Distributed under the MIT software license, see the accompanying
// file LICENSE or http://www.opensource.org/licenses/mit-license.php

package lnwire
import (
	"fmt"
	"io"
)

type RoutingTableRequestMessage struct {
	RoutingMessageBase
}

func (msg *RoutingTableRequestMessage) String() string {
	return fmt.Sprintf("RoutingTableRequestMessage{%v %v}", msg.SenderID, msg.ReceiverID)
}


func (msg *RoutingTableRequestMessage) Command() uint32{
	return CmdRoutingTableRequestMessage
}

func (msg *RoutingTableRequestMessage) Encode(w io.Writer, pver uint32) error{
	return nil
}

func (msg *RoutingTableRequestMessage) Decode(r io.Reader, pver uint32) error{
	return nil
}

func (msg *RoutingTableRequestMessage) MaxPayloadLength(uint32) uint32{
	return 0
}

func (msg *RoutingTableRequestMessage) Validate() error{
	return nil
}

var _ Message = (*RoutingTableRequestMessage)(nil)