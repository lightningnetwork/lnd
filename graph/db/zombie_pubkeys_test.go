package graphdb

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// TestMakeZombiePubkeys checks that the zombie index keys for a channel only
// ever hold each node's own key in its own slot, that at least one node can
// resurrect the channel, and that it's the stale side.
func TestMakeZombiePubkeys(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(t *rapid.T) {
		var node1, node2 [33]byte
		copy(node1[:], rapid.SliceOfN(rapid.Byte(), 33, 33).Draw(
			t, "node1",
		))
		copy(node2[:], rapid.SliceOfN(rapid.Byte(), 33, 33).Draw(
			t, "node2",
		))

		drawTime := func(label string) *time.Time {
			if !rapid.Bool().Draw(t, label+"_present") {
				return nil
			}
			ts := time.Unix(
				rapid.Int64Range(0, 1<<32).Draw(t, label), 0,
			)

			return &ts
		}
		e1, e2 := drawTime("e1"), drawTime("e2")

		key1, key2 := makeZombiePubkeys(node1, node2, e1, e2)

		var blank [33]byte
		require.Contains(t, [][33]byte{node1, blank}, key1)
		require.Contains(t, [][33]byte{node2, blank}, key2)
		require.False(t, key1 == blank && key2 == blank)

		switch {
		case e1 == nil && e2 == nil:
			require.Equal(t, node1, key1)
			require.Equal(t, node2, key2)

		case e1 == nil || (e2 != nil && e1.Before(*e2)):
			require.Equal(t, node1, key1)
			require.Equal(t, blank, key2)

		default:
			require.Equal(t, blank, key1)
			require.Equal(t, node2, key2)
		}
	})
}
