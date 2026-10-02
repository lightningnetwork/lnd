package gossipsync

import (
	"fmt"
	"sync"

	"github.com/lightningnetwork/lnd/lnwire"
)

// byzMode is how a byzantine peer misbehaves.
type byzMode uint8

const (
	// byzSilent never answers a query.
	byzSilent byzMode = iota

	// byzOversize answers a range query with more SCIDs than we accept.
	byzOversize

	// byzBadStart answers a range query with a stream that doesn't start
	// at the query's first block.
	byzBadStart

	// byzLiar answers with a reply stream that spends our whole reply
	// budget on made up channels, which we can't tell from real ones.
	byzLiar

	// byzFlood behaves like an honest peer with an empty graph, but also
	// floods us with unsolicited replies, queries and filters.
	byzFlood

	numByzModes
)

// String names the mode.
func (m byzMode) String() string {
	return [...]string{
		"silent", "oversize", "badStart", "liar", "flood",
	}[m]
}

// detectable reports whether the mode is a fault the syncer can recognize,
// as opposed to a lie it has to accept.
func (m byzMode) detectable() bool {
	return m != byzLiar
}

// byzantine is a peer with no manager that answers our queries however its
// mode says.
type byzantine struct {
	mode   byzMode
	limits RangeLimits

	mu  sync.Mutex
	out *link
}

// send queues messages to the node under test.
func (b *byzantine) send(msgs ...lnwire.Message) {
	b.mu.Lock()
	out := b.out
	b.mu.Unlock()

	if out != nil {
		_ = out.SendMessageLazy(false, msgs...)
	}
}

// fakeSCIDs returns n made up SCIDs in block h.
func fakeSCIDs(h uint32, n int) []lnwire.ShortChannelID {
	scids := make([]lnwire.ShortChannelID, n)
	for i := range scids {
		scids[i] = lnwire.ShortChannelID{
			BlockHeight: h, TxIndex: uint32(1_000_000 + i),
		}
	}

	return scids
}

// receive handles a message from the node under test.
func (b *byzantine) receive(msg lnwire.Message) {
	switch q := msg.(type) {
	case *lnwire.QueryChannelRange:
		b.answerRange(q)

	case *lnwire.QueryShortChanIDs:
		if b.mode == byzSilent {
			return
		}
		b.send(&lnwire.ReplyShortChanIDsEnd{
			ChainHash: q.ChainHash, Complete: 1,
		})
	}
}

// answerRange answers a query_channel_range according to the mode.
func (b *byzantine) answerRange(q *lnwire.QueryChannelRange) {
	reply := func(first, last uint32,
		scids []lnwire.ShortChannelID) *lnwire.ReplyChannelRange {

		return &lnwire.ReplyChannelRange{
			ChainHash:        q.ChainHash,
			FirstBlockHeight: first,
			NumBlocks:        last - first + 1,
			EncodingType:     lnwire.EncodingSortedPlain,
			ShortChanIDs:     scids,
		}
	}
	first, last := q.FirstBlockHeight, q.LastBlockHeight()

	switch b.mode {
	case byzSilent:

	// Spread one more SCID than the limit over replies that otherwise
	// tile the query correctly.
	case byzOversize:
		const perReply = 1_000
		n := int(b.limits.MaxSCIDs)/perReply + 1
		for i := range n {
			start := first + uint32(i)
			end := start
			if i == n-1 {
				end = last
			}
			b.send(reply(start, end, fakeSCIDs(start, perReply)))
		}

	case byzBadStart:
		b.send(reply(first+1, last, fakeSCIDs(first+1, 3)))

	// One fake channel per block, one block per reply, until the reply
	// budget runs out, which we treat as the end of the stream.
	case byzLiar:
		for i := range b.limits.MaxReplies + 2 {
			h := first + i
			b.send(reply(h, h, fakeSCIDs(h, 1)))
		}

	case byzFlood:
		final := reply(first, last, nil)
		final.Complete = 1
		b.send(final)
	}
}

// flood sends the node under test a burst of unsolicited messages.
func (b *byzantine) flood(n int) {
	for i := range n {
		b.send(
			&lnwire.ReplyChannelRange{
				ChainHash:        simChain,
				FirstBlockHeight: uint32(i),
				NumBlocks:        1,
				EncodingType:     lnwire.EncodingSortedPlain,
			},
			&lnwire.QueryChannelRange{
				ChainHash: simChain, NumBlocks: simBestHeight,
			},
			&lnwire.GossipTimestampRange{
				ChainHash:      simChain,
				TimestampRange: ^uint32(0),
			},
		)
	}
}

// String describes the peer.
func (b *byzantine) String() string {
	return fmt.Sprintf("byzantine(%v)", b.mode)
}
