package htlcswitch

import (
	"crypto/sha256"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/htlcswitch/hop"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// errFwdResponseStore is injected to simulate an unreadable response store.
var errFwdResponseStore = errors.New("injected response store error")

// testFwdResponseSource stands in for the durable response store so switch
// tests exercise replay policy without reaching into channeldb's close path.
type testFwdResponseSource struct {
	// mu guards responses while the switch is running.
	mu sync.Mutex

	// responses holds persisted records by outgoing circuit key.
	responses map[CircuitKey]*channeldb.FwdResponse

	// fetchErr is returned instead of the stored response set.
	fetchErr error
}

// newTestFwdResponseSource constructs a source holding the given responses.
func newTestFwdResponseSource(
	responses ...*channeldb.FwdResponse) *testFwdResponseSource {

	source := &testFwdResponseSource{
		responses: make(map[CircuitKey]*channeldb.FwdResponse),
	}
	for _, response := range responses {
		source.responses[response.OutKey] = response
	}

	return source
}

// fetch returns the current response set or an injected storage error.
func (s *testFwdResponseSource) fetch() ([]*channeldb.FwdResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.fetchErr != nil {
		return nil, s.fetchErr
	}

	responses := make([]*channeldb.FwdResponse, 0, len(s.responses))
	for _, response := range s.responses {
		responses = append(responses, response)
	}

	return responses, nil
}

// remove reaps a record the switch could not deliver.
func (s *testFwdResponseSource) remove(outKey *CircuitKey) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.responses, *outKey)

	return nil
}

// len reports how many records remain.
func (s *testFwdResponseSource) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return len(s.responses)
}

// attach points a switch at this source.
func (s *testFwdResponseSource) attach(sw *Switch) {
	sw.cfg.FetchFwdResponses = s.fetch
	sw.cfg.DeleteFwdResponse = s.remove
}

// TestSwitchReforwardFwdResponses asserts that a persisted response is replayed
// on startup as the exact wire message rather than a reconstruction.
func TestSwitchReforwardFwdResponses(t *testing.T) {
	t.Parallel()

	db := channeldb.OpenForTesting(t, t.TempDir())

	source := lnwire.NewShortChanIDFromInt(9200)
	inKey := CircuitKey{
		ChanID: lnwire.NewShortChanIDFromInt(9210),
		HtlcID: 1,
	}
	outKey := CircuitKey{ChanID: source, HtlcID: 2}

	s1, err := initSwitchWithDB(testStartingHeight, db)
	require.NoError(t, err)
	circuit := commitFwdResponseTestCircuit(t, s1, inKey, outKey)

	preimage := [32]byte{7}
	responses := newTestFwdResponseSource(&channeldb.FwdResponse{
		OutKey: outKey,
		Message: &lnwire.UpdateFulfillHTLC{
			ID:              outKey.HtlcID,
			PaymentPreimage: preimage,
		},
	})

	s2, err := initSwitchWithDB(testStartingHeight, db)
	require.NoError(t, err)
	responses.attach(s2)
	require.NoError(t, s2.Start())
	t.Cleanup(func() {
		_ = s2.Stop()
	})

	link := addFwdResponseTestLink(t, s2, inKey.ChanID)
	var packet *htlcPacket
	select {
	case packet = <-link.packets:
	case <-time.After(5 * time.Second):
		t.Fatal("response was not delivered to incoming link")
	}

	require.Nil(t, packet.destRef)
	require.Equal(t, outKey.ChanID, packet.outgoingChanID)
	require.Equal(t, outKey.HtlcID, packet.outgoingHTLCID)
	require.Equal(t, inKey.ChanID, packet.incomingChanID)
	require.Equal(t, inKey.HtlcID, packet.incomingHTLCID)
	require.NotNil(t, packet.sourceRef)
	require.Equal(t, circuit.AddRef, *packet.sourceRef)

	settle, ok := packet.htlc.(*lnwire.UpdateFulfillHTLC)
	require.True(t, ok)
	require.Equal(t, preimage, settle.PaymentPreimage)
}

// TestSwitchReapsOrphanedFwdResponses asserts that a persisted response with no
// open circuit is removed rather than retained indefinitely.
func TestSwitchReapsOrphanedFwdResponses(t *testing.T) {
	t.Parallel()

	db := channeldb.OpenForTesting(t, t.TempDir())

	outKey := CircuitKey{
		ChanID: lnwire.NewShortChanIDFromInt(9300),
		HtlcID: 5,
	}
	responses := newTestFwdResponseSource(&channeldb.FwdResponse{
		OutKey: outKey,
		Message: &lnwire.UpdateFulfillHTLC{
			ID:              outKey.HtlcID,
			PaymentPreimage: [32]byte{3},
		},
	})

	s, err := initSwitchWithDB(testStartingHeight, db)
	require.NoError(t, err)
	responses.attach(s)
	require.NoError(t, s.Start())
	t.Cleanup(func() {
		_ = s.Stop()
	})

	require.Zero(t, responses.len())
}

// TestSwitchFwdResponseFetchFails asserts that an unreadable response store
// aborts start-up. Treating a storage error as an empty set would silently
// skip the recovery this store exists for.
func TestSwitchFwdResponseFetchFails(t *testing.T) {
	t.Parallel()

	db := channeldb.OpenForTesting(t, t.TempDir())

	s, err := initSwitchWithDB(testStartingHeight, db)
	require.NoError(t, err)

	responses := newTestFwdResponseSource()
	responses.fetchErr = errFwdResponseStore
	responses.attach(s)

	require.ErrorIs(t, s.Start(), errFwdResponseStore)
}

// TestSwitchReforwardFwdResponseLocalPayment asserts that a persisted response
// for a locally initiated payment reaches the payment result store rather than
// a link.
func TestSwitchReforwardFwdResponseLocalPayment(t *testing.T) {
	t.Parallel()

	db := channeldb.OpenForTesting(t, t.TempDir())

	const attemptID = 21
	source := lnwire.NewShortChanIDFromInt(9400)
	inKey := CircuitKey{ChanID: hop.Source, HtlcID: attemptID}
	outKey := CircuitKey{ChanID: source, HtlcID: 22}

	s1, err := initSwitchWithDB(testStartingHeight, db)
	require.NoError(t, err)
	circuit := commitFwdResponseTestCircuit(t, s1, inKey, outKey)

	preimage := [32]byte{4}
	responses := newTestFwdResponseSource(&channeldb.FwdResponse{
		OutKey: outKey,
		Message: &lnwire.UpdateFulfillHTLC{
			ID:              outKey.HtlcID,
			PaymentPreimage: preimage,
		},
	})

	s2, err := initSwitchWithDB(testStartingHeight, db)
	require.NoError(t, err)
	responses.attach(s2)
	require.NoError(t, s2.Start())
	t.Cleanup(func() {
		_ = s2.Stop()
	})

	// The settle is routed to the switch itself, not to a link, and the
	// attempt's durable result records the preimage.
	require.Eventually(t, func() bool {
		hasResult, err := s2.HasAttemptResult(attemptID)

		return err == nil && hasResult
	}, 5*time.Second, 10*time.Millisecond)

	resultChan, err := s2.GetAttemptResult(
		attemptID, circuit.PaymentHash, newMockDeobfuscator(),
	)
	require.NoError(t, err)

	select {
	case result := <-resultChan:
		require.NoError(t, result.Error)
		require.Equal(t, preimage, result.Preimage)

	case <-time.After(5 * time.Second):
		t.Fatal("local payment result was not restored")
	}
}

// TestSwitchDuplicateFwdResponseFail verifies that a second failure for a
// circuit already being closed is an expected no-op. This occurs when the
// forwarding response store and contract court both replay a response for the
// same circuit during startup.
func TestSwitchDuplicateFwdResponseFail(t *testing.T) {
	t.Parallel()

	db := channeldb.OpenForTesting(t, t.TempDir())

	s, err := initSwitchWithDB(testStartingHeight, db)
	require.NoError(t, err)

	inKey := CircuitKey{
		ChanID: lnwire.NewShortChanIDFromInt(9900),
		HtlcID: 1,
	}
	outKey := CircuitKey{
		ChanID: lnwire.NewShortChanIDFromInt(9901),
		HtlcID: 2,
	}
	commitFwdResponseTestCircuit(t, s, inKey, outKey)

	// Model the exact response winning the startup race and marking the
	// circuit closing before the generic resolution is handled.
	_, err = s.circuits.CloseCircuit(outKey)
	require.NoError(t, err)

	fail := &lnwire.UpdateFailHTLC{ID: outKey.HtlcID}
	packet := &htlcPacket{
		outgoingChanID: outKey.ChanID,
		outgoingHTLCID: outKey.HtlcID,
		isResolution:   true,
		htlc:           fail,
	}

	require.NoError(t, s.handlePacketFail(packet, fail))
}

// commitFwdResponseTestCircuit persists and opens a circuit used by response
// replay tests.
func commitFwdResponseTestCircuit(t *testing.T, s *Switch, inKey,
	outKey CircuitKey) *PaymentCircuit {

	t.Helper()

	hash := sha256.Sum256([]byte(inKey.String()))
	circuit := &PaymentCircuit{
		AddRef: channeldb.AddRef{
			Height: 4,
			Index:  2,
		},
		Incoming:       inKey,
		PaymentHash:    hash,
		IncomingAmount: 1200,
		OutgoingAmount: 1000,
		ErrorEncrypter: NewMockObfuscator(),
	}

	actions, err := s.circuits.CommitCircuits(circuit)
	require.NoError(t, err)
	require.Len(t, actions.Adds, 1)
	require.Empty(t, actions.Drops)
	require.Empty(t, actions.Fails)

	require.NoError(t, s.circuits.OpenCircuits(Keystone{
		InKey:  inKey,
		OutKey: outKey,
	}))

	return circuit
}

// addFwdResponseTestLink attaches a mock incoming link that exposes a replayed
// response to the test.
func addFwdResponseTestLink(t *testing.T, s *Switch,
	scid lnwire.ShortChannelID) *mockChannelLink {

	t.Helper()

	peer, err := newMockServer(
		t, "closed-response-incoming", testStartingHeight, nil,
		testDefaultDelta,
	)
	require.NoError(t, err)

	chanID, _ := genID()
	link := newMockChannelLink(
		s, chanID, scid, emptyScid, peer, true, false, false, false,
	)
	require.NoError(t, s.AddLink(link))

	return link
}
