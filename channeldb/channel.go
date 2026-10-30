package channeldb

import "github.com/lightningnetwork/lnd/chanstate"

var (
	closedChannelBucket     = chanstate.ClosedChannelBucketKey()
	openChannelBucket       = chanstate.OpenChannelBucketKey()
	outpointBucket          = chanstate.OutpointBucketKey()
	chanIDBucket            = chanstate.ChanIDBucketKey()
	historicalChannelBucket = chanstate.HistoricalChannelBucketKey()
)
