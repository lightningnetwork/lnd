// Copyright (c) 2016 Bitfury Group Limited
// Distributed under the MIT software license, see the accompanying
// file LICENSE or http://www.opensource.org/licenses/mit-license.php

package lnwire

import (
	"bytes"
	"testing"

	"github.com/roasbeef/btcd/wire"
	"reflect"
)

func genNeighborUpdMessage() *NeighborUpdMessage {
	p1 := samplePubKey(1)
	p2 := samplePubKey(2)
	p3 := samplePubKey(3)
	e1 := sampleOutPoint(4)
	e2 := sampleOutPoint(5)

	msg := NeighborUpdMessage{
		Updates: []ChannelOperation{
			{
				NodePubKey1: p1,
				NodePubKey2: p2,
				ChannelId: &e1,
				Capacity: 100000,
				Weight: 1.0,
				Operation: 0,
			},
			{
				NodePubKey1: p2,
				NodePubKey2: p3,
				ChannelId: &e2,
				Capacity: 210000,
				Weight: 2.0,
				Operation: 1,
			},
		},
	}
	return &msg
}

func TestNeighborUpdMessageEncodeDecode(t *testing.T) {
	msg1 := genNeighborUpdMessage()
	b := new(bytes.Buffer)
	err := msg1.Encode(b, 0)
	if err != nil {
		t.Fatalf("Can't encode message: %v", err)
	}
	msg2 := new(NeighborUpdMessage)
	err = msg2.Decode(b, 0)
	if err != nil {
		t.Fatalf("Can't decode message: %v", err)
	}

	// Assert equality of the two instances.
	if !reflect.DeepEqual(msg1, msg2) {
		t.Fatalf("encode/decode error messages don't match %v vs %v",
			msg1, msg2)
	}

}

func TestNeighborUpdMessageReadWrite(t *testing.T) {
	msg1 := genNeighborUpdMessage()
	b := new(bytes.Buffer)
	_, err := WriteMessage(b, msg1, 0, wire.SimNet)
	if err != nil {
		t.Fatalf("Can't write message %v", err)
	}
	_, msg2, _, err := ReadMessage(b, 0, wire.SimNet)
	if err != nil {
		t.Fatalf("Can't read message %v", err)
	}
	_, ok := msg2.(*NeighborUpdMessage)
	if !ok {
		t.Fatalf("Can't convert to *NeighborUpdMessage")
	}
	// Assert equality of the two instances.
	if !reflect.DeepEqual(msg1, msg2) {
		t.Fatalf("encode/decode error messages don't match %v vs %v",
			msg1, msg2)
	}

}
