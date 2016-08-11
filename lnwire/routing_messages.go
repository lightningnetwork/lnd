// Copyright (c) 2016 Bitfury Group Limited
// Distributed under the MIT software license, see the accompanying
// file LICENSE or http://www.opensource.org/licenses/mit-license.php

package lnwire

import (
	"github.com/BitfuryLightning/tools/rt/graph"
)

// RoutingMessageBase is the base struct for all routing messages within the
// lnwire package.
type RoutingMessageBase struct {
	// SenderID is the ID of the sender of the routing message.
	SenderID graph.ID

	// ReceiverID is the ID of the receiver of the routig message.
	ReceiverID graph.ID
}

// GetReceiverID returns the ID of the receiver of routing message.
func (msg RoutingMessageBase) GetReceiverID() graph.ID {
	return msg.ReceiverID
}

// GetSenderID returns the ID of the sender of the routing message.
func (msg RoutingMessageBase) GetSenderID() graph.ID {
	return msg.SenderID
}

// RoutingMessageBase is a shared interface for all routing messages.
type RoutingMessage interface {
	GetSenderID() graph.ID
	GetReceiverID() graph.ID
}
