package channeldb

import "github.com/lightningnetwork/lnd/chanstate"

// Compile-time assertion that ChannelStateDB satisfies the channel-state store
// contract while the KV implementation still lives in channeldb.
var _ chanstate.Store = (*ChannelStateDB)(nil)
