package discovery

import (
	"errors"
	"math"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/neutrino/cache"
	"github.com/lightninglabs/neutrino/cache/lru"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
)

const (
	// DefaultBanThreshold is the default value to be used for banThreshold.
	DefaultBanThreshold = 100

	// maxBannedPeers limits the maximum number of banned pubkeys that
	// we'll store.
	// TODO(eugene): tune.
	maxBannedPeers = 10_000

	// banTime is the amount of time that the non-channel peer will be
	// banned for. Channel announcements from channel peers will be dropped
	// if it's not one of our channels.
	// TODO(eugene): tune.
	banTime = time.Hour * 48

	// resetDelta is the time after a peer's last ban update that we'll
	// reset its ban score.
	// TODO(eugene): tune.
	resetDelta = time.Hour * 48

	// purgeInterval is how often we'll remove entries from the
	// peerBanIndex and allow peers to be un-banned. This interval is also
	// used to reset ban scores of peers that aren't banned.
	purgeInterval = time.Minute * 10
)

var ErrPeerBanned = errors.New("peer has bypassed ban threshold - banning")

// ClosedChannelTracker handles closed channels being gossiped to us.
type ClosedChannelTracker interface {
	// GraphCloser is used to mark channels as closed and to check whether
	// certain channels are closed.
	GraphCloser

	// IsChannelPeer checks whether we have a channel with a peer.
	IsChannelPeer(*btcec.PublicKey) (bool, error)
}

// GraphCloser handles tracking closed channels by their scid.
type GraphCloser interface {
	// PutClosedScid marks a channel as closed so that we won't validate
	// channel announcements for it again.
	PutClosedScid(lnwire.ShortChannelID) error

	// IsClosedScid checks if a short channel id is closed.
	IsClosedScid(lnwire.ShortChannelID) (bool, error)
}

// NodeInfoInquirier handles queries relating to specific nodes and channels
// they may have with us.
type NodeInfoInquirer interface {
	// FetchOpenChannels returns the set of channels that we have with the
	// peer identified by the passed-in public key.
	FetchOpenChannels(*btcec.PublicKey) ([]*channeldb.OpenChannel, error)
}

// ScidCloserMan helps the gossiper handle closed channels that are in the
// ChannelGraph.
type ScidCloserMan struct {
	graph     GraphCloser
	channelDB NodeInfoInquirer
}

// NewScidCloserMan creates a new ScidCloserMan.
func NewScidCloserMan(graph GraphCloser,
	channelDB NodeInfoInquirer) *ScidCloserMan {

	return &ScidCloserMan{
		graph:     graph,
		channelDB: channelDB,
	}
}

// PutClosedScid marks scid as closed so the gossiper can ignore this channel
// in the future.
func (s *ScidCloserMan) PutClosedScid(scid lnwire.ShortChannelID) error {
	return s.graph.PutClosedScid(scid)
}

// IsClosedScid checks whether scid is closed so that the gossiper can ignore
// it.
func (s *ScidCloserMan) IsClosedScid(scid lnwire.ShortChannelID) (bool,
	error) {

	return s.graph.IsClosedScid(scid)
}

// IsChannelPeer checks whether we have a channel with the peer.
func (s *ScidCloserMan) IsChannelPeer(peerKey *btcec.PublicKey) (bool, error) {
	chans, err := s.channelDB.FetchOpenChannels(peerKey)
	if err != nil {
		return false, err
	}

	return len(chans) > 0, nil
}

// A compile-time constraint to ensure ScidCloserMan implements
// ClosedChannelTracker.
var _ ClosedChannelTracker = (*ScidCloserMan)(nil)

// cachedBanInfo is used to track a peer's ban score and if it is banned.
type cachedBanInfo struct {
	score      uint64
	lastUpdate time.Time
}

// Size returns the "size" of an entry.
func (c *cachedBanInfo) Size() (uint64, error) {
	return 1, nil
}

// isBanned returns true if the ban score is greater than the ban threshold.
func (c *cachedBanInfo) isBanned(banThreshold uint64) bool {
	return c.score >= banThreshold
}

// banman is responsible for banning peers that are misbehaving. The banman is
// in-memory and will be reset upon restart of LND. If a node's pubkey is in
// the peerBanIndex, it has a ban score. Ban scores start at 1 and are
// incremented by 1 for each instance of misbehavior. It uses an LRU cache to
// cut down on memory usage in case there are many banned peers and to protect
// against DoS.
type banman struct {
	// peerBanIndex tracks our peers' ban scores and if they are banned and
	// for how long. The ban score is incremented when our peer gives us
	// gossip messages that are invalid.
	peerBanIndex *lru.Cache[[33]byte, *cachedBanInfo]

	wg   sync.WaitGroup
	quit chan struct{}

	// banThreshold is the point at which non-channel peers will be banned.
	banThreshold uint64
}

// newBanman creates a new banman with the default maxBannedPeers.
func newBanman(banThreshold uint64) *banman {
	// If the ban threshold is set to 0, we'll use the max value to
	// effectively disable banning.
	if banThreshold == 0 {
		log.Warn("Banning is disabled due to zero banThreshold")
		banThreshold = math.MaxUint64
	}

	return &banman{
		peerBanIndex: lru.NewCache[[33]byte, *cachedBanInfo](
			maxBannedPeers,
		),
		quit:         make(chan struct{}),
		banThreshold: banThreshold,
	}
}

// start kicks off the banman by calling purgeExpiredBans.
func (b *banman) start() {
	b.wg.Add(1)
	go b.purgeExpiredBans()
}

// stop halts the banman.
func (b *banman) stop() {
	close(b.quit)
	b.wg.Wait()
}

// purgeOldEntries removes ban entries if their ban has expired.
func (b *banman) purgeExpiredBans() {
	defer b.wg.Done()

	purgeTicker := time.NewTicker(purgeInterval)
	defer purgeTicker.Stop()

	for {
		select {
		case <-purgeTicker.C:
			b.purgeBanEntries()

		case <-b.quit:
			return
		}
	}
}

// purgeBanEntries does two things:
// - removes peers from our ban list whose ban timer is up
// - removes peers whose ban scores have expired.
func (b *banman) purgeBanEntries() {
	keysToRemove := make([][33]byte, 0)

	sweepEntries := func(pubkey [33]byte, banInfo *cachedBanInfo) bool {
		if banInfo.isBanned(b.banThreshold) {
			// If the peer is banned, check if the ban timer has
			// expired.
			if banInfo.lastUpdate.Add(banTime).Before(time.Now()) {
				keysToRemove = append(keysToRemove, pubkey)
			}

			return true
		}

		if banInfo.lastUpdate.Add(resetDelta).Before(time.Now()) {
			// Remove non-banned peers whose ban scores have
			// expired.
			keysToRemove = append(keysToRemove, pubkey)
		}

		return true
	}

	b.peerBanIndex.Range(sweepEntries)

	for _, key := range keysToRemove {
		b.peerBanIndex.Delete(key)
	}
}

// isBanned checks whether the peer identified by the pubkey is banned.
func (b *banman) isBanned(pubkey [33]byte) bool {
	banInfo, err := b.peerBanIndex.Get(pubkey)
	switch {
	case errors.Is(err, cache.ErrElementNotFound):
		return false

	default:
		return banInfo.isBanned(b.banThreshold)
	}
}

// incrementBanScore increments a peer's ban score.
func (b *banman) incrementBanScore(pubkey [33]byte) {
	banInfo, err := b.peerBanIndex.Get(pubkey)
	switch {
	case errors.Is(err, cache.ErrElementNotFound):
		cachedInfo := &cachedBanInfo{
			score:      1,
			lastUpdate: time.Now(),
		}
		_, _ = b.peerBanIndex.Put(pubkey, cachedInfo)
	default:
		cachedInfo := &cachedBanInfo{
			score:      banInfo.score + 1,
			lastUpdate: time.Now(),
		}

		_, _ = b.peerBanIndex.Put(pubkey, cachedInfo)
	}
}
