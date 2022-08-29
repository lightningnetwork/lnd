package contractcourt

import "github.com/lightningnetwork/lnd/channeldb"

type mockHTLCNotifier struct {
	HtlcNotifier
}

func (m *mockHTLCNotifier) NotifyFinalHtlcEvent(key channeldb.CircuitKey,
	info channeldb.FinalHtlcInfo) { // nolint:whitespace
}
