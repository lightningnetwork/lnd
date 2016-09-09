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

type NeighborUpdMessage struct {
	DiffBuff *rt.DifferenceBuffer
}

func (msg *NeighborUpdMessage) Decode(r io.Reader, pver uint32) error {
	decoder := gob.NewDecoder(r)
	diffBuff := new(rt.DifferenceBuffer)
	err := decoder.Decode(diffBuff)
	msg.DiffBuff = diffBuff
	return err
}

func (msg *NeighborUpdMessage) Encode(w io.Writer, pver uint32) error {
	encoder := gob.NewEncoder(w)
	err := encoder.Encode(msg.DiffBuff)
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
	return fmt.Sprintf("NeighborUpdMessage{%v}", *msg.DiffBuff)
}

var _ Message = (*NeighborUpdMessage)(nil)
