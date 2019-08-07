package invoicesrpc

import (
	"strconv"
	"testing"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
)

// TestCheckInboundBandwidth creates a channel between two nodes and then makes
// each side check if there is enough bandwidth on the opposite channel to
// fulfill the specified amount.
func TestCheckInboundBandwidth(t *testing.T) {
	var tests = []struct {
		invoiceAmt   lnwire.MilliSatoshi
		hasBandwidth bool
	}{
		// 0,00001 BTC
		{invoiceAmt: 1000000, hasBandwidth: true},
		// 6 BTC
		{invoiceAmt: 600000000000, hasBandwidth: false},
		// 15 BTC
		{invoiceAmt: 1500000000000, hasBandwidth: false},
	}

	for i, tt := range tests {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			// create channel with 5 BTC on each side
			aliceChannel, bobChannel, cleanUp, err := lnwallet.CreateTestChannels()
			if err != nil {
				t.Fatal(err)
			}
			defer cleanUp()

			channels := []*channeldb.OpenChannel{
				aliceChannel.State(),
				bobChannel.State(),
			}

			// check inbound bandwidth from both sides
			for _, channel := range channels {
				hasBandwidth, _ := checkInboundBandwidth([]*channeldb.OpenChannel{channel}, tt.invoiceAmt)
				if hasBandwidth != tt.hasBandwidth {
					t.Errorf("fail inbound bandwidth check: expected %v, got %v", tt.hasBandwidth, hasBandwidth)
				}
			}
		})
	}
}
