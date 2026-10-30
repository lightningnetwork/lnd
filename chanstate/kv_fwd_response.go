package chanstate

import (
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnwire"
)

// KVFwdResponseExtractor persists forwarding responses within a caller-owned
// KV transaction. The KV channel-state store uses this capability during
// channel cleanup so extracting responses and removing forwarding packages
// remain atomic.
type KVFwdResponseExtractor interface {
	ExtractFwdResponses(tx kvdb.RwTx, source lnwire.ShortChannelID) error
}

// noOpFwdResponseExtractor preserves the behavior of standalone KV stores that
// do not configure forwarding-response persistence.
type noOpFwdResponseExtractor struct{}

func (noOpFwdResponseExtractor) ExtractFwdResponses(kvdb.RwTx,
	lnwire.ShortChannelID) error {

	return nil
}
