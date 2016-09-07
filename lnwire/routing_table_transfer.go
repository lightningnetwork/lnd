// Copyright (c) 2016 Bitfury Group Limited
// Distributed under the MIT software license, see the accompanying
// file LICENSE or http://www.opensource.org/licenses/mit-license.php

package lnwire

import (
	"encoding/gob"
	"fmt"
	"io"

	"github.com/BitfuryLightning/tools/rt"
)

type RoutingTableTransferMessage struct {
	RT *rt.RoutingTable
}

func (msg *RoutingTableTransferMessage) String() string {
	return fmt.Sprintf("RoutingTableTransferMessage{%v %v %v}", msg.RT)
}

func (msg *RoutingTableTransferMessage) Decode(r io.Reader, pver uint32) error {
	decoder := gob.NewDecoder(r)
	rt1 := rt.NewRoutingTable()
	err := decoder.Decode(rt1.G)
	msg.RT = rt1
	return err
}

func (msg *RoutingTableTransferMessage) Encode(w io.Writer, pver uint32) error {
	encoder := gob.NewEncoder(w)
	err := encoder.Encode(msg.RT.G)
	return err
}

func (msg *RoutingTableTransferMessage) Command() uint32 {
	return CmdRoutingTableTransferMessage
}

func (msg *RoutingTableTransferMessage) MaxPayloadLength(uint32) uint32 {
	// TODO: Insert some estimations
	return 1000000
}

func (msg *RoutingTableTransferMessage) Validate() error {
	// TODO: Add validation
	return nil
}

var _ Message = (*RoutingTableTransferMessage)(nil)
