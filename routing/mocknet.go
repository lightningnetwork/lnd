// Copyright (c) 2016 Bitfury Group Limited
// Distributed under the MIT software license, see the accompanying
// file LICENSE or http://www.opensource.org/licenses/mit-license.php

package routing

import (
	"fmt"
	"github.com/lightningnetwork/lnd/routing/rt/graph"
	"github.com/lightningnetwork/lnd/lnwire"
)

type MockNetwork struct {
	nodes         map[graph.Vertex]*RoutingManager
	chMsg         chan *RoutingMessage
	chQuit        chan struct{}
	printMessages bool
}

func NewMockNetwork(printMessages bool) *MockNetwork {
	return &MockNetwork{
		nodes:         make(map[graph.Vertex]*RoutingManager),
		chMsg:         make(chan *RoutingMessage),
		chQuit:        make(chan struct{}),
		printMessages: printMessages,
	}
}

func (net *MockNetwork) Start() {
	go func() {
		for {
			select {
			case msg, ok := <-net.chMsg:
				if !ok {
					return
				}
				receiverId := msg.ReceiverID
				// TODO: validate ReceiverID
				if net.printMessages {
					fmt.Println(msg.Msg)
				}
				if _, ok := net.nodes[receiverId]; ok {
					net.nodes[receiverId].ReceiveRoutingMessage(msg.Msg, msg.SenderID)
				}
			case <-net.chQuit:
				return
			}
		}
	}()
}

func (net *MockNetwork) Stop() {
	close(net.chQuit)
}

func (net *MockNetwork) Add(r *RoutingManager) {
	net.nodes[r.Id] = r
	chOut := make(chan *RoutingMessage)
	if r.config == nil {
		r.config = &RoutingConfig{}
	}
	r.config.SendMessage = func(receiver [33]byte, msg lnwire.Message) error {
		chOut <- &RoutingMessage{
			SenderID: r.Id,
			ReceiverID: graph.NewVertex(receiver[:]),
			Msg: msg,
		}
		return nil
	}
	go func() {
		for {
			select {
			case msg, ok := <-chOut:
				if !ok {
					return
				}
				net.chMsg <- msg
			case <-net.chQuit:
				return
			}
		}
	}()
}
