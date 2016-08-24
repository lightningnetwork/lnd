// Copyright (c) 2016 Bitfury Group Limited
// Distributed under the MIT software license, see the accompanying
// file LICENSE or http://www.opensource.org/licenses/mit-license.php

package lnwire

import (
	"fmt"
	"io"
)

type NeighborAckMessage struct {
}

func (msg *NeighborAckMessage) String() string {
	return fmt.Sprintf("NeighborAckMessage{}",)
}

func (msg *NeighborAckMessage) Command() uint32 {
	return CmdNeighborAckMessage
}

func (msg *NeighborAckMessage) Encode(w io.Writer, pver uint32) error {
    // Transmission function work incorrect with empty messages so write some random string to make message not empty
    w.Write([]byte("NeighborAckMessage"))
	return nil
}

func (msg *NeighborAckMessage) Decode(r io.Reader, pver uint32) error {
	return nil
}

func (msg *NeighborAckMessage) MaxPayloadLength(uint32) uint32 {
	// Some random number. Transmission functions work bad if it is 0
	return 100

}

func (msg *NeighborAckMessage) Validate() error {
	return nil
}

var _ Message = (*NeighborAckMessage)(nil)
