// Copyright (c) 2016 Bitfury Group Limited
// Distributed under the MIT software license, see the accompanying
// file LICENSE or http://www.opensource.org/licenses/mit-license.php

package routing

import (
	"fmt"
	"github.com/lightningnetwork/lnd/routing/rt"
	"github.com/lightningnetwork/lnd/routing/rt/graph"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/roasbeef/btcd/wire"
)

func channelOperationsFromRT(r *rt.RoutingTable) []lnwire.ChannelOperation {
	channels := r.AllChannels()
	chOps := make([]lnwire.ChannelOperation, len(channels))
	for i:=0; i<len(channels); i++ {
		var info *graph.ChannelInfo
		if channels[i].Info != nil {
			info = channels[i].Info
		} else {
			info = new(graph.ChannelInfo)
		}
		chOp := lnwire.ChannelOperation{
			NodePubKey1: channels[i].Src.ToByte33(),
			NodePubKey2: channels[i].Tgt.ToByte33(),
			ChannelId:   (*wire.OutPoint)(&channels[i].Id),
			Capacity: info.Cpt,
			Weight:   info.Wgt,
			Operation: byte(rt.AddChannelOP),
		}
		chOps[i] = chOp
	}
	return chOps
}

func channelOperationsFromDiffBuff(r rt.DifferenceBuffer) []lnwire.ChannelOperation {
	chOps := make([]lnwire.ChannelOperation, len(r))
	for i:=0; i<len(r); i++ {
		var info *graph.ChannelInfo
		if r[i].Info != nil {
			info = r[i].Info
		} else {
			info = new(graph.ChannelInfo)
		}
		chOp := lnwire.ChannelOperation{
			NodePubKey1: r[i].Src.ToByte33(),
			NodePubKey2: r[i].Tgt.ToByte33(),
			ChannelId:   (*wire.OutPoint)(&r[i].Id),
			Capacity: info.Cpt,
			Weight:   info.Wgt,
			Operation: byte(r[i].Operation),
		}
		chOps[i] = chOp
	}
	return chOps
}

func rtFromChannelOperations(chOps []lnwire.ChannelOperation) *rt.RoutingTable {
	r := rt.NewRoutingTable()
	for i := 0; i<len(chOps); i++{
		r.AddChannel(
			graph.NewVertex(chOps[i].NodePubKey1[:]),
			graph.NewVertex(chOps[i].NodePubKey2[:]),
			graph.EdgeID(*chOps[i].ChannelId),
			&graph.ChannelInfo{
				Cpt: chOps[i].Capacity,
				Wgt: chOps[i].Weight,
			},
		)
	}
	return r
}

func diffBuffFromChannelOperations(chOps []lnwire.ChannelOperation) *rt.DifferenceBuffer {
	d := rt.NewDifferenceBuffer()
	for i := 0; i<len(chOps); i++ {
		op := rt.NewChannelOperation(
			graph.NewVertex(chOps[i].NodePubKey1[:]),
			graph.NewVertex(chOps[i].NodePubKey2[:]),
			graph.EdgeID(*chOps[i].ChannelId),
			&graph.ChannelInfo{
				Cpt: chOps[i].Capacity,
				Wgt: chOps[i].Weight,
			},
			rt.OperationType(chOps[i].Operation),
		)
		*d = append(*d, op)
	}
	return d
}

// RoutingMessage is a wrapper around lnwire.Message which
// includes sender and receiver.
type RoutingMessage struct {
	SenderID   graph.Vertex
	ReceiverID graph.Vertex
	Msg        lnwire.Message
}

type addChannelCmd struct {
	Id1, Id2 graph.Vertex
	TxID     graph.EdgeID
	Info     *graph.ChannelInfo
	err      chan error
}

type removeChannelCmd struct {
	Id1, Id2 graph.Vertex
	TxID     graph.EdgeID
	err      chan error
}

type hasChannelCmd struct {
	Id1, Id2 graph.Vertex
	TxID     graph.EdgeID
	rez      chan bool
	err      chan error
}

type openChannelCmd struct {
	Id   graph.Vertex
	TxID graph.EdgeID
	info *graph.ChannelInfo
	err  chan error
}

type findPathCmd struct {
	Id      graph.Vertex
	rez     chan []graph.Vertex
	err     chan error
}

type findKShortestPathsCmd struct {
	Id      graph.Vertex
	k       int
	rez     chan [][]graph.Vertex
	err     chan error
}

type getRTCopyCmd struct {
	rez chan *rt.RoutingTable
}

type NeighborState int

const (
	StateINIT NeighborState = 0
	StateACK  NeighborState = 1
	StateWAIT NeighborState = 2
)

type neighborDescription struct {
	Id       graph.Vertex
	DiffBuff *rt.DifferenceBuffer
	State    NeighborState
}

// RoutingConfig contains configuration information for RoutingManager.
type RoutingConfig struct {
	// SendMessage is used by the routing manager to send a
	// message to a direct neighbor.
	SendMessage func([33]byte, lnwire.Message) error
}

// RoutingManager implements routing functionality.
type RoutingManager struct {
	// Current node.
	Id        graph.Vertex
	// Neighbors of the current node.
	neighbors map[graph.Vertex]*neighborDescription
	// Routing table.
	rT        *rt.RoutingTable
	// Configuration parameters.
	config    *RoutingConfig
	// Channel for input messages
	chIn      chan interface{}
	// Closing this channel will stop RoutingManager.
	chQuit    chan struct{}
	// When RoutingManager stops this channel is closed.
	ChDone    chan struct{}
}

// NewRoutingManager creates new RoutingManager
// with empyt routing table.
func NewRoutingManager(Id graph.Vertex, config *RoutingConfig) *RoutingManager {
	return &RoutingManager{
		Id:                Id,
		neighbors:         make(map[graph.Vertex]*neighborDescription),
		rT:                rt.NewRoutingTable(),
		config:            config,
		chIn:              make(chan interface{}, 10),
		chQuit:            make(chan struct{}, 1),
		ChDone:            make(chan struct{}),
	}
}

// Start - start message loop.
func (r *RoutingManager) Start() {
	go func() {
	out:
		for {
			// Prioritise quit.
			select {
			case <-r.chQuit:
				break out
			default:
			}
			select {
			case msg := <-r.chIn:
				r.handleMessage(msg)
			case <-r.chQuit:
				break out
			}
		}
		close(r.ChDone)
	}()
}

// Stop stops RoutingManager.
// Note if some messages were not processed they will be skipped.
func (r *RoutingManager) Stop() {
	close(r.chQuit)
}

func (r *RoutingManager) handleMessage(msg interface{}) {
	switch msg := msg.(type) {
	case *openChannelCmd:
		r.handleOpenChannelCmdMessage(msg)
	case *addChannelCmd:
		r.handleAddChannelCmdMessage(msg)
	case *hasChannelCmd:
		r.handleHasChannelCmdMessage(msg)
	case *removeChannelCmd:
		r.handleRemoveChannelCmdMessage(msg)
	case *findPathCmd:
		r.handleFindPath(msg)
	case *findKShortestPathsCmd:
		r.handleFindKShortestPaths(msg)
	case *getRTCopyCmd:
		r.handleGetRTCopy(msg)
	case *RoutingMessage:
		r.handleRoutingMessage(msg)
	default:
		fmt.Println("Unknown message type ", msg)
	}
}

// notifyNeighbors checks if there are
// pending changes for each neighbor and send them.
// Each neighbor has three states
// StateINIT - initial state. No messages has been send to this neighbor
// StateWAIT - node waits fo acknowledgement.
// StateACK - acknowledgement has been obtained. New updates can be send.
func (r *RoutingManager) notifyNeighbors() {
	for _, neighbor := range r.neighbors {
		if neighbor.State == StateINIT {
			neighbor.DiffBuff.Clear()
			msg := &lnwire.NeighborHelloMessage{
				Channels: channelOperationsFromRT(r.rT),
			}
			r.sendRoutingMessage(msg, neighbor.Id)
			neighbor.State = StateWAIT
			continue
		}
		if neighbor.State == StateACK && !neighbor.DiffBuff.IsEmpty() {
			msg := &lnwire.NeighborUpdMessage{
				Updates: channelOperationsFromDiffBuff(*neighbor.DiffBuff),
			}
			r.sendRoutingMessage(msg, neighbor.Id)
			neighbor.DiffBuff.Clear()
			neighbor.State = StateWAIT
		}
	}
}

// AddChannel add channel to routing tables.
func (r *RoutingManager) AddChannel(Id1, Id2 graph.Vertex, TxID graph.EdgeID, info *graph.ChannelInfo) error {
	msg := &addChannelCmd{
		Id1:  Id1,
		Id2:  Id2,
		TxID: TxID,
		Info: info,
		err:  make(chan error, 1),
	}
	r.chIn <- msg
	return <-msg.err
}

// HasChannel checks if there are channel in routing table
func (r *RoutingManager) HasChannel(Id1, Id2 graph.Vertex, TxID graph.EdgeID) bool {
	msg := &hasChannelCmd{
		Id1:  Id1,
		Id2:  Id2,
		TxID: TxID,
		rez:  make(chan bool, 1),
		err:  make(chan error, 1),
	}
	r.chIn <- msg
	return <-msg.rez
}

// RemoveChannel removes channel from routing table
func (r *RoutingManager) RemoveChannel(Id1, Id2 graph.Vertex, TxID graph.EdgeID) error {
	msg := &removeChannelCmd{
		Id1:  Id1,
		Id2:  Id2,
		TxID: TxID,
		err:  make(chan error, 1),
	}
	r.chIn <- msg
	return <-msg.err
}

// OpenChannel is used to open channel from this node to other node.
// It adds node to neighbors and starts routing tables exchange.
func (r *RoutingManager) OpenChannel(Id graph.Vertex, TxID graph.EdgeID, info *graph.ChannelInfo) error {
	msg := &openChannelCmd{
		Id:   Id,
		TxID: TxID,
		info: info,
		err:  make(chan error, 1),
	}
	r.chIn <- msg
	return <-msg.err
}

// FindPath finds path from this node to some other node
func (r *RoutingManager) FindPath(destId graph.Vertex) ([]graph.Vertex, error) {
	msg := &findPathCmd{
		Id:      destId,
		rez:     make(chan []graph.Vertex, 1),
		err:     make(chan error, 1),
	}
	r.chIn <- msg
	return <-msg.rez, <-msg.err
}

func (r *RoutingManager) handleFindPath(msg *findPathCmd) {
	path, err := r.rT.ShortestPath(r.Id, msg.Id)
	msg.rez <- path
	msg.err <- err
}

// FindKShortesPaths tries to find k paths from this node to destination.
// If timeouts returns all found paths
func (r *RoutingManager) FindKShortestPaths(destId graph.Vertex, k int) ([][]graph.Vertex, error) {
		msg := &findKShortestPathsCmd{
		Id:      destId,
		k:       k,
		rez:     make(chan [][]graph.Vertex, 1),
		err:     make(chan error, 1),
	}
	r.chIn <- msg
	return <-msg.rez, <-msg.err
}

// Find k-shortest path.
func (r *RoutingManager) handleFindKShortestPaths(msg *findKShortestPathsCmd) {
	paths, err := r.rT.KShortestPaths(r.Id, msg.Id, msg.k)
	msg.rez <- paths
	msg.err <- err
}

func (r *RoutingManager) handleGetRTCopy(msg *getRTCopyCmd) {
	msg.rez <- r.rT.Copy()
}

// GetRTCopy - returns copy of current node routing table.
// Note: difference buffers are not copied.
func (r *RoutingManager) GetRTCopy() *rt.RoutingTable {
	msg := &getRTCopyCmd{
		rez: make(chan *rt.RoutingTable, 1),
	}
	r.chIn <- msg
	return <-msg.rez
}

func (r *RoutingManager) handleOpenChannelCmdMessage(msg *openChannelCmd) {
	// TODO: validate that channel do not exist
	r.rT.AddChannel(r.Id, msg.Id, msg.TxID, msg.info)
	// TODO(mkl): what to do if neighbot already exists.
	r.neighbors[msg.Id] = &neighborDescription{
		Id:       msg.Id,
		DiffBuff: r.rT.NewDiffBuff(),
		State:    StateINIT,
	}
	r.notifyNeighbors()
	msg.err <- nil
}

func (r *RoutingManager) handleAddChannelCmdMessage(msg *addChannelCmd) {
	r.rT.AddChannel(msg.Id1, msg.Id2, msg.TxID, msg.Info)
	msg.err <- nil
}

func (r *RoutingManager) handleHasChannelCmdMessage(msg *hasChannelCmd) {
	msg.rez <- r.rT.HasChannel(msg.Id1, msg.Id2, msg.TxID)
	msg.err <- nil
}

func (r *RoutingManager) handleRemoveChannelCmdMessage(msg *removeChannelCmd) {
	r.rT.RemoveChannel(msg.Id1, msg.Id2, msg.TxID)
	r.notifyNeighbors()
	msg.err <- nil
}

func (r *RoutingManager) handleNeighborHelloMessage(msg *lnwire.NeighborHelloMessage, senderID graph.Vertex) {
	// Sometimes we can obtain NeighborHello message from node that is
	// not our neighbor yet. Because channel creation workflow
	// end in different times for nodes.
	t := rtFromChannelOperations(msg.Channels)
	r.rT.AddTable(t)
	r.sendRoutingMessage(&lnwire.NeighborAckMessage{}, senderID)
	r.notifyNeighbors()
}

func (r *RoutingManager) handleNeighborUpdMessage(msg *lnwire.NeighborUpdMessage, senderID graph.Vertex) {
	if _, ok := r.neighbors[senderID]; ok {
		diffBuff := diffBuffFromChannelOperations(msg.Updates)
		r.rT.ApplyDiffBuff(diffBuff)
		r.sendRoutingMessage(&lnwire.NeighborAckMessage{}, senderID)
		r.notifyNeighbors()
	}
}

func (r *RoutingManager) handleNeighborRstMessage(msg *lnwire.NeighborRstMessage, senderID graph.Vertex) {
	if _, ok := r.neighbors[senderID]; ok {
		r.neighbors[senderID].State = StateINIT
		r.notifyNeighbors()
	}
}

func (r *RoutingManager) handleNeighborAckMessage(msg *lnwire.NeighborAckMessage, senderID graph.Vertex) {
	if _, ok := r.neighbors[senderID]; ok && r.neighbors[senderID].State == StateWAIT {
		r.neighbors[senderID].State = StateACK
		// In case there are new updates for node which
		// appears between sending NeighborUpd and NeighborAck
		r.notifyNeighbors()
	}
}

func (r *RoutingManager) handleRoutingMessage(rmsg *RoutingMessage) {
	msg := rmsg.Msg
	switch msg := msg.(type) {
	case *lnwire.NeighborHelloMessage:
		r.handleNeighborHelloMessage(msg, rmsg.SenderID)
	case *lnwire.NeighborUpdMessage:
		r.handleNeighborUpdMessage(msg, rmsg.SenderID)
	case *lnwire.NeighborRstMessage:
		r.handleNeighborRstMessage(msg, rmsg.SenderID)
	case *lnwire.NeighborAckMessage:
		r.handleNeighborAckMessage(msg, rmsg.SenderID)
	default:
		fmt.Printf("Unknown message type %T\n inside RoutingMessage", msg)
	}
}

func (r *RoutingManager) sendRoutingMessage(msg lnwire.Message, receiverId graph.Vertex) {
	r.config.SendMessage(receiverId.ToByte33(), msg)
}

func (r *RoutingManager) ReceiveRoutingMessage(msg lnwire.Message, senderID graph.Vertex) {
	r.chIn <- &RoutingMessage{
		SenderID:   senderID,
		ReceiverID: r.Id,
		Msg:        msg,
	}
}