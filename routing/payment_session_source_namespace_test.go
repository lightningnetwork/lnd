package routing

import (
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/fn/v2"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// TestSessionSourceNamespace tests that the SessionSource correctly uses
// namespaced mission control instances when creating payment sessions.
func TestSessionSourceNamespace(t *testing.T) {
	t.Parallel()

	// Create test source node.
	sourceNode := &models.LightningNode{
		PubKeyBytes: route.Vertex{1, 2, 3},
	}

	// Track which MC instance is used.
	var defaultMCUsed, customMCUsed bool

	// Create mission control instances with tracking.
	defaultMC := &mockMissionControl{}
	defaultMC.On("GetProbability",
		route.Vertex{1}, route.Vertex{2},
		lnwire.MilliSatoshi(1000), btcutil.Amount(10000),
		mock.AnythingOfType("[]routing.EstimatorOption")).
		Return(0.5).
		Run(func(args mock.Arguments) {
			defaultMCUsed = true
		})

	customMC := &mockMissionControl{}
	customMC.On("GetProbability",
		route.Vertex{1}, route.Vertex{2},
		lnwire.MilliSatoshi(1000), btcutil.Amount(10000),
		mock.AnythingOfType("[]routing.EstimatorOption")).
		Return(0.6).
		Run(func(args mock.Arguments) {
			customMCUsed = true
		})

	// Create the GetMissionControl function that returns different
	// MC instances based on namespace.
	getMissionControl := func(namespace string) (
		MissionControlQuerier, error) {

		switch namespace {
		case "custom":
			return customMC, nil
		case "error":
			return nil, fmt.Errorf("namespace error")
		default:
			return defaultMC, nil
		}
	}

	// Create a minimal graph session factory.
	mockGraphFactory := &mockGraphSessionFactory{
		sessionFunc: func(cb func(graph graphdb.NodeTraverser) error) error {
			// We don't need to actually traverse the graph for this test.
			return nil
		},
	}

	// Create session source with our mocks.
	sessionSource := &SessionSource{
		GraphSessionFactory: mockGraphFactory,
		SourceNode:          sourceNode,
		MissionControl:      defaultMC,
		GetMissionControl:   getMissionControl,
		GetLink: func(chanID lnwire.ShortChannelID) (
			htlcswitch.ChannelLink, error) {
			return nil, nil
		},
		PathFindingConfig: PathFindingConfig{
			AttemptCost: 100,
		},
	}

	testCases := []struct {
		name              string
		namespace         string
		expectDefaultMC   bool
		expectCustomMC    bool
		getMissionControl func(string) (MissionControlQuerier, error)
		expectError       bool
	}{
		{
			name:              "default namespace uses default MC",
			namespace:         "",
			expectDefaultMC:   true,
			expectCustomMC:    false,
			getMissionControl: getMissionControl,
		},
		{
			name:              "custom namespace uses custom MC",
			namespace:         "custom",
			expectDefaultMC:   false,
			expectCustomMC:    true,
			getMissionControl: getMissionControl,
		},
		{
			name:              "nil GetMissionControl uses default",
			namespace:         "custom",
			expectDefaultMC:   true,
			expectCustomMC:    false,
			getMissionControl: nil,
		},
		{
			name:              "error from GetMissionControl",
			namespace:         "error",
			expectDefaultMC:   false,
			expectCustomMC:    false,
			getMissionControl: getMissionControl,
			expectError:       true,
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			// Reset tracking flags.
			defaultMCUsed = false
			customMCUsed = false

			// Update the GetMissionControl function for this test.
			sessionSource.GetMissionControl = tc.getMissionControl

			// Create a payment with the test namespace.
			payment := &LightningPayment{
				Target:                  route.Vertex{4, 5, 6},
				Amount:                  1000,
				MissionControlNamespace: tc.namespace,
				FinalCLTVDelta:          100,
			}
			// Set a payment hash so Identifier() doesn't panic.
			testHash := lntypes.Hash{1, 2, 3}
			err := payment.SetPaymentHash(testHash)
			require.NoError(t, err)

			// Create a payment session.
			session, err := sessionSource.NewPaymentSession(
				payment, fn.None[tlv.Blob](),
				fn.None[htlcswitch.AuxTrafficShaper](),
			)

			if tc.expectError {
				require.Error(t, err)
				require.Nil(t, session)
				return
			}

			require.NoError(t, err)
			require.NotNil(t, session)

			// Cast to concrete type to access mission control.
			paymentSession, ok := session.(*paymentSession)
			require.True(t, ok)

			// Trigger MC usage by calling GetProbability.
			_ = paymentSession.missionControl.GetProbability(
				route.Vertex{1}, route.Vertex{2}, 1000, 10000,
			)

			// Verify the correct MC was used.
			require.Equal(t, tc.expectDefaultMC, defaultMCUsed,
				"default MC usage mismatch")
			require.Equal(t, tc.expectCustomMC, customMCUsed,
				"custom MC usage mismatch")
		})
	}
}

// mockGraphSessionFactory implements GraphSessionFactory for testing.
type mockGraphSessionFactory struct {
	sessionFunc func(cb func(graph graphdb.NodeTraverser) error) error
}

func (m *mockGraphSessionFactory) GraphSession(cb func(graph graphdb.NodeTraverser) error) error {
	if m.sessionFunc != nil {
		return m.sessionFunc(cb)
	}
	return nil
}
