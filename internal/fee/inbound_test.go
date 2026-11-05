package fee

import (
	"math"
	"math/big"
	"testing"

	"pgregory.net/rapid"
)

func TestInboundOracle(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		amount := rapid.Uint64().Draw(t, "amount")
		base := rapid.Int32().Draw(t, "base")
		rate := rapid.Int32().Draw(t, "rate")
		r := int64(rate)
		if r > maxInboundRate {
			r = maxInboundRate
		} else if r < -maxInboundRate {
			r = -maxInboundRate
		}

		want := new(big.Int).Mul(
			new(big.Int).SetUint64(amount), big.NewInt(r),
		)
		want.Quo(want, big.NewInt(1_000_000))
		want.Add(want, big.NewInt(int64(base)))
		got, err := Inbound(amount, base, rate).Unpack()
		if !want.IsInt64() {
			if err == nil {
				t.Fatalf("expected overflow, got %d", got)
			}

			return
		}
		if err != nil || got != want.Int64() {
			t.Fatalf("got %d, %v, want %s", got, err, want)
		}
	})
}

func TestInboundBoundaries(t *testing.T) {
	const threshold uint64 = math.MaxInt64 / 10_000_000
	amounts := []uint64{threshold - 1, threshold, threshold + 1}
	for _, amount := range amounts {
		for _, rate := range []int32{-10_000_000, 10_000_000} {
			got, err := Inbound(amount, 3, rate).Unpack()
			want := int64(amount) * 10
			if rate < 0 {
				want = -want
			}
			want += 3
			if err != nil || got != want {
				t.Fatalf(
					"amount %d rate %d: got %d, %v, "+
						"want %d",
					amount, rate, got, err, want,
				)
			}
		}
	}
	rates := []int32{math.MinInt32, -10_000_001, 10_000_001, math.MaxInt32}
	for _, rate := range rates {
		got, err := Inbound(100, 0, rate).Unpack()
		want := int64(1000)
		if rate < 0 {
			want = -want
		}
		if err != nil || got != want {
			t.Fatalf(
				"rate %d: got %d, %v, want %d",
				rate, got, err, want,
			)
		}
	}
	_, err := Inbound(math.MaxUint64, 0, 10_000_000).Unpack()
	if err == nil {
		t.Fatal("expected inbound overflow")
	}
}

func TestCombinedOracle(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		out := rapid.Uint64().Draw(t, "out")
		in := rapid.Int64().Draw(t, "in")
		want := new(big.Int).Add(
			new(big.Int).SetUint64(out), big.NewInt(in),
		)
		got, err := Combined(out, in).Unpack()
		if !want.IsInt64() {
			if err == nil {
				t.Fatalf("expected overflow, got %d", got)
			}

			return
		}
		if err != nil || got != want.Int64() {
			t.Fatalf("got %d, %v, want %s", got, err, want)
		}
	})
	got, err := Combined(uint64(math.MaxInt64)+1, math.MinInt64).Unpack()
	if err != nil || got != 0 {
		t.Fatalf("cancellation: got %d, %v", got, err)
	}
}

func TestAddAmountOracle(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		amount := rapid.Uint64().Draw(t, "amount")
		delta := rapid.Int64().Draw(t, "delta")
		want := new(big.Int).Add(
			new(big.Int).SetUint64(amount), big.NewInt(delta),
		)
		got, err := AddAmount(amount, delta).Unpack()
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
