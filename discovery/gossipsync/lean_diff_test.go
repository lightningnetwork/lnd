package gossipsync

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// leanBinEnv names the environment variable holding the path of the Lean
// model's executable, built by lean/check.sh. The differential tests skip
// when it is unset, so a plain go test needs no Lean toolchain.
const leanBinEnv = "GOSSIPSYNC_LEAN_BIN"

// leanModel is a running instance of the Lean model's executable, which
// answers one case at a time over its standard input and output.
type leanModel struct {
	in  io.WriteCloser
	out *bufio.Scanner
}

// startLean starts the Lean executable, or skips the test if it isn't
// configured. The process lives until the test ends.
func startLean(t *testing.T) *leanModel {
	t.Helper()

	bin := os.Getenv(leanBinEnv)
	if bin == "" {
		t.Skipf("%s not set; run lean/check.sh", leanBinEnv)
	}

	cmd := exec.Command(bin)
	in, err := cmd.StdinPipe()
	require.NoError(t, err)
	out, err := cmd.StdoutPipe()
	require.NoError(t, err)
	cmd.Stderr = os.Stderr
	require.NoError(t, cmd.Start())

	t.Cleanup(func() {
		_ = in.Close()
		_ = cmd.Wait()
	})

	scanner := bufio.NewScanner(out)
	scanner.Buffer(make([]byte, 1<<20), 1<<24)

	return &leanModel{in: in, out: scanner}
}

// ask sends one case to the model and returns its answer, without the
// closing end line.
func (m *leanModel) ask(t require.TestingT, input string) []string {
	_, err := io.WriteString(m.in, input)
	require.NoError(t, err)

	var lines []string
	for m.out.Scan() {
		line := m.out.Text()
		if line == "end" {
			return lines
		}
		lines = append(lines, line)
	}
	require.NoError(t, m.out.Err())
	require.Fail(t, "lean model exited mid-case")

	return nil
}

// chainHash maps a model chain number to a chain hash.
func chainHash(c uint8) chainhash.Hash { return chainhash.Hash{c} }

// formatEntries renders SCIDs and timestamp pairs in the model's format.
func formatEntries(scids []lnwire.ShortChannelID, ts lnwire.Timestamps) string {
	var b strings.Builder
	for i, scid := range scids {
		var t1, t2 uint32
		if len(ts) != 0 {
			t1, t2 = ts[i].Timestamp1, ts[i].Timestamp2
		}
		fmt.Fprintf(&b, " %d %d %d", scid.ToUint64(), t1, t2)
	}

	return b.String()
}

// formatReply renders a reply in the model's input format.
func formatReply(r *lnwire.ReplyChannelRange, withTs bool) string {
	ts := 0
	if withTs {
		ts = 1
	}

	return fmt.Sprintf("reply %d %d %d %d %d %d %d%s", r.ChainHash[0],
		r.FirstBlockHeight, r.NumBlocks, r.Complete, r.EncodingType, ts,
		len(r.ShortChanIDs),
		formatEntries(r.ShortChanIDs, r.Timestamps))
}

// errKind names a rejection the way the model does. The five invalid
// reply messages are told apart by their text.
func errKind(err error) string {
	msg := err.Error()
	switch {
	case errors.Is(err, ErrRangeReplyTooLarge):
		return "tooLarge"
	case strings.Contains(msg, "prior to query"):
		return "beforeQuery"
	case strings.Contains(msg, "after query"):
		return "afterQuery"
	case strings.Contains(msg, "not at query start"):
		return "notAtStart"
	case strings.Contains(msg, "does not continue"):
		return "gap"
	case strings.Contains(msg, "unhandled encoding"):
		return "badEncoding"
	}

	return "unknown: " + msg
}

// freshness reads back a buffered timestamp as a number.
func freshness(f any) uint64 {
	if u, ok := f.(lnwire.UnixTimestamp); ok {
		return uint64(u)
	}

	return math.MaxUint64
}

// runGoAccumulator feeds replies to the Go accumulator the way onReply does,
// stopping at the first error or completion, and renders each step and the
// final buffer in the model's output format.
func runGoAccumulator(query *lnwire.QueryChannelRange,
	replies []*lnwire.ReplyChannelRange, limits RangeLimits,
	now time.Time) []string {

	var (
		acc   = newRangeAccumulator(query)
		lines []string
	)
	for _, reply := range replies {
		next, done, err := acc.add(reply, limits, now)
		if err != nil {
			lines = append(lines, "err "+errKind(err))
			break
		}

		acc = next
		d := 0
		if done {
			d = 1
		}
		lines = append(lines, fmt.Sprintf("ok %d %d %d %d", d,
			acc.replyBudgetUsed, acc.scids,
			acc.prev.LastBlockHeight()))

		if done {
			break
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "chans %d", len(acc.chans))
	for _, c := range acc.chans {
		fmt.Fprintf(&b, " %d %d %d", c.ShortChannelID.ToUint64(),
			freshness(c.Node1Freshness),
			freshness(c.Node2Freshness))
	}

	return append(lines, b.String())
}

// clampU32 clamps a signed height to the uint32 range.
func clampU32(v int64) uint32 {
	switch {
	case v < 0:
		return 0
	case v > math.MaxUint32:
		return math.MaxUint32
	}

	return uint32(v)
}

// genHeight draws a height near zero or near the top of the uint32 range,
// where LastBlockHeight saturates.
func genHeight(t *rapid.T, label string) uint32 {
	if rapid.Bool().Draw(t, label+"High") {
		return math.MaxUint32 - rapid.Uint32Range(0, 40).Draw(t, label)
	}

	return rapid.Uint32Range(0, 40).Draw(t, label)
}

// genQuery draws a query, including zero and saturating block counts.
func genQuery(t *rapid.T) *lnwire.QueryChannelRange {
	num := rapid.OneOf(
		rapid.Uint32Range(0, 60),
		rapid.Uint32Range(math.MaxUint32-3, math.MaxUint32),
	).Draw(t, "queryNum")

	return &lnwire.QueryChannelRange{
		ChainHash:        chainHash(1),
		FirstBlockHeight: genHeight(t, "queryFirst"),
		NumBlocks:        num,
	}
}

// genTimestamp draws a timestamp near the freshness horizon on either side
// of now, or anywhere in the uint32 range.
func genTimestamp(t *rapid.T, now int64, horizon int64, label string) uint32 {
	if rapid.IntRange(0, 4).Draw(t, label+"Far") == 0 {
		return rapid.Uint32().Draw(t, label)
	}

	off := rapid.Int64Range(-2*horizon-2, 2*horizon+2).Draw(t, label)

	return clampU32(now + off)
}

// genEntries draws the SCIDs of one reply, with timestamps if withTs.
func genEntries(t *rapid.T, withTs bool, now, horizon int64) (
	[]lnwire.ShortChannelID, lnwire.Timestamps) {

	n := rapid.IntRange(0, 4).Draw(t, "numSCIDs")
	scids := make([]lnwire.ShortChannelID, n)
	var ts lnwire.Timestamps
	if withTs && n > 0 {
		ts = make(lnwire.Timestamps, n)
	}
	for i := range scids {
		scids[i] = lnwire.NewShortChanIDFromInt(
			rapid.Uint64Range(0, 1<<45).Draw(t, "scid"),
		)
		if ts != nil {
			ts[i] = lnwire.ChanUpdateTimestamps{
				Timestamp1: genTimestamp(t, now, horizon, "t1"),
				Timestamp2: genTimestamp(t, now, horizon, "t2"),
			}
		}
	}

	return scids, ts
}

// genAdversarialReply draws a reply relative to the query and to the last
// block of the previous reply: a legacy echo, a plausible continuation, an
// off-by-one, or noise.
func genAdversarialReply(t *rapid.T, q *lnwire.QueryChannelRange,
	prevLast int64, now, horizon int64) *lnwire.ReplyChannelRange {

	reply := &lnwire.ReplyChannelRange{
		ChainHash: q.ChainHash,
		Complete: uint8(rapid.SampledFrom(
			[]int{0, 0, 1, 2},
		).Draw(t, "complete")),
		EncodingType: lnwire.QueryEncoding(rapid.SampledFrom(
			[]int{0, 0, 0, 1, 1, 2},
		).Draw(t, "encoding")),
	}
	if rapid.IntRange(0, 9).Draw(t, "otherChain") == 0 {
		reply.ChainHash = chainHash(2)
	}

	qFirst := int64(q.FirstBlockHeight)
	qLast := int64(q.LastBlockHeight())
	switch rapid.IntRange(0, 9).Draw(t, "shape") {
	case 0:
		reply.FirstBlockHeight = q.FirstBlockHeight
		reply.NumBlocks = q.NumBlocks

	case 7, 8:
		reply.FirstBlockHeight = clampU32(qFirst + rapid.Int64Range(
			-3, 70).Draw(t, "noiseFirst"))
		reply.NumBlocks = rapid.Uint32Range(0, 70).Draw(t, "noiseNum")

	case 9:
		reply.FirstBlockHeight = rapid.Uint32().Draw(t, "anyFirst")
		reply.NumBlocks = rapid.Uint32().Draw(t, "anyNum")

	default:
		base := prevLast
		if base < 0 {
			base = qFirst
		}
		first := base + rapid.Int64Range(-1, 2).Draw(t, "delta")
		if prevLast >= 0 && rapid.Bool().Draw(t, "continue") {
			first = prevLast + 1
		}
		reply.FirstBlockHeight = clampU32(first)

		var last int64
		switch rapid.IntRange(0, 7).Draw(t, "end") {
		case 0:
			last = qLast
		case 1:
			last = qLast + 1
		default:
			last = int64(reply.FirstBlockHeight) +
				rapid.Int64Range(-1, 8).Draw(t, "span")
		}
		num := last - int64(reply.FirstBlockHeight) + 1
		if rapid.IntRange(0, 5).Draw(t, "zeroNum") == 0 {
			num = 0
		}
		reply.NumBlocks = clampU32(num)
	}

	withTs := rapid.Bool().Draw(t, "withTs")
	reply.ShortChanIDs, reply.Timestamps = genEntries(
		t, withTs, now, horizon,
	)

	return reply
}

// genBlocks draws sorted, distinct block heights inside the query, each
// with sorted SCIDs, as the graph returns them.
func genBlocks(t *rapid.T, q *lnwire.QueryChannelRange, now,
	horizon int64) []graphdb.BlockChannelRange {

	first, last := q.FirstBlockHeight, q.LastBlockHeight()
	span := uint64(last) - uint64(first)
	if span > 120 {
		span = 120
	}

	count := min(rapid.IntRange(0, 20).Draw(t, "numBlocks"), int(span)+1)
	offsets := rapid.SliceOfNDistinct(
		rapid.Uint64Range(0, span), count, count,
		func(o uint64) uint64 { return o },
	).Draw(t, "offsets")
	slices.Sort(offsets)

	// Sometimes put a block on the query's last height, which is far
	// from the others for a saturating query.
	heights := make([]uint32, 0, len(offsets)+1)
	for _, o := range offsets {
		heights = append(heights, first+uint32(o))
	}
	if rapid.IntRange(0, 3).Draw(t, "lastBlock") == 0 &&
		(len(heights) == 0 || heights[len(heights)-1] < last) {

		heights = append(heights, last)
	}

	blocks := make([]graphdb.BlockChannelRange, 0, len(heights))
	for _, h := range heights {
		n := rapid.IntRange(0, 6).Draw(t, "perBlock")
		chans := make([]graphdb.ChannelUpdateInfo, n)
		for i := range chans {
			scid := lnwire.ShortChannelID{
				BlockHeight: h & 0xFFFFFF,
				TxIndex:     uint32(i),
			}
			t1 := genTimestamp(t, now, horizon, "bt1")
			t2 := genTimestamp(t, now, horizon, "bt2")
			chans[i] = graphdb.NewV1ChannelUpdateInfo(
				scid, time.Unix(int64(t1), 0),
				time.Unix(int64(t2), 0),
			)
		}
		blocks = append(blocks, graphdb.BlockChannelRange{
			Height:   h,
			Channels: chans,
		})
	}

	return blocks
}

// noShuffle leaves a block's channels in order, so that fitBlock keeps the
// first ones, as the model's take does.
func noShuffle(int, func(i, j int)) {}

// chunkerCase renders a chunker case in the model's input format.
func chunkerCase(c *rangeChunker, blocks []graphdb.BlockChannelRange) string {
	ts := 0
	if c.withTimestamps {
		ts = 1
	}

	var b strings.Builder
	fmt.Fprintf(&b, "chunk %d %d %d %d %d %d %d\n", c.query.ChainHash[0],
		c.query.FirstBlockHeight, c.query.NumBlocks, c.encoding, ts,
		c.chunkSize, len(blocks))
	for _, block := range blocks {
		fmt.Fprintf(&b, "block %d %d", block.Height,
			len(block.Channels))
		for _, info := range block.Channels {
			fmt.Fprintf(&b, " %d %d %d",
				info.ShortChannelID.ToUint64(),
				info.Node1FreshnessTime().Unix(),
				info.Node2FreshnessTime().Unix())
		}
		b.WriteString("\n")
	}

	return b.String()
}

// genHonestStream draws a stream from our own chunker, then mutates at most
// one reply: a shifted first block, a longer range, an unknown encoding, or a
// duplicated or dropped reply.
func genHonestStream(t *rapid.T, query *lnwire.QueryChannelRange, now,
	horizon int64) []*lnwire.ReplyChannelRange {

	chunker := &rangeChunker{
		query: query,
		encoding: lnwire.QueryEncoding(
			rapid.IntRange(0, 1).Draw(t, "encoding"),
		),
		chunkSize: rapid.OneOf(
			rapid.IntRange(0, 3), rapid.IntRange(4, 8),
		).Draw(t, "chunkSize"),
		withTimestamps: rapid.Bool().Draw(t, "withTs"),
		shuffle:        noShuffle,
	}

	var replies []*lnwire.ReplyChannelRange
	for r := range chunker.replies(genBlocks(t, query, now, horizon)) {
		replies = append(replies, r)
	}

	i := rapid.IntRange(0, len(replies)-1).Draw(t, "victim")
	switch rapid.IntRange(0, 5).Draw(t, "mutation") {
	case 1:
		replies[i].FirstBlockHeight++
	case 2:
		replies[i].NumBlocks++
	case 3:
		replies[i].EncodingType = 2
	case 4:
		replies = slices.Insert(replies, i, replies[i])
	case 5:
		replies = slices.Delete(replies, i, i+1)
	}

	return replies
}

// genAdversarialStream draws a stream of one to ten adversarial replies,
// each placed relative to the one before it.
func genAdversarialStream(t *rapid.T, query *lnwire.QueryChannelRange, now,
	horizon int64) []*lnwire.ReplyChannelRange {

	var (
		n        = rapid.IntRange(1, 10).Draw(t, "numReplies")
		prevLast = int64(-1)
		replies  []*lnwire.ReplyChannelRange
	)
	for range n {
		r := genAdversarialReply(t, query, prevLast, now, horizon)
		replies = append(replies, r)
		prevLast = int64(r.LastBlockHeight())
	}

	return replies
}

// TestLeanDiffAccumulator asserts that the Go accumulator and the Lean model
// agree, step by step, on random reply streams: the error kind or the done
// flag, budget, SCID count and position after each reply, and the final
// buffer. Half the streams are honest chunker output with at most one
// mutation, and half are adversarial.
func TestLeanDiffAccumulator(t *testing.T) {
	lean := startLean(t)

	rapid.Check(t, func(rt *rapid.T) {
		query := genQuery(rt)
		horizon := rapid.Int64Range(0, 200).Draw(rt, "horizon")
		now := rapid.OneOf(
			rapid.Int64Range(0, 3000),
			rapid.Int64Range(
				math.MaxUint32-3000, math.MaxUint32+3000,
			),
		).Draw(rt, "now")
		limits := RangeLimits{
			MaxReplies: rapid.OneOf(
				rapid.Uint32Range(0, 4),
				rapid.Uint32Range(5, 60),
			).Draw(rt, "maxReplies"),
			MaxSCIDs: rapid.OneOf(
				rapid.Uint32Range(0, 10),
				rapid.Uint32Range(11, 200),
			).Draw(rt, "maxSCIDs"),
			FreshnessHorizon: time.Duration(horizon) * time.Second,
		}

		var replies []*lnwire.ReplyChannelRange
		if rapid.Bool().Draw(rt, "honest") {
			replies = genHonestStream(rt, query, now, horizon)
		} else {
			replies = genAdversarialStream(rt, query, now, horizon)
		}

		var in strings.Builder
		fmt.Fprintf(&in, "acc %d %d %d %d %d %d %d %d\n",
			query.ChainHash[0], query.FirstBlockHeight,
			query.NumBlocks, limits.MaxReplies, limits.MaxSCIDs,
			horizon, now, len(replies))
		for _, r := range replies {
			in.WriteString(formatReply(r, len(r.Timestamps) != 0))
			in.WriteString("\n")
		}

		want := runGoAccumulator(
			query, replies, limits, time.Unix(now, 0),
		)
		got := lean.ask(rt, in.String())
		require.Equal(rt, want, got, "input:\n%s", in.String())
	})
}

// TestLeanDiffChunker asserts that the Go chunker and the Lean model produce
// the same replies for random graphs, chunk sizes, encodings and timestamp
// settings. The Go chunker gets a shuffle that swaps nothing, so an
// oversized block keeps its first channels, as the model's take does.
func TestLeanDiffChunker(t *testing.T) {
	lean := startLean(t)

	rapid.Check(t, func(rt *rapid.T) {
		query := genQuery(rt)
		chunker := &rangeChunker{
			query: query,
			encoding: lnwire.QueryEncoding(
				rapid.IntRange(0, 1).Draw(rt, "encoding"),
			),
			chunkSize: rapid.IntRange(0, 8).Draw(
				rt, "chunkSize",
			),
			withTimestamps: rapid.Bool().Draw(rt, "withTs"),
			shuffle:        noShuffle,
		}
		blocks := genBlocks(rt, query, 1_000_000, 1000)

		var want []string
		for r := range chunker.replies(blocks) {
			want = append(
				want, formatReply(r, chunker.withTimestamps),
			)
		}

		in := chunkerCase(chunker, blocks)
		got := lean.ask(rt, in)
		require.Equal(rt, want, got, "input:\n%s", in)
	})
}
