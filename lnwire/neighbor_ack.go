// Copyright (c) 2016 Bitfury Group Limited
// Distributed under the MIT software license, see the accompanying
// file LICENSE or http://www.opensource.org/licenses/mit-license.php

package lnwire

import (
	"fmt"
	"io"
	"bytes"
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
	buff := make([]byte, 18)
	_, err := r.Read(buff)
	if err != nil {
		return err
	}
	if !bytes.Equal(buff, []byte("NeighborAckMessage")) {
		fmt.Errorf("Can't decode NeighborAckMessage message")
	}
	return nil
}

func (msg *NeighborAckMessage) MaxPayloadLength(uint32) uint32 {
	// Length of the string "NeighborAckMessage" used in Encode
	// Transmission functions work bad if it is 0
	return 18

}

func (msg *NeighborAckMessage) Validate() error {
	return nil
}

var _ Message = (*NeighborAckMessage)(nil)
