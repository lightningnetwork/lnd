// Copyright (c) 2016 Bitfury Group Limited
// Distributed under the MIT software license, see the accompanying
// file LICENSE or http://www.opensource.org/licenses/mit-license.php

package lnwire

import (
	"fmt"
	"io"
)

type NeighborHelloMessage struct {
	// List of channels
	Channels []ChannelOperation
}

func (msg *NeighborHelloMessage) Decode(r io.Reader, pver uint32) error {
	err := readElements(r, &msg.Channels)
	return err
}

func (msg *NeighborHelloMessage) Encode(w io.Writer, pver uint32) error {
	err := writeElement(w, msg.Channels)
	return err
}

func (msg *NeighborHelloMessage) Command() uint32 {
	return CmdNeighborHelloMessage
}

func (msg *NeighborHelloMessage) MaxPayloadLength(uint32) uint32 {
	// TODO: Insert some estimations
	return 1000000
}

func (msg *NeighborHelloMessage) Validate() error {
	// TODO: Add validation
	return nil
}

func (msg *NeighborHelloMessage) String() string {
	return fmt.Sprintf("NeighborHelloMessage{%v}", msg.Channels)
}

var _ Message = (*NeighborHelloMessage)(nil)
