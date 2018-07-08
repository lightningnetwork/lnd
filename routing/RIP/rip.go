package RIP

import (
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
)

type RIPRouter struct {
	RouteTable []ripRouterEntry
	SelfNode   [33]byte
	DB         *channeldb.DB
	quit       chan int
}

type ripRouterEntry struct {
	Dest     [33]byte
	NextHop  [33]byte
	Distance int8
}

func (r *RIPRouter) start() {

}

func (r *RIPRouter) stop() {

}

func (r *RIPRouter) handleUpdate(update *lnwire.RIPUpdate) {

}

func (r *RIPRouter) sendUpdate(updade *lnwire.RIPUpdate) {

}
