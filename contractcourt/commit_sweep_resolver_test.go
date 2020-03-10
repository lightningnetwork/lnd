package contractcourt

import (
	"reflect"
	"testing"
	"time"

	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/sweep"
)

type commitSweepResolverTestContext struct {
	resolver           *commitSweepResolver
	notifier           *mockNotifier
	sweeper            *mockSweeper
	resolverResultChan chan resolveResult
	t                  *testing.T
}

func newCommitSweepResolverTestContext(t *testing.T,
	resolution *lnwallet.CommitOutputResolution) *commitSweepResolverTestContext {

	notifier := &mockNotifier{
		epochChan: make(chan *chainntnfs.BlockEpoch),
		spendChan: make(chan *chainntnfs.SpendDetail),
		confChan:  make(chan *chainntnfs.TxConfirmation),
	}

	sweeper := newMockSweeper()

	checkPointChan := make(chan struct{}, 1)

	chainCfg := ChannelArbitratorConfig{
		ChainArbitratorConfig: ChainArbitratorConfig{
			Notifier: notifier,
			Sweeper:  sweeper,
		},
	}

	cfg := ResolverConfig{
		ChannelArbitratorConfig: chainCfg,
		Checkpoint: func(_ ContractResolver) error {
			checkPointChan <- struct{}{}
			return nil
		},
	}

	resolver := newCommitSweepResolver(
		*resolution, 0, wire.OutPoint{}, cfg,
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
		nextResolver, err := i.resolver.Resolve()
		i.resolverResultChan <- resolveResult{
			nextResolver: nextResolver,
			err:          err,
		}
	}()
}

func (i *commitSweepResolverTestContext) notifyEpoch(height int32) {
	i.notifier.epochChan <- &chainntnfs.BlockEpoch{
		Height: height,
	}
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
	sweptInputs chan input.Input
}

func newMockSweeper() *mockSweeper {
	return &mockSweeper{
		sweptInputs: make(chan input.Input),
	}
}

func (s *mockSweeper) SweepInput(input input.Input, params sweep.Params) (
	chan sweep.Result, error) {

	s.sweptInputs <- input

	result := make(chan sweep.Result, 1)
	result <- sweep.Result{
		Tx: &wire.MsgTx{},
	}
	return result, nil
}

func (s *mockSweeper) CreateSweepTx(inputs []input.Input, feePref sweep.FeePreference,
	currentBlockHeight uint32) (*wire.MsgTx, error) {

	return nil, nil
}

var _ UtxoSweeper = &mockSweeper{}

// TestCommitSweepResolverNoDelay tests resolution of a direct commitment output
// unencumbered by a time lock.
func TestCommitSweepResolverNoDelay(t *testing.T) {
	t.Parallel()
	defer timeout(t)()

	res := lnwallet.CommitOutputResolution{
		SelfOutputSignDesc: input.SignDescriptor{
			Output: &wire.TxOut{
				Value: 100,
			},
			WitnessScript: []byte{0},
		},
	}

	ctx := newCommitSweepResolverTestContext(t, &res)
	ctx.resolve()

	ctx.notifier.confChan <- &chainntnfs.TxConfirmation{}

	// No csv delay, so the input should be swept immediately.
	<-ctx.sweeper.sweptInputs

	ctx.waitForResult()
}

// TestCommitSweepResolverDelay tests resolution of a direct commitment output
// that is encumbered by a time lock.
func TestCommitSweepResolverDelay(t *testing.T) {
	t.Parallel()
	defer timeout(t)()

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

	ctx := newCommitSweepResolverTestContext(t, &res)

	report := ctx.resolver.report()
	if !reflect.DeepEqual(report, &ContractReport{
		Outpoint:     outpoint,
		Type:         ReportOutputUnencumbered,
		Amount:       btcutil.Amount(amt),
		LimboBalance: btcutil.Amount(amt),
	}) {
		t.Fatal("unexpected resolver report")
	}

	ctx.resolve()

	ctx.notifier.confChan <- &chainntnfs.TxConfirmation{
		BlockHeight: testInitialBlockHeight - 1,
	}

	// Allow resolver to process confirmation.
	time.Sleep(100 * time.Millisecond)

	// Expect report to be updated.
	report = ctx.resolver.report()
	if report.MaturityHeight != testInitialBlockHeight+2 {
		t.Fatal("report maturity height incorrect")
	}

	// Notify initial block height. The csv lock is still in effect, so we
	// don't expect any sweep to happen yet.
	ctx.notifyEpoch(testInitialBlockHeight)

	select {
	case <-ctx.sweeper.sweptInputs:
		t.Fatal("no sweep expected")
	case <-time.After(100 * time.Millisecond):
	}

	// A new block arrives. The commit tx confirmed at height -1 and the csv
	// is 3, so a spend will be valid in the first block after height +1.
	ctx.notifyEpoch(testInitialBlockHeight + 1)

	<-ctx.sweeper.sweptInputs

	ctx.waitForResult()

	report = ctx.resolver.report()
	if !reflect.DeepEqual(report, &ContractReport{
		Outpoint:         outpoint,
		Type:             ReportOutputUnencumbered,
		Amount:           btcutil.Amount(amt),
		RecoveredBalance: btcutil.Amount(amt),
		MaturityHeight:   testInitialBlockHeight + 2,
	}) {
		t.Fatal("unexpected resolver report")
	}
}
