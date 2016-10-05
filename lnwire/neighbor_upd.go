// Copyright (c) 2016 Bitfury Group Limited
// Distributed under the MIT software license, see the accompanying
// file LICENSE or http://www.opensource.org/licenses/mit-license.php

package lnwire

import (
	"fmt"
	"io"
)

type NeighborUpdMessage struct {
	Updates []ChannelOperation
}

func (msg *NeighborUpdMessage) Decode(r io.Reader, pver uint32) error {
	err := readElements(r, &msg.Updates)
	return err
}

func (msg *NeighborUpdMessage) Encode(w io.Writer, pver uint32) error {
	err := writeElements(w, msg.Updates)
	return err
}

func (msg *NeighborUpdMessage) Command() uint32 {
	return CmdNeighborUpdMessage
}

func (msg *NeighborUpdMessage) MaxPayloadLength(uint32) uint32 {
	// TODO: Insert some estimations
	return 1000000
}

func (msg *NeighborUpdMessage) Validate() error {
	// TODO: Add validation
	return nil
}

func (msg *NeighborUpdMessage) String() string {
	return fmt.Sprintf("NeighborUpdMessage{%v}", msg.Updates)
}

var _ Message = (*NeighborUpdMessage)(nil)
