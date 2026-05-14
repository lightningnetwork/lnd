package channeldb

import "github.com/lightningnetwork/lnd/chanstate"

// Compile-time assertions that ChannelStateDB satisfies the channel-state
// store contracts while the KV implementation still lives in channeldb.
var _ chanstate.Store[*OpenChannel] = (*ChannelStateDB)(nil)
