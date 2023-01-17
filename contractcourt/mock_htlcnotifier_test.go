package contractcourt

import (
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/channeldb/models"
)

type mockHTLCNotifier struct {
	HtlcNotifier
}

func (m *mockHTLCNotifier) NotifyFinalHtlcEvent(key models.CircuitKey,
	info channeldb.FinalHtlcInfo) { //nolint:whitespace
}
