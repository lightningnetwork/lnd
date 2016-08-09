// Copyright (c) 2016 Bitfury Group Limited
// Distributed under the MIT software license, see the accompanying
// file LICENSE or http://www.opensource.org/licenses/mit-license.php

package lnwire

import (
	"github.com/BitfuryLightning/tools/rt/graph"
)

type RoutingMessageBase struct {
	SenderID graph.ID
	ReceiverID graph.ID
}

func (msg RoutingMessageBase) GetReceiverID() graph.ID{
	return msg.ReceiverID
}

func (msg RoutingMessageBase) GetSenderID() graph.ID{
	return msg.SenderID
}

// Interface for all routing messages. All messages have sender and receiver
type RoutingMessage interface {
	GetSenderID() graph.ID
	GetReceiverID() graph.ID
}
