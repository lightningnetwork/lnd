package RIP

import (
	"bytes"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/roasbeef/btcd/btcec"
	"sync"
)

const NUM_RIP_BUFFER = 10

const (
	LINK_ADD    = 1
	LINK_REMOVE = 2
)

type RIPRouter struct {
	RouteTable   []ripRouterEntry
	SelfNode     [33]byte
	UpdateBuffer chan *lnwire.RIPUpdate
	LinkChangeChan chan *LinkChange
	Neighbours   map[[33]byte]struct{}
	SendToPeer   func(target *btcec.PublicKey, msgs ...lnwire.Message) error
	DB           *channeldb.DB
	quit         chan struct{}
}

type ripRouterEntry struct {
	Dest     [33]byte
	NextHop  [33]byte
	Distance int8
}

type LinkChange struct {
	ChangeType  int
	NeighbourID [33]byte
	//TODO(xuehan):add  balance change
}

func NewRIPRouter(db *channeldb.DB, selfNode [33]byte) *RIPRouter {
	return &RIPRouter{
		DB:           db,
		SelfNode:     selfNode,
		UpdateBuffer: make(chan *lnwire.RIPUpdate, NUM_RIP_BUFFER),
		LinkChangeChan: make(chan *LinkChange),
		RouteTable:   []ripRouterEntry{},
		quit:         make(chan struct{}),
	}
}

func (r *RIPRouter) Start(wg *sync.WaitGroup) {
	defer wg.Done()
	for {
		select {
		case update := <-r.UpdateBuffer:
			// TODO
			r.handleUpdate(update, [33]byte{})

		case linkChange := <- r.LinkChangeChan:
			switch linkChange.ChangeType {
			// If there is a new neighbour, we handle it.
			case LINK_ADD:
				// Add the neighbour
				r.Neighbours[linkChange.NeighbourID] = struct{}{}
				addUpdate := &lnwire.RIPUpdate{
					Distance: 0,
				}
				copy(addUpdate.Destination[:], linkChange.NeighbourID[:])
				r.handleUpdate(addUpdate, linkChange.NeighbourID)

			// If the remote node was down or the channel was closed,
			// We delete it and notify our neighbour.
			case LINK_REMOVE:
				delete(r.Neighbours, linkChange.NeighbourID)
				rmUpdate := &lnwire.RIPUpdate{
					Distance: 16,
				}
				copy(rmUpdate.Destination[:], linkChange.NeighbourID[:])
				r.handleUpdate(rmUpdate, linkChange.NeighbourID)
			}

		case <-r.quit:
			//TODO: add log
			return
		}
	}
}

func (r *RIPRouter) Stop() {
	close(r.quit)
}

func (r *RIPRouter) handleUpdate(update *lnwire.RIPUpdate, source [33]byte) error {
	nextHop := r.SelfNode
	distance := update.Distance + 1
	dest := update.Destination

	if distance >= 16 {
		distance = 16
	}

	ifUpdate := false
	ifExist := false
	// Update the router table
	for _, entry := range r.RouteTable {
		if bytes.Equal(entry.Dest[:], dest[:]) {
			ifExist = true
			if bytes.Equal(entry.NextHop[:], nextHop[:]) {
				entry.Distance = distance
				ifUpdate = true
			} else {
				if entry.Distance > distance {
					copy(entry.NextHop[:], dest[:])
					entry.Distance = distance
					ifUpdate = true
				}
			}
		}
	}
	// if this entry is not in the route table, we add this update into table
	if !ifExist {
		newEntry := ripRouterEntry{
			Distance: distance,
			Dest:     dest,
		}
		copy(newEntry.NextHop[:], dest[:])
		r.RouteTable = append(r.RouteTable, newEntry)
	}

	// BroadCast this update to his neighbours
	if ifUpdate {
		for peer, _ := range r.Neighbours {
			if !bytes.Equal(peer[:], source[:]) {
				peerPubKey, err := btcec.ParsePubKey(peer[:], btcec.S256())
				if err != nil {
					//TODO(xuehan): return multi err
					return err
				}
				update := lnwire.RIPUpdate{
					Distance: distance,
				}
				copy(update.Destination[:], dest[:])
				r.SendToPeer(peerPubKey, &update)
			}
		}
	}
	return nil
}
