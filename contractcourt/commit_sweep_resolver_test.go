package contractcourt

import (
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lntest/mock"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/sweep"
	"github.com/stretchr/testify/require"
)

const (
	testCommitSweepConfHeight = 99
)

type commitSweepResolverTestContext struct {
	resolver           *commitSweepResolver
	notifier           *mock.ChainNotifier
	sweeper            *mockSweeper
	resolverResultChan chan resolveResult
	t                  *testing.T
}

func newCommitSweepResolverTestContext(t *testing.T,
	resolution *lnwallet.CommitOutputResolution,
	confirmHeight uint32) *commitSweepResolverTestContext {

	notifier := &mock.ChainNotifier{
		EpochChan: make(chan *chainntnfs.BlockEpoch),
		SpendChan: make(chan *chainntnfs.SpendDetail),
		ConfChan:  make(chan *chainntnfs.TxConfirmation),
	}

	sweeper := newMockSweeper()

	checkPointChan := make(chan struct{}, 1)

	chainCfg := ChannelArbitratorConfig{
		ChainArbitratorConfig: ChainArbitratorConfig{
			Notifier: notifier,
			Sweeper:  sweeper,
			Budget:   *DefaultBudgetConfig(),
			QueryIncomingCircuit: func(
				circuit models.CircuitKey) *models.CircuitKey {

				return nil
			},
		},
		PutResolverReport: func(_ kvdb.RwTx,
			_ *channeldb.ResolverReport) error {

			return nil
		},
	}

	cfg := ResolverConfig{
		ChannelArbitratorConfig: chainCfg,
		Checkpoint: func(_ ContractResolver,
			_ ...*channeldb.ResolverReport) error {

			checkPointChan <- struct{}{}
			return nil
		},
	}

	resolver := newCommitSweepResolver(
		*resolution, confirmHeight, wire.OutPoint{}, cfg,
	)

	return &commitSweepResolverTestContext{
		resolver: resolver,
		notifier: notifier,
		sweeper:  sweeper,
		t:        t,
	}
}

func (i *commitSweepResolverTestContext) resolve() {
	// Start resolver.
	i.resolverResultChan = make(chan resolveResult, 1)
	go func() {
		err := i.resolver.Launch()
		require.NoError(i.t, err)

		nextResolver, err := i.resolver.Resolve()
		i.resolverResultChan <- resolveResult{
			nextResolver: nextResolver,
			err:          err,
		}
	}()
}

func (i *commitSweepResolverTestContext) waitForResult() {
	i.t.Helper()

	result := <-i.resolverResultChan
	if result.err != nil {
		i.t.Fatal(result.err)
	}

	if result.nextResolver != nil {
		i.t.Fatal("expected no next resolver")
	}
}

type mockSweeper struct {
	sweptInputs       chan input.Input
	updatedInputs     chan wire.OutPoint
	sweepTx           *wire.MsgTx
	sweepErr          error
	createSweepTxChan chan *wire.MsgTx

	deadlines []int
}

func newMockSweeper() *mockSweeper {
	return &mockSweeper{
		sweptInputs:       make(chan input.Input, 3),
		updatedInputs:     make(chan wire.OutPoint),
		sweepTx:           &wire.MsgTx{},
		createSweepTxChan: make(chan *wire.MsgTx),
		deadlines:         []int{},
	}
}

func (s *mockSweeper) SweepInput(input input.Input, params sweep.Params) (
	chan sweep.Result, error) {

	s.sweptInputs <- input

	// Update the deadlines used if it's set.
	params.DeadlineHeight.WhenSome(func(d int32) {
		s.deadlines = append(s.deadlines, int(d))
	})

	result := make(chan sweep.Result, 1)
	result <- sweep.Result{
		Tx:  s.sweepTx,
		Err: s.sweepErr,
	}
	return result, nil
}

func (s *mockSweeper) RelayFeePerKW() chainfee.SatPerKWeight {
	return 253
}

func (s *mockSweeper) UpdateParams(input wire.OutPoint,
	params sweep.Params) (chan sweep.Result, error) {

	s.updatedInputs <- input

	result := make(chan sweep.Result, 1)
	result <- sweep.Result{
		Tx: s.sweepTx,
	}
	return result, nil
}

var _ UtxoSweeper = &mockSweeper{}

// TestCommitSweepResolverNoDelay tests resolution of a direct commitment output
// unencumbered by a time lock.
func TestCommitSweepResolverNoDelay(t *testing.T) {
	t.Parallel()
	defer timeout()()

	res := lnwallet.CommitOutputResolution{
		SelfOutputSignDesc: input.SignDescriptor{
			Output: &wire.TxOut{
				Value: 100,
			},
			WitnessScript: []byte{0},
		},
	}

	ctx := newCommitSweepResolverTestContext(
		t, &res, testCommitSweepConfHeight,
	)

	// Replace our checkpoint with one which will push reports into a
	// channel for us to consume. We replace this function on the resolver
	// itself because it is created by the test context.
	reportChan := make(chan *channeldb.ResolverReport)
	ctx.resolver.Checkpoint = func(_ ContractResolver,
		reports ...*channeldb.ResolverReport) error {

		// Send all of our reports into the channel.
		for _, report := range reports {
			reportChan <- report
		}

		return nil
	}

	ctx.resolve()

	// No csv delay, so the input should be swept immediately.
	<-ctx.sweeper.sweptInputs

	spendTx := &wire.MsgTx{}
	spendHash := spendTx.TxHash()

	amt := btcutil.Amount(res.SelfOutputSignDesc.Output.Value)
	expectedReport := &channeldb.ResolverReport{
		OutPoint:        wire.OutPoint{},
		Amount:          amt,
		ResolverType:    channeldb.ResolverTypeCommit,
		ResolverOutcome: channeldb.ResolverOutcomeClaimed,
		SpendTxID:       &spendHash,
	}

	assertResolverReport(t, reportChan, expectedReport)

	ctx.waitForResult()
}

// testCommitSweepResolverDelay tests resolution of a direct commitment output
// that is encumbered by a time lock. sweepErr indicates whether the local node
// fails to sweep the output.
func testCommitSweepResolverDelay(t *testing.T, sweepErr error) {
	defer timeout()()

	const sweepProcessInterval = 100 * time.Millisecond
	amt := int64(100)
	outpoint := wire.OutPoint{
		Index: 5,
	}
	res := lnwallet.CommitOutputResolution{
		SelfOutputSignDesc: input.SignDescriptor{
			Output: &wire.TxOut{
				Value: amt,
			},
			WitnessScript: []byte{0},
		},
		MaturityDelay: 3,
		SelfOutPoint:  outpoint,
	}

	// Use confirmHeight = 99, so maturityHeight = 99 + 3 = 102.
	ctx := newCommitSweepResolverTestContext(
		t, &res, testCommitSweepConfHeight,
	)

	// Replace our checkpoint with one which will push reports into a
	// channel for us to consume. We replace this function on the resolver
	// itself because it is created by the test context.
	reportChan := make(chan *channeldb.ResolverReport)
	ctx.resolver.Checkpoint = func(_ ContractResolver,
		reports ...*channeldb.ResolverReport) error {

		// Send all of our reports into the channel.
		for _, report := range reports {
			reportChan <- report
		}

		return nil
	}

	// Setup whether we expect the sweeper to receive a sweep error in this
	// test case.
	ctx.sweeper.sweepErr = sweepErr

	report := ctx.resolver.report()
	expectedReport := ContractReport{
		Outpoint:     outpoint,
		Type:         ReportOutputUnencumbered,
		Amount:       btcutil.Amount(amt),
		LimboBalance: btcutil.Amount(amt),
	}
	require.Equal(t, expectedReport, *report)

	ctx.resolve()

	// Allow resolver to launch and update the report.
	time.Sleep(sweepProcessInterval)

	// Expect report to be updated.
	// confirmHeight(99) + maturityDelay(3) = 102.
	report = ctx.resolver.report()
	expectedMaturity := testCommitSweepConfHeight + res.MaturityDelay
	require.Equal(t, expectedMaturity, report.MaturityHeight)

	// Notify initial block height. Although the csv lock is still in
	// effect, we expect the input being sent to the sweeper before the csv
	// lock expires.
	//
	// Set the resolution report outcome based on whether our sweep
	// succeeded.
	outcome := channeldb.ResolverOutcomeClaimed
	if sweepErr != nil {
		outcome = channeldb.ResolverOutcomeUnclaimed
	}
	sweepTx := ctx.sweeper.sweepTx.TxHash()

	assertResolverReport(t, reportChan, &channeldb.ResolverReport{
		OutPoint:        outpoint,
		ResolverType:    channeldb.ResolverTypeCommit,
		ResolverOutcome: outcome,
		Amount:          btcutil.Amount(amt),
		SpendTxID:       &sweepTx,
	})

	ctx.waitForResult()

	// If this test case generates a sweep error, we don't expect to be
	// able to recover anything. This might happen if the local commitment
	// output was swept by a justice transaction by the remote party.
	expectedRecoveredBalance := btcutil.Amount(amt)
	if sweepErr != nil {
		expectedRecoveredBalance = 0
	}

	report = ctx.resolver.report()
	expectedReport = ContractReport{
		Outpoint:         outpoint,
		Type:             ReportOutputUnencumbered,
		Amount:           btcutil.Amount(amt),
		MaturityHeight:   testCommitSweepConfHeight + res.MaturityDelay,
		RecoveredBalance: expectedRecoveredBalance,
	}
	require.Equal(t, expectedReport, *report)
}

// TestCommitSweepResolverDelay tests resolution of a direct commitment output
// that is encumbered by a time lock.
func TestCommitSweepResolverDelay(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		sweepErr error
	}{{
		name:     "success",
		sweepErr: nil,
	}, {
		name:     "remote spend",
		sweepErr: sweep.ErrRemoteSpend,
	}}

	for _, tc := range testCases {
		tc := tc
		ok := t.Run(tc.name, func(t *testing.T) {
			testCommitSweepResolverDelay(t, tc.sweepErr)
		})
		if !ok {
			break
		}
	}
}
