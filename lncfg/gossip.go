package lncfg

import (
	"fmt"
	"time"

	"github.com/lightningnetwork/lnd/discovery"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

// minAnnouncementConf defines the minimal num of confs needed for the config
// AnnouncementConf. We choose 3 here as it's unlikely a reorg depth of 3 would
// happen.
//
// NOTE: The specs recommends setting this value to 6, which is the default
// value used for AnnouncementConf. However the receiver should be able to
// decide which channels to be included in its local graph, more details can be
// found:
// - https://github.com/lightning/bolts/pull/1215#issuecomment-2557337202
const minAnnouncementConf = 3

//nolint:ll
type Gossip struct {
	PinnedSyncersRaw []string `long:"pinned-syncers" description:"A set of peers that should always remain in an active sync state, which can be used to closely synchronize the routing tables of two nodes. The value should be a hex-encoded pubkey, the flag can be specified multiple times to add multiple peers. Connected peers matching this pubkey will remain active for the duration of the connection and not count towards the NumActiveSyncer count."`

	PinnedSyncers discovery.PinnedSyncers

	MaxChannelUpdateBurst int `long:"max-channel-update-burst" description:"The maximum number of updates for a specific channel and direction that lnd will accept over the channel update interval."`

	ChannelUpdateInterval time.Duration `long:"channel-update-interval" description:"The interval used to determine how often lnd should allow a burst of new updates for a specific channel and direction."`

	SubBatchDelay time.Duration `long:"sub-batch-delay" description:"The duration to wait before sending the next announcement batch if there are multiple. Use a small value if there are a lot announcements and they need to be broadcast quickly."`

	AnnouncementConf uint32 `long:"announcement-conf" description:"The number of confirmations required before processing channel announcements."`

	MsgRateBytes uint64 `long:"msg-rate-bytes" description:"The maximum number of bytes of gossip messages that will be sent per second. This is a global limit that applies to all peers."`

	MsgBurstBytes uint64 `long:"msg-burst-bytes" description:"The maximum number of bytes of gossip messages that will be sent in a burst. This is a global limit that applies to all peers. This value should be set to something greater than 130 KB"`
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

// Validate checks the Gossip configuration to ensure that the input values are
// sane.
func (g *Gossip) Validate() error {
	if g.AnnouncementConf < minAnnouncementConf {
		return fmt.Errorf("announcement-conf=%v must be no less than "+
			"%v", g.AnnouncementConf, minAnnouncementConf)
	}

	if g.MsgBurstBytes < lnwire.MaxSliceLength {
		return fmt.Errorf("msg-burst-bytes=%v must be at least %v",
			g.MsgBurstBytes, lnwire.MaxSliceLength)
	}

	return nil
}

// Compile-time constraint to ensure Gossip implements the Validator interface.
var _ Validator = (*Gossip)(nil)
