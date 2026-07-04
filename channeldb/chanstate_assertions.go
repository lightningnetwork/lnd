package channeldb

import (
	"github.com/lightningnetwork/lnd/chanlifecycle"
	"github.com/lightningnetwork/lnd/chanstate"
)

// Compile-time assertion that ChannelStateDB satisfies the channel-state store
// contract while channeldb keeps compatibility wrappers for callers that still
// depend on the old package.
var _ chanstate.Store = (*ChannelStateDB)(nil)

// Compile-time assertions that ChannelStateDB keeps compatibility wrappers for
// callers that still depend on the old package.
var (
	_ chanlifecycle.ChannelLifecycle   = (*ChannelStateDB)(nil)
	_ chanlifecycle.LinkNodeMaintainer = (*ChannelStateDB)(nil)
)
