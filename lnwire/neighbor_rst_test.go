// Copyright (c) 2016 Bitfury Group Limited
// Distributed under the MIT software license, see the accompanying
// file LICENSE or http://www.opensource.org/licenses/mit-license.php

package lnwire

import (
       "bytes"
       "testing"

       "github.com/roasbeef/btcd/wire"
)

func TestNeighborRstMessageEncodeDecode(t *testing.T) {
       b := new(bytes.Buffer)
       msg1 := NeighborRstMessage{}
       err := msg1.Encode(b, 0)
       if err != nil {
               t.Fatalf("Can't encode message ", err)
       }
       msg2 := new(NeighborRstMessage)
       err = msg2.Decode(b, 0)
       if err != nil {
               t.Fatalf("Can't decode message %v", err)
       }
}

func TestNeighborRstMessageReadWrite(t *testing.T){
       b := new(bytes.Buffer)
       msg1 := &NeighborRstMessage{}
       _, err := WriteMessage(b, msg1, 0, wire.SimNet)
       if err != nil {
               t.Fatalf("Can't write message %v", err)
       }
       _, msg2, _, err :=  ReadMessage(b, 0, wire.SimNet)
       if err != nil {
               t.Fatalf("Can't read message %v", err)
       }
       _, ok := msg2.(*NeighborRstMessage)
       if !ok {
               t.Fatalf("Can't convert to *NeighborRstMessage")
       }
}

