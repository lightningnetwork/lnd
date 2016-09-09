// Copyright (c) 2016 Bitfury Group Limited
// Distributed under the MIT software license, see the accompanying
// file LICENSE or http://www.opensource.org/licenses/mit-license.php

package lnwire

import (
	"fmt"
	"io"
)

type NeighborRstMessage struct {
}

func (msg *NeighborRstMessage) String() string {
	return fmt.Sprintf("NeighborRstMessage{}")
}

func (msg *NeighborRstMessage) Command() uint32 {
	return CmdNeighborRstMessage
}

func (msg *NeighborRstMessage) Encode(w io.Writer, pver uint32) error {
	return nil
}

func (msg *NeighborRstMessage) Decode(r io.Reader, pver uint32) error {
	return nil
}

func (msg *NeighborRstMessage) MaxPayloadLength(uint32) uint32 {
	return 0
}

func (msg *NeighborRstMessage) Validate() error {
	return nil
}

var _ Message = (*NeighborRstMessage)(nil)
