package routing

import (
	"github.com/lightningnetwork/lnd/channeldb/models"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/mock"
)

// mockAdditionalEdge is a mock of the AdditionalEdge interface.
type mockAdditionalEdge struct{ mock.Mock }

// IntermediatePayloadSize returns the sphinx payload size defined in BOLT04 if
// this edge were to be included in a route.
func (m *mockAdditionalEdge) IntermediatePayloadSize(amount lnwire.MilliSatoshi,
	expiry uint32, legacy bool, channelID uint64) uint64 {

	args := m.Called(amount, expiry, legacy, channelID)

	return args.Get(0).(uint64)
}

// EdgePolicy return the policy of the mockAdditionalEdge.
func (m *mockAdditionalEdge) EdgePolicy() *models.CachedEdgePolicy {
	args := m.Called()

	edgePolicy := args.Get(0)
	if edgePolicy == nil {
		return nil
	}

	return edgePolicy.(*models.CachedEdgePolicy)
}
