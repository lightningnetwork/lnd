package discovery

import (
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
)

// remotePubFromChanInfo returns the public key of the remote peer given a
// ChannelEdgeInfo that describe a channel we have with them.
func remotePubFromChanInfo(chanInfo *channeldb.ChannelEdgeInfo,
	chanFlags lnwire.ChanUpdateChanFlags) [33]byte {

	var remotePubKey [33]byte
	switch {
	case chanFlags&lnwire.ChanUpdateDirection == 0:
		remotePubKey = chanInfo.NodeKey2Bytes
	case chanFlags&lnwire.ChanUpdateDirection == 1:
		remotePubKey = chanInfo.NodeKey1Bytes
	}

	return remotePubKey
}
