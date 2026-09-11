package lnd

import (
	"testing"

	"github.com/lightningnetwork/lnd/lnrpc"
)

func TestDescribeGraphCacheKey(t *testing.T) {
	t.Parallel()

	requests := []*lnrpc.ChannelGraphRequest{
		{},
		{IncludeUnannounced: true},
		{IncludeAuthProof: true},
		{IncludeUnannounced: true, IncludeAuthProof: true},
	}

	cache := make(map[describeGraphCacheKey]*lnrpc.ChannelGraph)
	for _, req := range requests {
		key := newDescribeGraphCacheKey(req)
		if _, ok := cache[key]; ok {
			t.Fatalf("cache key collision for request: %+v", req)
		}

		cache[key] = &lnrpc.ChannelGraph{}
	}

	if len(cache) != len(requests) {
		t.Fatalf("expected %d cache entries, got %d", len(requests), len(cache))
	}
}
