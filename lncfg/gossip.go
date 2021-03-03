package lncfg

import (
	"time"

	"github.com/lightningnetwork/lnd/discovery"
	"github.com/lightningnetwork/lnd/routing/route"
)

type Gossip struct {
	PinnedSyncersRaw []string `long:"pinned-syncers" description:"A set of peers that should always remain in an active sync state, which can be used to closely synchronize the routing tables of two nodes. The value should be comma separated list of hex-encoded pubkeys. Connected peers matching this pubkey will remain active for the duration of the connection and not count towards the NumActiveSyncer count."`

	PinnedSyncers discovery.PinnedSyncers

	MaxChannelUpdateBurst int `long:"max-channel-update-burst" description:"The maximum number of updates for a specific channel and direction that lnd will accept over the channel update interval."`

	ChannelUpdateInterval time.Duration `long:"channel-update-interval" description:"The interval used to determine how often lnd should allow a burst of new updates for a specific channel and direction."`
}

// Parse the pubkeys for the pinned syncers.
func (g *Gossip) Parse() error {
	pinnedSyncers := make(discovery.PinnedSyncers)
	for _, pubkeyStr := range g.PinnedSyncersRaw {
		vertex, err := route.NewVertexFromStr(pubkeyStr)
		if err != nil {
			return err
		}
		pinnedSyncers[vertex] = struct{}{}
	}

	g.PinnedSyncers = pinnedSyncers

	return nil
}
