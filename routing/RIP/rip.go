package RIP

import (
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
	"sync"
	"bytes"
	"github.com/roasbeef/btcd/btcec"
)

const NUM_RIP_BUFFER = 10

type RIPRouter struct {
	RouteTable   []ripRouterEntry
	SelfNode     [33]byte
	UpdateBuffer chan *lnwire.RIPUpdate
	Neighbours 	 [][33]byte
	SendToPeer func(target *btcec.PublicKey, msgs ...lnwire.Message) error
	DB           *channeldb.DB
	quit         chan struct{}
}

type ripRouterEntry struct {
	Dest     [33]byte
	NextHop  [33]byte
	Distance int8
}

func newRIPRouter(db *channeldb.DB, selfNode [33]byte) *RIPRouter {
	return &RIPRouter{
		DB:           db,
		SelfNode:     selfNode,
		UpdateBuffer: make(chan *lnwire.RIPUpdate, NUM_RIP_BUFFER),
		RouteTable:   []ripRouterEntry{},
		quit:         make(chan struct{}),	
	}
}

func (r *RIPRouter) start(wg *sync.WaitGroup) {
	defer wg.Done()
	for {
		select {
			case update := <- r.UpdateBuffer:
				// TODO
				r.handleUpdate(update, [33]byte{})
			case <- r.quit:
				//TODO: add log
				return
		}
	}
}

func (r *RIPRouter) stop() {
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
			Distance:distance,
			Dest:dest,
		}
		copy(newEntry.NextHop[:], dest[:])
		r.RouteTable = append(r.RouteTable, newEntry)
	}

	// BroadCast this update to his neighbours
	if ifUpdate {
		for _, peer := range r.Neighbours {
			if !bytes.Equal(peer[:], source[:]){
				peerPubKey, err := btcec.ParsePubKey(peer[:], btcec.S256())
				if err != nil {
					//TODO(xuehan): return multi err
					return err
				}
				update := lnwire.RIPUpdate{
					Distance: distance,
				}
				copy(update.Destination[:],dest[:])
				r.SendToPeer(peerPubKey, &update)
			}
		}
	}
	return nil
}

