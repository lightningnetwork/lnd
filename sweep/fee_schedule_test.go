package sweep

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

func TestScheduleSurvivesRetry(t *testing.T) {
	t.Parallel()

	tp, _ := createTestPublisher(t)
	s := New(&UtxoSweeperConfig{})
	s.currentHeight = 100
	inp := createTestInput(100000, input.WitnessKeyHash)
	pi := &SweeperInput{
		Input: &inp, state: PendingPublish, DeadlineHeight: 111,
		params: Params{Immediate: true, Budget: 10000,
			StartingFeeRate: fn.Some(chainfee.SatPerKWeight(1000))},
	}
	s.inputs[inp.OutPoint()] = pi

	request := func(height int32) *BumpRequest {
		s.currentHeight = height
		tp.currentHeight.Store(height)
		req := &BumpRequest{
			Inputs: []input.Input{&inp}, Budget: 10000,
			DeliveryAddress: changePkScript, MaxFeeRate: 11000,
			DeadlineHeight: 111,
		}
		require.NoError(t, s.attachFeeContext(req))

		return req
	}

	first, err := tp.initializeFeeFunction(request(100))
	require.NoError(t, err)
	require.EqualValues(t, 1000, first.FeeRate())
	_, err = first.Increment()
	require.NoError(t, err)

	// An active fee search is discarded: retrying in the same block does
	// not advance the schedule merely because a candidate failed.
	retry, err := tp.initializeFeeFunction(request(100))
	require.NoError(t, err)
	require.EqualValues(t, 1000, retry.FeeRate())

	// Five actual blocks, however, put us halfway along the original ramp.
	retry, err = tp.initializeFeeFunction(request(105))
	require.NoError(t, err)
	require.EqualValues(t, 6000, retry.FeeRate())
	require.EqualValues(t, 100, pi.feeSchedule.StartHeight)
	require.EqualValues(t, 1000, pi.params.StartingFeeRate.UnwrapOrFail(t))

	// Rebuilding after the deadline uses the cap, with no position error.
	retry, err = tp.initializeFeeFunction(request(112))
	require.NoError(t, err)
	require.EqualValues(t, 11000, retry.FeeRate())
}

// TestBatchDeferredSchedule checks that an immediate sweep can collect a
// deferred input. Its starting rate binds the batch at once, but its urgency
// commits nothing until its own ramp begins.
func TestBatchDeferredSchedule(t *testing.T) {
	t.Parallel()

	tp, m := createTestPublisher(t)
	s := New(&UtxoSweeperConfig{})
	s.currentHeight = 100
	tp.currentHeight.Store(100)
	for i, rate := range []chainfee.SatPerKWeight{1000, 1500} {
		inp := createTestInput(100000, input.WitnessKeyHash)
		pi := &SweeperInput{
			Input: &inp, state: Init, DeadlineHeight: 111,
			params: Params{
				Budget: 10000, Immediate: i == 0,
				StartingFeeRate: fn.Some(rate),
			},
		}
		s.inputs[inp.OutPoint()] = pi
		s.startFeeSchedule(pi)
		require.EqualValues(t, 100+i, pi.feeSchedule.StartHeight)
	}

	eligible := s.updateSweeperInputs()
	require.Len(t, eligible, 2)
	m.estimator.On("RelayFeePerKW").
		Return(chainfee.SatPerKWeight(250)).Once()
	aggregator := NewBudgetAggregator(
		m.estimator, 10, fn.None[AuxSweeper](),
	)
	sets := aggregator.ClusterInputs(eligible)
	require.Len(t, sets, 1)
	set := sets[0]
	require.True(t, set.Immediate())
	require.Len(t, set.Inputs(), 2)
	req := &BumpRequest{
		Inputs: set.Inputs(), Budget: set.Budget(),
		DeliveryAddress: changePkScript, MaxFeeRate: 11000,
		DeadlineHeight: set.DeadlineHeight(),
		Immediate:      set.Immediate(),
	}
	require.NoError(t, s.attachFeeContext(req))
	origins := append([]InputSchedule(nil), req.FeeSchedules...)
	f, err := tp.initializeFeeFunction(req)
	require.NoError(t, err)
	require.EqualValues(t, 1500, f.FeeRate())

	// On the next block only the immediate input commits its half of the
	// headroom: 1500 + 9500 * 0.5 * 1/10. Following the active attempt
	// must agree with reconstructing it, and the rate never falls.
	previous := f.FeeRate()
	for height := int32(101); height <= 112; height++ {
		target := calcCurrentConfTarget(height, req.DeadlineHeight)
		_, err := f.IncreaseFeeRate(target)
		require.NoError(t, err)
		switch {
		case height == 101:
			require.EqualValues(t, 1975, f.FeeRate())
		case height >= 110:
			require.EqualValues(t, 11000, f.FeeRate())
		}
		require.GreaterOrEqual(t, f.FeeRate(), previous)
		previous = f.FeeRate()

		tp.currentHeight.Store(height)
		rebuilt, err := tp.initializeFeeFunction(req)
		require.NoError(t, err)
		require.Equal(t, rebuilt.FeeRate(), f.FeeRate())
	}

	// A local fee search on a fresh attempt changes no saved origin.
	tp.currentHeight.Store(100)
	f, err = tp.initializeFeeFunction(req)
	require.NoError(t, err)
	require.NoError(t, increaseCandidateFee(f))
	require.Greater(t, f.FeeRate(), chainfee.SatPerKWeight(1500))
	rebuilt, err := tp.initializeFeeFunction(req)
	require.NoError(t, err)
	require.EqualValues(t, 1500, rebuilt.FeeRate())
	require.Equal(t, origins, req.FeeSchedules)
	for _, pi := range s.inputs {
		require.Equal(t, pi.params.StartingFeeRate,
			pi.feeSchedule.StartingFeeRate)
	}
}

func TestScheduleReconstruction(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		tp, _ := createTestPublisher(t)
		width := rapid.Uint32Range(2, 100).Draw(rt, "width")
		elapsed := rapid.Uint32Range(0, width).Draw(rt, "elapsed")
		capRate := chainfee.SatPerKWeight(
			rapid.Int64Range(1001, 2000).Draw(rt, "cap"),
		)
		count := rapid.IntRange(1, 5).Draw(rt, "schedules")
		schedules := make([]InputSchedule, count)
		for i := range schedules {
			start := chainfee.SatPerKWeight(
				rapid.Int64Range(250, 1000).Draw(rt, "start"),
			)
			schedules[i] = InputSchedule{
				FeeSchedule: FeeSchedule{
					StartHeight: rapid.Int32Range(80, 101).
						Draw(rt, "origin"),
					StartingFeeRate: fn.Some(start),
				},
				Budget: btcutil.Amount(
					rapid.Int64Range(0, 100000).
						Draw(rt, "budget"),
				),
			}
		}
		inp := createTestInput(100000, input.WitnessKeyHash)
		req := &BumpRequest{
			Inputs:          []input.Input{&inp},
			DeliveryAddress: changePkScript,
			Budget:          10000, MaxFeeRate: capRate,
			DeadlineHeight: 100 + int32(width) + 1,
			FeeSchedules:   schedules,
		}
		tp.currentHeight.Store(100)
		original, err := tp.initializeFeeFunction(req)
		require.NoError(rt, err)
		_, err = original.IncreaseFeeRate(width + 1 - elapsed)
		require.NoError(rt, err)
		tp.currentHeight.Store(100 + int32(elapsed))
		reconstructed, err := tp.initializeFeeFunction(req)
		require.NoError(rt, err)
		require.Equal(rt, original.FeeRate(), reconstructed.FeeRate())
		require.LessOrEqual(rt, reconstructed.FeeRate(), capRate)
	})
}

func TestSchedulePolicyAndHistory(t *testing.T) {
	t.Parallel()

	s := New(&UtxoSweeperConfig{})
	s.currentHeight = 100
	inp := createTestInput(100000, input.WitnessKeyHash)
	pi := &SweeperInput{
		Input: &inp, DeadlineHeight: 111,
		params: Params{Budget: 10000,
			StartingFeeRate: fn.Some(chainfee.SatPerKWeight(1000))},
	}
	s.inputs[inp.OutPoint()] = pi
	s.startFeeSchedule(pi)
	require.EqualValues(t, 101, pi.feeSchedule.StartHeight)
	original := pi.feeSchedule
	params := pi.params
	params.Immediate = true
	pi.updateFeeSchedulePolicy(params)
	require.Same(t, original, pi.feeSchedule)
	params.Budget++
	pi.updateFeeSchedulePolicy(params)
	require.Nil(t, pi.feeSchedule)

	// Two inputs share one published transaction; resolving one spend
	// invalidates the replacement hint for the surviving input as well.
	other := createTestInput(1000, input.WitnessKeyHash)
	survivor := &SweeperInput{Input: &other}
	s.inputs[other.OutPoint()] = survivor
	info := RBFInfo{FeeRate: 2000}
	pi.rbf, survivor.rbf = fn.Some(info), fn.Some(info)
	survivor.lastFeeRate = 2000
	s.invalidateFeeContext(&wire.MsgTx{TxIn: []*wire.TxIn{
		{PreviousOutPoint: inp.OutPoint()},
	}})
	require.True(t, survivor.rbf.IsNone())
	require.EqualValues(t, 2000, survivor.lastFeeRate)
}

func TestRegroupedScheduleRespectsBudget(t *testing.T) {
	t.Parallel()

	tp, _ := createTestPublisher(t)
	tp.currentHeight.Store(105)
	inp := createTestInput(100000, input.WitnessKeyHash)
	req := &BumpRequest{
		Inputs:          []input.Input{&inp},
		DeliveryAddress: changePkScript,
		Budget:          10000, MaxFeeRate: 2000, DeadlineHeight: 111,
		FeeSchedules: []InputSchedule{{FeeSchedule: FeeSchedule{
			StartHeight: 100,
			StartingFeeRate: fn.Some(
				chainfee.SatPerKWeight(1000),
			),
		}}},
		PreviousTxs: []RBFInfo{{FeeRate: 10000}},
	}
	f, err := tp.initializeFeeFunction(req)
	require.NoError(t, err)
	require.EqualValues(t, 2000, f.FeeRate())

	// The floor is an accident of the attempt: the schedule beneath it is
	// untouched and still has steps to take, none of which raise the
	// effective rate above the cap.
	for {
		increased, err := f.Increment()
		if err != nil {
			require.ErrorIs(t, err, ErrMaxPosition)
			break
		}
		require.False(t, increased)
		require.EqualValues(t, 2000, f.FeeRate())
	}

	// Without the invalidated publication hint, the survivor starts at
	// its own elapsed urgency, using the smaller batch's affordable cap.
	req.PreviousTxs = nil
	f, err = tp.initializeFeeFunction(req)
	require.NoError(t, err)
	require.EqualValues(t, 1500, f.FeeRate())
}

// TestRegroupedInputCommitsOwnBudget checks that an old input regrouped with
// a new one commits only its own budget to its elapsed urgency. The new
// input's ramp has just begun, so its budget is not yet committed.
func TestRegroupedInputCommitsOwnBudget(t *testing.T) {
	t.Parallel()

	tp, _ := createTestPublisher(t)
	tp.currentHeight.Store(600)
	old := createTestInput(2_000_000, input.WitnessKeyHash)
	fresh := createTestInput(2_000_000, input.WitnessKeyHash)
	start := fn.Some(chainfee.SatPerKWeight(300))
	req := &BumpRequest{
		Inputs:          []input.Input{&old, &fresh},
		DeliveryAddress: changePkScript,
		Budget:          1_001_000, MaxFeeRate: 10_000_000,
		DeadlineHeight: 1100,
		FeeSchedules: []InputSchedule{{
			FeeSchedule: FeeSchedule{
				StartHeight: 100, StartingFeeRate: start,
			},
			Budget: 1_000,
		}, {
			FeeSchedule: FeeSchedule{
				StartHeight: 600, StartingFeeRate: start,
			},
			Budget: 1_000_000,
		}},
	}
	capRate, err := req.MaxFeeRateAllowed()
	require.NoError(t, err)
	f, err := tp.initializeFeeFunction(req)
	require.NoError(t, err)

	// The old input is 500 blocks along a 999-block ramp. It commits that
	// fraction of its budget's share of the headroom, which is a small
	// fraction of the pooled cap.
	share := 1_000.0 / 1_001_000.0
	committed := float64(capRate-300) * share * 500 / 999
	require.InDelta(t, 300+committed, float64(f.FeeRate()), 1)
	require.Less(t, f.FeeRate(), capRate/1000)

	// Every budget is committed by the deadline.
	_, err = f.IncreaseFeeRate(1)
	require.NoError(t, err)
	require.Equal(t, capRate, f.FeeRate())
}

// outpointAuxSweeper attributes extra budget to particular inputs, additively,
// as the AuxSweeper interface requires.
type outpointAuxSweeper struct {
	MockAuxSweeper

	extra map[wire.OutPoint]btcutil.Amount
}

// ExtraBudgetForInputs sums the extra budget attributed to each input.
func (a *outpointAuxSweeper) ExtraBudgetForInputs(
	inputs []input.Input) fn.Result[btcutil.Amount] {

	var total btcutil.Amount
	for _, inp := range inputs {
		total += a.extra[inp.OutPoint()]
	}

	return fn.Ok(total)
}

// TestScheduleWeightsIncludeAuxBudget checks that a schedule is weighted by
// its input's whole contribution to the batch's budget, including extra budget
// that the aux sweeper attributes to it. Otherwise another input's urgency
// could commit that extra budget, or the input's own urgency would barely
// commit it.
func TestScheduleWeightsIncludeAuxBudget(t *testing.T) {
	t.Parallel()

	start := fn.Some(chainfee.SatPerKWeight(1000))
	params := Params{Budget: 1000, Immediate: true, StartingFeeRate: start}

	// An old input is halfway along its ramp and a new one is just
	// starting. Either may carry extra budget, as custom channel inputs
	// do.
	for _, tc := range []struct {
		name           string
		oldAux, newAux btcutil.Amount
	}{
		{"new input's extra budget", 0, 20_000},
		{"old input's extra budget", 20_000, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := createTestInput(100000, input.WitnessKeyHash)
			fresh := createTestInput(100000, input.WitnessKeyHash)
			aux := &outpointAuxSweeper{
				extra: map[wire.OutPoint]btcutil.Amount{
					old.OutPoint():   tc.oldAux,
					fresh.OutPoint(): tc.newAux,
				},
			}
			s := New(&UtxoSweeperConfig{
				AuxSweeper: fn.Some[AuxSweeper](aux),
			})
			s.currentHeight = 105
			s.inputs[old.OutPoint()] = &SweeperInput{
				Input: &old, params: params,
				DeadlineHeight: 111,
				feeSchedule: &FeeSchedule{
					StartHeight:     100,
					StartingFeeRate: start,
				},
			}
			s.inputs[fresh.OutPoint()] = &SweeperInput{
				Input: &fresh, params: params,
				DeadlineHeight: 111,
			}

			total := 2000 + tc.oldAux + tc.newAux
			req := &BumpRequest{
				Inputs:          []input.Input{&old, &fresh},
				DeliveryAddress: changePkScript,
				Budget:          total, MaxFeeRate: 1_000_000,
				DeadlineHeight: 111,
			}
			require.NoError(t, s.attachFeeContext(req))
			require.Equal(t, 1000+tc.oldAux,
				req.FeeSchedules[0].Budget)
			require.Equal(t, 1000+tc.newAux,
				req.FeeSchedules[1].Budget)

			tp, _ := createTestPublisher(t)
			tp.currentHeight.Store(105)
			capRate, err := req.MaxFeeRateAllowed()
			require.NoError(t, err)
			f, err := tp.initializeFeeFunction(req)
			require.NoError(t, err)

			// Only the old input's ramp has begun, and it commits
			// half of its share of the headroom.
			share := float64(1000+tc.oldAux) / float64(total)
			want := 1000 + float64(capRate-1000)*share/2
			require.InDelta(t, want, float64(f.FeeRate()), 1)
		})
	}
}

// setOnlyAuxSweeper answers queries about a set of inputs, but fails queries
// about a single input.
type setOnlyAuxSweeper struct {
	MockAuxSweeper
}

// ExtraBudgetForInputs attributes 20,000 sats to each input of a set.
func (a *setOnlyAuxSweeper) ExtraBudgetForInputs(
	inputs []input.Input) fn.Result[btcutil.Amount] {

	if len(inputs) == 1 {
		return fn.Err[btcutil.Amount](errDummy)
	}

	return fn.Ok(btcutil.Amount(20_000 * len(inputs)))
}

// TestAuxLookupFailureDefersSweep checks that no sweep is attempted when an
// input's extra budget cannot be determined after the batch's budget has
// counted it. Weighting the input by its own budget alone would apportion its
// extra budget by other inputs' budgets. The inputs remain eligible for the
// next block.
func TestAuxLookupFailureDefersSweep(t *testing.T) {
	t.Parallel()

	aux := &setOnlyAuxSweeper{}
	publisher := &MockBumper{}
	defer publisher.AssertExpectations(t)
	s := New(&UtxoSweeperConfig{
		AuxSweeper: fn.Some[AuxSweeper](aux),
		Publisher:  publisher,
		GenSweepScript: func() fn.Result[lnwallet.AddrWithKey] {
			return fn.Ok(changePkScript)
		},
	})
	s.currentHeight = 105

	params := Params{
		Budget: 1000, Immediate: true,
		DeadlineHeight:  fn.Some(int32(111)),
		StartingFeeRate: fn.Some(chainfee.SatPerKWeight(1000)),
	}
	first := createTestInput(100000, input.WitnessKeyHash)
	second := createTestInput(100000, input.WitnessKeyHash)
	var inputs []SweeperInput
	for _, inp := range []*input.BaseInput{&first, &second} {
		pi := &SweeperInput{
			Input: inp, params: params, DeadlineHeight: 111,
		}
		s.inputs[inp.OutPoint()] = pi
		inputs = append(inputs, *pi)
	}

	// The set's budget counts the extra budget of both inputs.
	set, err := NewBudgetInputSet(inputs, 111, fn.Some[AuxSweeper](aux))
	require.NoError(t, err)
	require.EqualValues(t, 42_000, set.Budget())

	// Attributing it to each input fails, so nothing is broadcast.
	require.ErrorIs(t, s.sweep(set), errDummy)
	eligible := s.updateSweeperInputs()
	for _, inp := range []*input.BaseInput{&first, &second} {
		require.Equal(t, Init, s.inputs[inp.OutPoint()].state)
		require.Contains(t, eligible, inp.OutPoint())
	}
}

// TestScheduleWithoutBaselinePublishesAtCap checks that a schedule whose
// deadline was at hand from its origin carries no starting rate, and still
// publishes at the batch's cap.
func TestScheduleWithoutBaselinePublishesAtCap(t *testing.T) {
	t.Parallel()

	tp, _ := createTestPublisher(t)
	tp.currentHeight.Store(110)
	inp := createTestInput(100000, input.WitnessKeyHash)
	req := &BumpRequest{
		Inputs:          []input.Input{&inp},
		DeliveryAddress: changePkScript,
		Budget:          10000, MaxFeeRate: 2000, DeadlineHeight: 111,
		FeeSchedules: []InputSchedule{{FeeSchedule: FeeSchedule{
			StartHeight:     110,
			StartingFeeRate: fn.None[chainfee.SatPerKWeight](),
		}}},
	}
	f, err := tp.initializeFeeFunction(req)
	require.NoError(t, err)
	require.EqualValues(t, 2000, f.FeeRate())
}

// TestRequestedRateAboveBatchCap checks the gap between per-input admission
// and whole-transaction affordability. An admitted input's requested rate
// must not prevent its batch from publishing at the affordable cap.
func TestRequestedRateAboveBatchCap(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		height  int32
		grouped bool
	}{
		{"alone before deadline", 100, false},
		{"alone at deadline", 111, false},
		{"grouped before deadline", 100, true},
		{"grouped one block left", 110, true},
		{"grouped at deadline", 111, true},
		{"grouped past deadline", 112, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tp, m := createTestPublisher(t)
			tp.cfg.AuxSweeper = fn.None[AuxSweeper]()
			s := New(&UtxoSweeperConfig{})
			s.currentHeight = 100
			inp := createTestInput(100000, input.WitnessKeyHash)
			requested := fn.Some(chainfee.SatPerKWeight(10000))
			pi := &SweeperInput{
				Input: &inp, DeadlineHeight: 111,
				params: Params{
					Budget:          3000,
					StartingFeeRate: requested,
					Immediate:       true,
				},
			}
			s.inputs[inp.OutPoint()] = pi
			s.startFeeSchedule(pi)
			if tc.grouped {
				other := createTestInput(100000,
					input.WitnessKeyHash)
				lowRate := fn.Some(chainfee.SatPerKWeight(250))
				companion := &SweeperInput{
					Input: &other, DeadlineHeight: 111,
					params: Params{
						Budget: 1000, Immediate: true,
						StartingFeeRate: lowRate,
					},
				}
				s.inputs[other.OutPoint()] = companion
				s.startFeeSchedule(companion)
			}
			m.estimator.On("RelayFeePerKW").
				Return(chainfee.SatPerKWeight(250)).Once()
			aggregator := NewBudgetAggregator(
				m.estimator, 10, fn.None[AuxSweeper](),
			)
			sets := aggregator.ClusterInputs(s.inputs)
			require.Len(t, sets, 1)
			set := sets[0]
			require.Len(t, set.Inputs(), len(s.inputs))
			req := &BumpRequest{
				Inputs:          set.Inputs(),
				Budget:          set.Budget(),
				DeadlineHeight:  set.DeadlineHeight(),
				DeliveryAddress: changePkScript,
				MaxFeeRate:      100000,
				Immediate:       true,
			}
			capRate, err := req.MaxFeeRateAllowed()
			require.NoError(t, err)
			require.Less(t, capRate, requested.UnwrapOrFail(t))
			s.currentHeight = tc.height
			tp.currentHeight.Store(tc.height)
			require.NoError(t, s.attachFeeContext(req))
			m.signer.On("ComputeInputScript", mock.Anything,
				mock.Anything).Return(&input.Script{}, nil)
			m.wallet.On("CheckMempoolAcceptance", mock.Anything).
				Return(nil).Once()
			m.wallet.On("PublishTransaction", mock.Anything,
				mock.Anything).Return(nil).Once()
			result := <-tp.Broadcast(req)
			require.NoError(t, result.Err)
			require.Equal(t, TxPublished, result.Event)
			require.Equal(t, capRate, result.FeeRate)
			require.LessOrEqual(t, result.Fee, req.Budget)
			require.Len(t, result.Tx.TxIn, len(s.inputs))
			require.Equal(t, requested, pi.params.StartingFeeRate)
			require.Equal(t, requested,
				pi.feeSchedule.StartingFeeRate)
		})
	}
}

// TestRegroupedSurvivorRemainsEligible exercises the affordability filter and
// publisher after a high-budget input in a shared transaction is spent.
func TestRegroupedSurvivorRemainsEligible(t *testing.T) {
	t.Parallel()

	tp, m := createTestPublisher(t)
	store := NewMockSweeperStore()
	defer store.AssertExpectations(t)
	s := New(&UtxoSweeperConfig{Store: store})
	s.currentHeight = 100
	for _, budget := range []int64{1000, 10000} {
		inp := createTestInput(100000, input.WitnessKeyHash)
		pi := &SweeperInput{
			Input: &inp, state: PendingPublish, DeadlineHeight: 111,
			params: Params{
				Budget: btcutil.Amount(budget), Immediate: true,
				StartingFeeRate: fn.Some(
					chainfee.SatPerKWeight(250),
				),
			},
		}
		s.inputs[inp.OutPoint()] = pi
		s.startFeeSchedule(pi)
	}
	m.estimator.On("RelayFeePerKW").Return(chainfee.SatPerKWeight(250))
	aggregator := NewBudgetAggregator(
		m.estimator, 10, fn.None[AuxSweeper](),
	)
	sets := aggregator.ClusterInputs(s.inputs)
	require.Len(t, sets, 1)
	record := &TxRecord{FeeRate: 10000, Fee: 7000}
	store.On("StoreTx", record).Return(nil).Once()
	require.NoError(t, s.markInputsPublished(record, sets[0]))

	// Resolve the high-budget input's conflicting spend. The survivor's
	// requested rate and original schedule must still be usable alone.
	for op, pi := range s.inputs {
		if pi.params.Budget == 10000 {
			s.invalidateFeeContext(&wire.MsgTx{TxIn: []*wire.TxIn{
				{PreviousOutPoint: op},
			}})
			delete(s.inputs, op)
		}
	}
	s.markInputsPublishFailed(sets[0])
	sets = aggregator.ClusterInputs(s.inputs)
	require.Len(t, sets, 1)
	require.Len(t, sets[0].Inputs(), 1)
	s.currentHeight = 105
	tp.currentHeight.Store(105)
	req := &BumpRequest{
		Inputs: sets[0].Inputs(), Budget: sets[0].Budget(),
		DeadlineHeight:  sets[0].DeadlineHeight(),
		DeliveryAddress: changePkScript, MaxFeeRate: 100000,
	}
	require.NoError(t, s.attachFeeContext(req))
	require.Empty(t, req.PreviousTxs)
	require.EqualValues(t, 100, req.FeeSchedules[0].StartHeight)
	require.EqualValues(t, 250,
		req.FeeSchedules[0].StartingFeeRate.UnwrapOrFail(t))
	m.signer.On("ComputeInputScript", mock.Anything, mock.Anything).
		Return(&input.Script{}, nil)
	m.wallet.On("CheckMempoolAcceptance", mock.Anything).Return(nil).Once()
	candidate, err := tp.initializeTx(&monitorRecord{req: req})
	require.NoError(t, err)
	require.LessOrEqual(t, candidate.fee, req.Budget)
	require.Greater(t, candidate.feeRate, chainfee.SatPerKWeight(250))
	require.Less(t, candidate.feeRate, chainfee.SatPerKWeight(10000))
}

func TestScheduleEstimateCached(t *testing.T) {
	t.Parallel()

	estimator := &chainfee.MockEstimator{}
	defer estimator.AssertExpectations(t)
	estimator.On("EstimateFeePerKW", uint32(11)).Return(
		chainfee.SatPerKWeight(2000), nil).Once()
	estimator.On("RelayFeePerKW").Return(
		chainfee.SatPerKWeight(250)).Once()
	s := New(&UtxoSweeperConfig{FeeEstimator: estimator})
	s.currentHeight = 100
	inp := createTestInput(100000, input.WitnessKeyHash)
	pi := &SweeperInput{
		Input: &inp, DeadlineHeight: 111,
		params: Params{Immediate: true},
	}
	s.inputs[inp.OutPoint()] = pi
	for _, height := range []int32{100, 105, 109} {
		s.currentHeight = height
		req := &BumpRequest{Inputs: []input.Input{&inp}}
		require.NoError(t, s.attachFeeContext(req))
		require.EqualValues(t, 100, req.FeeSchedules[0].StartHeight)
		require.EqualValues(t, 2000,
			req.FeeSchedules[0].StartingFeeRate.UnwrapOrFail(t))
		require.True(t, pi.params.StartingFeeRate.IsNone())
	}
}

// TestRestartContinuesPublishedSchedule checks every block of the recovered
// ramp, including deferred publication, regrouping, and a lower batch cap.
func TestRestartContinuesPublishedSchedule(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		requested fn.Option[chainfee.SatPerKWeight]
		immediate bool
		capRate   chainfee.SatPerKWeight
		seed      chainfee.SatPerKWeight
	}{
		{
			name:      "requested below publication",
			requested: fn.Some(chainfee.SatPerKWeight(1000)),
			immediate: true, capRate: 10000, seed: 8200,
		},
		{
			name:      "automatic rate",
			immediate: true, capRate: 10000, seed: 8200,
		},
		{
			name:    "deferred publication",
			capRate: 10000, seed: 8200,
		},
		{
			name:      "higher requested rate",
			requested: fn.Some(chainfee.SatPerKWeight(9000)),
			immediate: true, capRate: 10000, seed: 9000,
		},
		{
			name:      "publication above batch cap",
			requested: fn.Some(chainfee.SatPerKWeight(1000)),
			immediate: true, capRate: 6000, seed: 8200,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tp, _ := createTestPublisher(t)
			store := NewMockSweeperStore()
			defer store.AssertExpectations(t)
			mempool := chainntnfs.NewMockMempoolWatcher()
			defer mempool.AssertExpectations(t)
			notifier := &chainntnfs.MockChainNotifier{}
			defer notifier.AssertExpectations(t)
			s := New(&UtxoSweeperConfig{
				Store:    store,
				Mempool:  mempool,
				Notifier: notifier,
			})
			s.currentHeight = 180
			inp := createTestInput(100000, input.WitnessKeyHash)
			op := inp.OutPoint()
			tx := wire.MsgTx{Version: 2, TxIn: []*wire.TxIn{
				{PreviousOutPoint: op},
			}}
			mempool.On("LookupInputMempoolSpend", op).
				Return(fn.Some(tx)).Once()
			store.On("GetTx", tx.TxHash()).Return(&TxRecord{
				Txid: tx.TxHash(), Fee: 1000, FeeRate: 8200,
				Published: true,
			}, nil).Once()
			notifier.On("RegisterSpendNtfn", &op, mock.Anything,
				mock.Anything).Return(&chainntnfs.SpendEvent{
				Spend:  make(chan *chainntnfs.SpendDetail),
				Cancel: func() {},
			}, nil).Once()
			params := Params{
				StartingFeeRate: tc.requested,
				DeadlineHeight:  fn.Some(int32(201)),
				Immediate:       tc.immediate,
			}
			require.NoError(t, s.handleNewInput(&sweepInputMessage{
				input: &inp, params: params,
				resultChan: make(chan Result, 1),
			}))
			pi := s.inputs[op]
			require.Equal(t, params, pi.params)
			require.EqualValues(t, 8200, pi.lastFeeRate)

			var active FeeFunction
			for height := int32(180); height <= 200; height++ {
				s.currentHeight = height
				tp.currentHeight.Store(height)
				req := &BumpRequest{
					Inputs:          []input.Input{&inp},
					Budget:          10000,
					DeliveryAddress: changePkScript,
					DeadlineHeight:  201,
					MaxFeeRate:      tc.capRate,
				}
				require.NoError(t, s.attachFeeContext(req))
				require.EqualValues(t, 180,
					req.FeeSchedules[0].StartHeight)
				require.Equal(t, tc.seed,
					req.FeeSchedules[0].StartingFeeRate.
						UnwrapOrFail(t))
				require.EqualValues(t, 8200,
					req.PreviousTxs[0].FeeRate)

				// Retries must follow the same continuation as
				// an uninterrupted publisher.
				f, err := tp.initializeFeeFunction(req)
				require.NoError(t, err)
				if active == nil {
					active = f
				} else {
					_, err = active.IncreaseFeeRate(
						uint32(201 - height),
					)
					if err != nil {
						require.ErrorIs(t, err,
							ErrMaxPosition)
						require.Equal(t, tc.capRate,
							active.FeeRate())
					}
				}
				start := min(tc.seed, tc.capRate)
				want := start + (tc.capRate-start)*
					chainfee.SatPerKWeight(height-180)/20
				require.Equal(t, want, f.FeeRate(),
					"reconstructed at height %d", height)
				require.Equal(t, want, active.FeeRate(),
					"uninterrupted at height %d", height)
				require.Equal(t, params, pi.params)
			}
		})
	}
}

func TestScheduleOriginAccountsForMaturity(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		csv       uint32
		locktime  uint32
		immediate bool
		want      int32
	}{
		{"deferred", 0, 0, false, 101},
		{"immediate", 0, 0, true, 100},
		{"csv", 10, 0, false, 109},
		{"cltv", 0, 120, false, 120},
		{"both", 10, 120, true, 120},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inp := &input.MockInput{}
			defer inp.AssertExpectations(t)
			inp.On("RequiredLockTime").
				Return(tc.locktime, true).Once()
			inp.On("HeightHint").Return(uint32(100)).Once()
			inp.On("BlocksToMaturity").Return(tc.csv).Once()
			s := New(&UtxoSweeperConfig{})
			s.currentHeight = 100
			pi := &SweeperInput{
				Input:  inp,
				params: Params{Immediate: tc.immediate},
			}
			s.startFeeSchedule(pi)
			require.Equal(t, tc.want, pi.feeSchedule.StartHeight)
			s.currentHeight = tc.want
			s.startFeeSchedule(pi)
			require.Equal(t, tc.want, pi.feeSchedule.StartHeight)
		})
	}
}
