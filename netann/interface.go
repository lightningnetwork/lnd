package netann

import (
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/graph/db/models"
)

// DB abstracts the required database functionality needed by the
// ChanStatusManager.
type DB interface {
	// FetchAllOpenChannels returns a slice of all open channels known to
	// the daemon. This may include private or pending channels.
	FetchAllOpenChannels() ([]*channeldb.OpenChannel, error)
}

// ChannelGraph abstracts the required channel graph queries used by the
// ChanStatusManager.
type ChannelGraph interface {
	// FetchChannelEdgesByOutpoint returns the channel edge info and most
	// recent channel edge policies for a given outpoint.
	FetchChannelEdgesByOutpoint(*wire.OutPoint) (*models.ChannelEdgeInfo,
		*models.ChannelEdgePolicy, *models.ChannelEdgePolicy, error)
}
