package reputation

import (
	"encoding/binary"
	"time"

	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/lnwire"
)

// circuit builds a CircuitKey from an scid int and htlc id.
func circuit(scidInt, htlcID uint64) models.CircuitKey {
	return models.CircuitKey{
		ChanID: lnwire.NewShortChanIDFromInt(scidInt),
		HtlcID: htlcID,
	}
}

// chanID builds a distinct ChannelID from a small integer.
func chanID(v uint64) lnwire.ChannelID {
	var id lnwire.ChannelID
	binary.BigEndian.PutUint64(id[24:], v)

	return id
}

// advance moves a test clock forward by d.
func advance(c *clock.TestClock, d time.Duration) {
	c.SetTime(c.Now().Add(d))
}
