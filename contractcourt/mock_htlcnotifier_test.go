package contractcourt

import (
	"github.com/lightningnetwork/lnd/chanstate"
	"github.com/lightningnetwork/lnd/graph/db/models"
)

type mockHTLCNotifier struct {
	HtlcNotifier

	finalHtlcEvents []finalHtlcEvent
}

type finalHtlcEvent struct {
	key  models.CircuitKey
	info chanstate.FinalHtlcInfo
}

func (m *mockHTLCNotifier) NotifyFinalHtlcEvent(key models.CircuitKey,
	info chanstate.FinalHtlcInfo) {

	m.finalHtlcEvents = append(m.finalHtlcEvents, finalHtlcEvent{
		key:  key,
		info: info,
	})
}
