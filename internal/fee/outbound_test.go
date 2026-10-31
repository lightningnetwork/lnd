package fee

import (
	"math"
	"math/big"
	"testing"

	"pgregory.net/rapid"
)

func TestOutboundOracle(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		amount := rapid.Uint64().Draw(t, "amount")
		base := rapid.Uint64().Draw(t, "base")
		rate := rapid.Uint64().Draw(t, "rate")

		want := new(big.Int).Mul(
			new(big.Int).SetUint64(amount),
			new(big.Int).SetUint64(rate),
		)
		want.Quo(want, big.NewInt(1_000_000))
		want.Add(want, new(big.Int).SetUint64(base))
		got, err := Outbound(amount, base, rate).Unpack()
		if !want.IsUint64() {
			if err == nil {
				t.Fatalf("expected overflow, got %d", got)
			}

			return
		}
		if err != nil || got != want.Uint64() {
			t.Fatalf("got %d, %v, want %s", got, err, want)
		}
	})
}

func TestOutboundBoundaries(t *testing.T) {
	const threshold uint64 = math.MaxUint64 / math.MaxUint32
	amounts := []uint64{threshold - 1, threshold, threshold + 1}
	for _, amount := range amounts {
		want := new(big.Int).Mul(
			new(big.Int).SetUint64(amount),
			new(big.Int).SetUint64(math.MaxUint32),
		)
		want.Quo(want, big.NewInt(1_000_000))
		got, err := Outbound(amount, 0, math.MaxUint32).Unpack()
		if err != nil || got != want.Uint64() {
			t.Fatalf(
				"amount %d: got %d, %v, want %s",
				amount, got, err, want,
			)
		}
	}

	_, err := Outbound(1_000_000, math.MaxUint64, 1).Unpack()
	if err == nil {
		t.Fatal("expected base addition overflow")
	}
	_, err = Outbound(math.MaxUint64, 0, math.MaxUint64).Unpack()
	if err == nil {
		t.Fatal("expected proportional overflow")
	}
}
