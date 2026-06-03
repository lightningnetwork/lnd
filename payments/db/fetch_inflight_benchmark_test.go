//go:build (test_db_sqlite && !test_db_postgres) || (test_db_postgres && !test_db_sqlite)

package paymentsdb

import (
	"database/sql"
	"encoding/binary"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/sqldb/sqlc"
)

// fetchInFlightBenchScenario identifies one payment-history shape benchmarked
// by BenchmarkFetchInFlightPayments.
type fetchInFlightBenchScenario string

// fetchInFlightBenchPaymentState is the seeded lifecycle state of a synthetic
// payment row.
type fetchInFlightBenchPaymentState uint8

// fetchInFlightBenchCase describes one benchmark scenario and its seeded data
// distribution.
type fetchInFlightBenchCase struct {
	scenario    fetchInFlightBenchScenario
	description string
}

// fetchInFlightBenchSeedStats describes the concrete row counts seeded for a
// benchmark scenario.
type fetchInFlightBenchSeedStats struct {
	retryable       int
	unresolved      int
	terminalFailed  int
	terminalSettled int
	attempts        int
	expected        int
}

// fetchInFlightBenchBackend creates a SQLStore for one benchmark backend.
type fetchInFlightBenchBackend interface {
	newStore(*testing.B, fetchInFlightBenchScenario,
		int) (*SQLStore, fetchInFlightBenchSeedStats)
}

// Untyped scenario constants. Functions accept fetchInFlightBenchScenario; Go
// performs the implicit conversion at the call/case site, so we keep the type
// discipline on the function signatures while letting the const block fit
// within the 80-column line limit.
const (
	benchScenarioAllNonTerminal        = "all-non-terminal"
	benchScenarioTerminalFailed        = "terminal-failed"
	benchScenarioTerminalSettled       = "terminal-settled"
	benchScenarioNonTerminalEarly20Pct = "non-terminal-early-20pct"
	benchScenarioNonTerminalLate20Pct  = "non-terminal-late-20pct"
	benchScenarioMixedShuffled         = "mixed-shuffled-10f-10n-80s"
)

const (
	benchPaymentStateRetryable fetchInFlightBenchPaymentState = iota
	benchPaymentStateUnresolved
	benchPaymentStateTerminalFailed
	benchPaymentStateTerminalSettled
)

// Per-scenario setup descriptions, kept at package level so the prose fits
// within the line-length limit without needing extra wrapping inside the
// nested struct literal returned by fetchInFlightBenchCases.
const (
	benchDescAllNonTerminal = "every payment is non-terminal. " +
		"The rows alternate between retryable failed-only " +
		"payments, which exercise the fail_reason=NULL/no " +
		"settled-attempt predicate, and active payments with " +
		"one settled attempt plus one unresolved attempt, " +
		"which exercise the unresolved-attempt predicate."

	benchDescTerminalFailed = "every payment has " +
		"fail_reason=FailureReasonNoRoute and one failed HTLC " +
		"attempt resolution. This is a terminal failed history: " +
		"no payments should be returned. It verifies the " +
		"payment-driven query does not catastrophically regress " +
		"when both non-terminal predicate branches are empty; " +
		"small per-row overhead is expected."

	benchDescTerminalSettled = "every payment has fail_reason=NULL " +
		"and one settled HTLC attempt resolution with a preimage. " +
		"This is a terminal settled history: no payments should " +
		"be returned."

	benchDescNonTerminalEarly20Pct = "the first 20 percent of " +
		"payment ids are non-terminal payments split between " +
		"retryable failed-only rows and active unresolved-attempt " +
		"rows. The remaining 80 percent are settled terminal " +
		"payments. This measures finding non-terminal payments " +
		"early in the payments table."

	benchDescNonTerminalLate20Pct = "the first 80 percent of " +
		"payment ids are settled terminal payments, and the final " +
		"20 percent are non-terminal payments split between " +
		"retryable failed-only rows and active unresolved-attempt " +
		"rows. This measures the likely startup case where recent " +
		"payments near the end of the table still need resumption."

	benchDescMixedShuffled = "payments are deterministically " +
		"shuffled so 10 percent are terminal failed, 10 percent " +
		"are non-terminal split between retryable failed-only " +
		"rows and active unresolved-attempt rows, and 80 percent " +
		"are terminal settled. This is the more realistic " +
		"mixed-history case."
)

// BenchmarkFetchInFlightPayments exercises SQLStore.FetchInFlightPayments
// end to end against the selected SQL backend. The setup inserts directly
// into the SQL tables so larger synthetic histories can be created without
// benchmarking payment lifecycle API setup cost.
//
// The default size is intentionally modest. For larger manual runs, set:
//
//	PAYMENT_BENCH_SIZE=250000
//
// Note that scenarios returning many payments include full MPPayment
// reconstruction and allocation costs, not just the SQL selector cost.
func BenchmarkFetchInFlightPayments(b *testing.B) {
	b.ReportAllocs()

	backend := newFetchInFlightBenchBackend(b)
	sizes := fetchInFlightBenchSizes(b)
	benchCases := fetchInFlightBenchCases()

	for _, size := range sizes {
		for _, benchCase := range benchCases {
			name := fmt.Sprintf(
				"%s/%d", benchCase.scenario, size,
			)
			b.Run(name, func(b *testing.B) {
				runFetchInFlightBench(
					b, backend, benchCase, size,
				)
			})
		}
	}
}

// runFetchInFlightBench seeds the configured scenario and exercises
// FetchInFlightPayments for the active sub-benchmark.
func runFetchInFlightBench(b *testing.B,
	backend fetchInFlightBenchBackend,
	benchCase fetchInFlightBenchCase, size int) {

	b.ReportAllocs()

	b.Logf("setup: %s", benchCase.description)

	store, stats := backend.newStore(b, benchCase.scenario, size)
	b.Logf("seeded: %s", stats)

	ctx := b.Context()

	for b.Loop() {
		payments, err := store.FetchInFlightPayments(ctx)
		if err != nil {
			b.Fatalf("fetch in-flight: %v", err)
		}

		if len(payments) != stats.expected {
			b.Fatalf("expected %d payments, got %d",
				stats.expected, len(payments))
		}
	}
}

// String returns a compact human-readable summary of the seeded benchmark row
// counts.
func (s fetchInFlightBenchSeedStats) String() string {
	return fmt.Sprintf(
		"%d retryable, %d unresolved, %d terminal failed, "+
			"%d terminal settled, %d attempts total, "+
			"%d payments expected",
		s.retryable, s.unresolved, s.terminalFailed,
		s.terminalSettled, s.attempts, s.expected,
	)
}

// fetchInFlightBenchCases returns the benchmark scenarios with explicit setup
// descriptions for each synthetic payment-history distribution.
func fetchInFlightBenchCases() []fetchInFlightBenchCase {
	return []fetchInFlightBenchCase{
		{
			scenario:    benchScenarioAllNonTerminal,
			description: benchDescAllNonTerminal,
		},
		{
			scenario:    benchScenarioTerminalFailed,
			description: benchDescTerminalFailed,
		},
		{
			scenario:    benchScenarioTerminalSettled,
			description: benchDescTerminalSettled,
		},
		{
			scenario:    benchScenarioNonTerminalEarly20Pct,
			description: benchDescNonTerminalEarly20Pct,
		},
		{
			scenario:    benchScenarioNonTerminalLate20Pct,
			description: benchDescNonTerminalLate20Pct,
		},
		{
			scenario:    benchScenarioMixedShuffled,
			description: benchDescMixedShuffled,
		},
	}
}

// fetchInFlightBenchSizes returns the configured payment counts for each
// benchmark scenario.
func fetchInFlightBenchSizes(b *testing.B) []int {
	b.Helper()

	sizeEnv := strings.TrimSpace(os.Getenv("PAYMENT_BENCH_SIZE"))
	if sizeEnv == "" {
		return []int{1_000}
	}

	parts := strings.Split(sizeEnv, ",")
	sizes := make([]int, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		size, err := strconv.Atoi(part)
		if err != nil || size <= 0 {
			b.Fatalf("invalid PAYMENT_BENCH_SIZE %q", part)
		}

		sizes = append(sizes, size)
	}

	if len(sizes) == 0 {
		b.Fatalf("PAYMENT_BENCH_SIZE did not contain any sizes")
	}

	return sizes
}

// seedFetchInFlightBenchDB inserts synthetic payments, HTLC attempts,
// resolutions, and route hops using the generated SQL queries.
func seedFetchInFlightBenchDB(b *testing.B, db *sql.DB,
	withTx func(*sql.Tx) SQLQueries, scenario fetchInFlightBenchScenario,
	totalPayments int) fetchInFlightBenchSeedStats {

	b.Helper()

	createdAt := time.Unix(1_700_000_000, 0).UTC()
	pubKey := vertex[:]
	ctx := b.Context()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		b.Fatalf("begin seed tx: %v", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	queries := withTx(tx)
	var stats fetchInFlightBenchSeedStats

	insertAttemptAndHop := func(paymentID, attemptIndex int,
		paymentHash []byte) {

		attemptIndex64 := int64(attemptIndex)
		_, err := queries.InsertHtlcAttempt(
			ctx, sqlc.InsertHtlcAttemptParams{
				PaymentID:          int64(paymentID),
				AttemptIndex:       attemptIndex64,
				SessionKey:         benchIDBytes(totalPayments + attemptIndex),
				AttemptTime:        createdAt,
				PaymentHash:        paymentHash,
				FirstHopAmountMsat: int64(1000),
				RouteTotalTimeLock: int32(40),
				RouteTotalAmount:   int64(1000),
				RouteSourceKey:     pubKey,
			},
		)
		if err != nil {
			b.Fatalf("insert attempt %d: %v", attemptIndex, err)
		}

		_, err = queries.InsertRouteHop(ctx, sqlc.InsertRouteHopParams{
			HtlcAttemptIndex: attemptIndex64,
			HopIndex:         int32(0),
			PubKey:           pubKey,
			Scid:             strconv.Itoa(attemptIndex),
			OutgoingTimeLock: int32(40),
			AmtToForward:     int64(1000),
		})
		if err != nil {
			b.Fatalf("insert hop %d: %v", attemptIndex, err)
		}

		stats.attempts++
	}

	settleAttempt := func(attemptIndex int, settlePreimage []byte) {
		attemptIndex64 := int64(attemptIndex)
		err := queries.SettleAttempt(ctx, sqlc.SettleAttemptParams{
			AttemptIndex:   attemptIndex64,
			ResolutionTime: createdAt,
			ResolutionType: int32(HTLCAttemptResolutionSettled),
			SettlePreimage: settlePreimage,
		})
		if err != nil {
			b.Fatalf("settle attempt %d: %v", attemptIndex, err)
		}
	}

	failAttempt := func(attemptIndex int) {
		attemptIndex64 := int64(attemptIndex)
		err := queries.FailAttempt(ctx, sqlc.FailAttemptParams{
			AttemptIndex:   attemptIndex64,
			ResolutionTime: createdAt,
			ResolutionType: int32(HTLCAttemptResolutionFailed),
		})
		if err != nil {
			b.Fatalf("fail attempt %d: %v", attemptIndex, err)
		}
	}

	for id := 1; id <= totalPayments; id++ {
		paymentState := benchPaymentState(scenario, id, totalPayments)
		identifier := benchIDBytes(id)
		paymentID, err := queries.InsertPayment(
			ctx, sqlc.InsertPaymentParams{
				AmountMsat:        int64(2000),
				CreatedAt:         createdAt,
				PaymentIdentifier: identifier,
			},
		)
		if err != nil {
			b.Fatalf("insert payment %d: %v", id, err)
		}

		insertAttemptAndHop(int(paymentID), id, identifier)

		switch paymentState {
		case benchPaymentStateRetryable:
			stats.retryable++
			stats.expected++
			failAttempt(id)

		case benchPaymentStateTerminalFailed:
			stats.terminalFailed++
			_, err := queries.FailPayment(ctx, sqlc.FailPaymentParams{
				FailReason: sql.NullInt32{
					Valid: true,
					Int32: int32(FailureReasonNoRoute),
				},
				PaymentIdentifier: identifier,
			})
			if err != nil {
				b.Fatalf("fail payment %d: %v", id, err)
			}
			failAttempt(id)

		case benchPaymentStateUnresolved:
			stats.unresolved++
			stats.expected++
			settleAttempt(id, benchIDBytes(2*totalPayments+id))

			insertAttemptAndHop(
				int(paymentID), totalPayments+id, identifier,
			)

		case benchPaymentStateTerminalSettled:
			stats.terminalSettled++
			settleAttempt(id, benchIDBytes(2*totalPayments+id))
		}
	}

	if err := tx.Commit(); err != nil {
		b.Fatalf("commit seed tx: %v", err)
	}

	return stats
}

// benchPaymentState returns the seeded lifecycle state for a payment in the
// given benchmark scenario.
func benchPaymentState(scenario fetchInFlightBenchScenario,
	id, totalPayments int) fetchInFlightBenchPaymentState {

	switch scenario {
	case benchScenarioAllNonTerminal:
		return benchNonTerminalPaymentState(id)

	case benchScenarioTerminalFailed:
		return benchPaymentStateTerminalFailed

	case benchScenarioTerminalSettled:
		return benchPaymentStateTerminalSettled

	case benchScenarioNonTerminalEarly20Pct:
		if id <= totalPayments/5 {
			return benchNonTerminalPaymentState(id)
		}

		return benchPaymentStateTerminalSettled

	case benchScenarioNonTerminalLate20Pct:
		if id > totalPayments-totalPayments/5 {
			return benchNonTerminalPaymentState(id)
		}

		return benchPaymentStateTerminalSettled

	case benchScenarioMixedShuffled:
		return benchMixedShuffledPaymentState(id, totalPayments)

	default:
		return benchPaymentStateTerminalSettled
	}
}

// benchMixedShuffledPaymentState returns a deterministic mixed distribution of
// failed, non-terminal, and settled payments.
func benchMixedShuffledPaymentState(id,
	totalPayments int) fetchInFlightBenchPaymentState {

	failedPayments := totalPayments / 10
	nonTerminalPayments := totalPayments / 10
	rank := benchPermutedRank(id, totalPayments)

	if rank < failedPayments {
		return benchPaymentStateTerminalFailed
	}

	if rank < failedPayments+nonTerminalPayments {
		return benchNonTerminalPaymentState(rank - failedPayments)
	}

	return benchPaymentStateTerminalSettled
}

// benchNonTerminalPaymentState alternates non-terminal rows between the two SQL
// predicate branches that FetchNonTerminalPayments must include.
func benchNonTerminalPaymentState(index int) fetchInFlightBenchPaymentState {
	if index%2 == 0 {
		return benchPaymentStateRetryable
	}

	return benchPaymentStateUnresolved
}

// benchPermutedRank maps a payment id to a deterministic pseudo-random rank in
// the benchmark table.
func benchPermutedRank(id, totalPayments int) int {
	return ((id - 1) * benchCoprimeStep(totalPayments)) % totalPayments
}

// benchCoprimeStep returns a deterministic step that is coprime with the total
// payment count so benchPermutedRank visits each rank exactly once.
func benchCoprimeStep(totalPayments int) int {
	for _, step := range []int{
		7919, 5807, 4093, 2053, 1021, 521, 251, 127, 61, 31, 17,
		7, 5, 3,
	} {
		if benchGCD(step, totalPayments) == 1 {
			return step
		}
	}

	return 1
}

// benchGCD returns the greatest common divisor of two integers.
func benchGCD(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}

	if a < 0 {
		return -a
	}

	return a
}

// benchIDBytes returns a stable 32-byte identifier for a seeded payment value.
func benchIDBytes(id int) []byte {
	var value [32]byte
	binary.BigEndian.PutUint64(value[24:], uint64(id))

	return value[:]
}
