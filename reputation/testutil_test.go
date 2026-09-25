package reputation

import (
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

// scid builds a ShortChannelID from its integer representation.
func scid(v uint64) lnwire.ShortChannelID {
	return lnwire.NewShortChanIDFromInt(v)
}

// advance moves a test clock forward by d.
func advance(c *clock.TestClock, d time.Duration) {
	c.SetTime(c.Now().Add(d))
}
