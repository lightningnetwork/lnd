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

	MsgRateBytes uint64 `long:"msg-rate-bytes" description:"The total rate of outbound gossip messages, expressed in bytes per second. This setting controls the long-term average speed of gossip traffic sent from your node. The rate limit is applied globally across all peers, not per-peer. If the rate of outgoing messages exceeds this value, lnd will start to queue and delay messages to stay within the limit."`

	MsgBurstBytes uint64 `long:"msg-burst-bytes" description:"The maximum burst of outbound gossip data, in bytes, that can be sent at once. This works in conjunction with gossip.msg-rate-bytes as part of a token bucket rate-limiting scheme. This value represents the size of the token bucket. It allows for short, high-speed bursts of traffic, with the long-term rate controlled by gossip.msg-rate-bytes. This value must be larger than the maximum lightning message size (~65KB) to allow sending large gossip messages."`

	FilterConcurrency int `long:"filter-concurrency" description:"The maximum number of concurrent gossip filter applications that can be processed. If not set, defaults to 5."`

	BanThreshold uint64 `long:"ban-threshold" description:"The score at which a peer is banned. A peer's ban score is incremented for each invalid gossip message. Invalid messages include those with bad signatures, stale timestamps, excessive updates, or invalid chain data. Once the score reaches this threshold, the peer is banned. Set to 0 to disable banning."`

	PeerMsgRateBytes uint64 `long:"peer-msg-rate-bytes" description:"The peer-specific rate of outbound gossip messages, expressed in bytes per second. This setting controls the long-term average speed of gossip traffic sent from your node. The rate limit is applied to each peer. If the rate of outgoing messages exceeds this value, lnd will start to queue and delay messages sending to that peer to stay within the limit."`
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

	if g.MsgBurstBytes <= g.MsgRateBytes {
		return fmt.Errorf("msg-burst-bytes=%v must be greater than "+
			"msg-rate-bytes=%v", g.MsgBurstBytes, g.MsgRateBytes)
	}

	if g.MsgRateBytes <= g.PeerMsgRateBytes {
		return fmt.Errorf("msg-rate-bytes=%v must be greater than "+
			"peer-msg-rate-bytes=%v", g.MsgRateBytes,
			g.PeerMsgRateBytes)
	}

	return nil
}

// Compile-time constraint to ensure Gossip implements the Validator interface.
var _ Validator = (*Gossip)(nil)
