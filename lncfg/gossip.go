package lncfg

import (
	"time"

	"github.com/lightningnetwork/lnd/discovery"
	"github.com/lightningnetwork/lnd/routing/route"
)

//nolint:lll
type Gossip struct {
	PinnedSyncersRaw []string `long:"pinned-syncers" description:"A set of peers that should always remain in an active sync state, which can be used to closely synchronize the routing tables of two nodes. The value should be a hex-encoded pubkey, the flag can be specified multiple times to add multiple peers. Connected peers matching this pubkey will remain active for the duration of the connection and not count towards the NumActiveSyncer count."`

	PinnedSyncers discovery.PinnedSyncers

	MaxChannelUpdateBurst int `long:"max-channel-update-burst" description:"The maximum number of updates for a specific channel and direction that lnd will accept over the channel update interval."`

	ChannelUpdateInterval time.Duration `long:"channel-update-interval" description:"The interval used to determine how often lnd should allow a burst of new updates for a specific channel and direction."`

	SubBatchDelay time.Duration `long:"sub-batch-delay" description:"The duration to wait before sending the next announcement batch if there are multiple. Use a small value if there are a lot announcements and they need to be broadcast quickly."`
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
