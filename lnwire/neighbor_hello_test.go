// Copyright (c) 2016 Bitfury Group Limited
// Distributed under the MIT software license, see the accompanying
// file LICENSE or http://www.opensource.org/licenses/mit-license.php

package lnwire

import (
	"bytes"
	"testing"

	"github.com/BitfuryLightning/tools/rt"
	"github.com/BitfuryLightning/tools/rt/graph"
)

func TestNeighborHelloMessageEncodeDecode(t *testing.T) {
	Id1 := graph.NewID(1)
	Id2 := graph.NewID(2)
	rt1 := rt.NewRoutingTable()
	rt1.AddChannel(Id1, Id2, graph.NewEdgeID("1"), &rt.ChannelInfo{1, 1})
	b := new(bytes.Buffer)
	msg1 := NeighborHelloMessage{RT: rt1}
	err := msg1.Encode(b, 0)
	if err != nil {
		t.Fatalf("Can't encode message ", err)
	}
	msg2 := new(NeighborHelloMessage)
	err = msg2.Decode(b, 0)
	if err != nil {
		t.Fatalf("Can't decode message ", err)
	}
	if msg2.RT == nil {
		t.Fatal("After decoding RT should not be nil")
	}
	if !msg2.RT.HasChannel(Id1, Id2, nil) {
		t.Errorf("msg2.RT.HasChannel(Id1, Id2) = false, want true")
	}
	if !msg2.RT.HasChannel(Id2, Id1, nil) {
		t.Errorf("msg2.RT.HasChannel(Id2, Id1) = false, want true")
	}
}
