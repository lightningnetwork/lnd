package contractcourt

import (
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/graph/db/models"
)

type mockHTLCNotifier struct {
	HtlcNotifier

	finalHtlcEvents []channeldb.FinalHtlcInfo
}

func (m *mockHTLCNotifier) NotifyFinalHtlcEvent(key models.CircuitKey,
	info channeldb.FinalHtlcInfo) {

	m.finalHtlcEvents = append(m.finalHtlcEvents, info)
}
