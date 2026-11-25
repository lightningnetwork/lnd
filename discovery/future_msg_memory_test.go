package discovery

import (
	"bytes"
	"testing"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// denseChanAnn decodes a channel_announcement that fills a whole message
// with features, all bits set. Nothing about it needs to be valid: premature
// announcements are cached before any other check.
func denseChanAnn(t *testing.T) *lnwire.ChannelAnnouncement1 {
	t.Helper()

	// Four signatures, chain hash, short_channel_id and four keys take
	// 428 bytes, plus 2 for the features length.
	featLen := lnwire.MaxMsgBody - 430

	var body bytes.Buffer
	body.Write(make([]byte, 256))
	body.Write([]byte{byte(featLen >> 8), byte(featLen)})
	body.Write(bytes.Repeat([]byte{0xff}, featLen))
	body.Write(make([]byte, 32+8))
	key := append([]byte{0x02}, bytes.Repeat([]byte{0x11}, 32)...)
	for range 4 {
		body.Write(key)
	}

	ann := &lnwire.ChannelAnnouncement1{}
	require.NoError(t, ann.Decode(bytes.NewReader(body.Bytes()), 0))

	return ann
}

// TestFutureMsgCacheRetainedMemory checks that the premature-message cache
// bounds the memory it retains, not just the number of messages. A
// channel_announcement's features are decoded into a map with one entry per
// set bit, so one 65 KB message holds 65,536 entries (about 0.7 MB).
func TestFutureMsgCacheRetainedMemory(t *testing.T) {
	t.Parallel()

	ann := denseChanAnn(t)
	require.Equal(t, 65536, ann.Features.NumFeatures())

	// One peer sends maxFutureMessages of them, each with a far-future
	// short_channel_id.
	cache := newFutureMsgCache(maxFutureMsgCacheSize)
	for i := range maxFutureMessages {
		_, _ = cache.Put(cache.nextMsgID(), &cachedFutureMsg{
			msg:    &networkMsg{msg: ann, isRemote: true},
			height: uint32(1_000_000 + i),
			size:   futureMsgSize(ann),
		})
	}

	var retained int
	cache.Range(func(_ uint64, m *cachedFutureMsg) bool {
		a, ok := m.msg.msg.(*lnwire.ChannelAnnouncement1)
		if !ok {
			t.Fatal("unexpected cached message type")
		}
		retained += a.Features.NumFeatures()

		return true
	})

	// Each entry is charged the wire body plus 16 per feature. With this
	// budget only 58 dense announcements fit, bounding the retained feature
	// bits to a small fraction of the old 1,000-entry limit.
	require.LessOrEqual(t, cache.Len(), 58)
	require.LessOrEqual(t, retained, 58*65536,
		"cache retains %d decoded feature bits", retained)
}

// TestFutureMsgCacheWeightedEviction checks that weighted cache admission and
// eviction preserve the ordinary-message bound and reject oversized entries.
func TestFutureMsgCacheWeightedEviction(t *testing.T) {
	t.Parallel()

	ann := denseChanAnn(t)
	entrySize := futureMsgSize(ann)
	require.Equal(t,
		uint64(futureMsgMinSize+(1<<16)*futureFeatureEntrySize),
		entrySize,
	)

	// A cache sized for two dense announcements retains exactly two and
	// evicts the oldest on the third insert.
	cache := newFutureMsgCache(2 * entrySize)
	for range 3 {
		_, err := cache.Put(cache.nextMsgID(), &cachedFutureMsg{
			msg:  &networkMsg{msg: ann, isRemote: true},
			size: entrySize,
		})
		require.NoError(t, err)
	}
	require.Equal(t, 2, cache.Len())
	require.Equal(t, 2*entrySize, cache.Size())

	// An entry larger than the entire capacity is refused.
	small := newFutureMsgCache(1)
	_, err := small.Put(1, &cachedFutureMsg{size: entrySize})
	require.Error(t, err)
	require.Zero(t, small.Len())

	// Ordinary (empty) messages still fit up to the count bound when each
	// is charged the minimum size.
	ordinary := newFutureMsgCache(maxFutureMsgCacheSize)
	for range maxFutureMessages {
		_, err := ordinary.Put(ordinary.nextMsgID(), &cachedFutureMsg{
			size: futureMsgMinSize,
		})
		require.NoError(t, err)
	}
	require.Equal(t, maxFutureMessages, ordinary.Len())
}
