package fn

import (
	"fmt"
	"math/rand"
	"slices"
	"testing"
	"testing/quick"
	"time"

	"github.com/stretchr/testify/require"
)

func even(a int) bool { return a%2 == 0 }
func odd(a int) bool  { return a%2 != 0 }

func TestAll(t *testing.T) {
	x := []int{0, 2, 4, 6, 8}
	require.True(t, All(x, even))
	require.False(t, All(x, odd))

	y := []int{1, 3, 5, 7, 9}
	require.False(t, All(y, even))
	require.True(t, All(y, odd))

	z := []int{0, 2, 4, 6, 9}
	require.False(t, All(z, even))
	require.False(t, All(z, odd))
}

func TestAny(t *testing.T) {
	x := []int{1, 3, 5, 7, 9}
	require.False(t, Any(x, even))
	require.True(t, Any(x, odd))

	y := []int{0, 3, 5, 7, 9}
	require.True(t, Any(y, even))
	require.True(t, Any(y, odd))

	z := []int{0, 2, 4, 6, 8}
	require.True(t, Any(z, even))
	require.False(t, Any(z, odd))
}

func TestMap(t *testing.T) {
	inc := func(i int) int { return i + 1 }

	x := []int{0, 2, 4, 6, 8}

	y := Map(x, inc)

	z := []int{1, 3, 5, 7, 9}

	require.True(t, slices.Equal(y, z))
}

func TestFilter(t *testing.T) {
	x := []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}

	y := Filter(x, even)

	require.True(t, All(y, even))

	z := Filter(y, odd)

	require.Zero(t, len(z))
}

func TestFoldl(t *testing.T) {
	seed := []int{}
	stupid := func(s []int, a int) []int { return append(s, a) }

	x := []int{0, 1, 2, 3, 4}

	r := Foldl(seed, x, stupid)

	require.True(t, slices.Equal(x, r))
}

func TestFoldr(t *testing.T) {
	seed := []int{}
	stupid := func(a int, s []int) []int { return append(s, a) }

	x := []int{0, 1, 2, 3, 4}

	z := Foldr(seed, x, stupid)

	slices.Reverse[[]int](x)

	require.True(t, slices.Equal(x, z))
}

func TestFind(t *testing.T) {
	x := []int{10, 11, 12, 13, 14, 15}

	div3 := func(a int) bool { return a%3 == 0 }
	div8 := func(a int) bool { return a%8 == 0 }

	require.Equal(t, Find(x, div3), Some(12))

	require.Equal(t, Find(x, div8), None[int]())
}

func TestFlatten(t *testing.T) {
	x := [][]int{{0}, {1}, {2}}

	y := Flatten(x)

	require.True(t, slices.Equal(y, []int{0, 1, 2}))
}

func TestReplicate(t *testing.T) {
	require.True(t, slices.Equal([]int{1, 1, 1, 1, 1}, Replicate(5, 1)))
}

func TestSpan(t *testing.T) {
	x := []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}
	lt5 := func(a int) bool { return a < 5 }

	low, high := Span(x, lt5)

	require.True(t, slices.Equal(low, []int{0, 1, 2, 3, 4}))
	require.True(t, slices.Equal(high, []int{5, 6, 7, 8, 9}))
}

func TestSplitAt(t *testing.T) {
	x := []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}
	fst, snd := SplitAt(5, x)

	require.True(t, slices.Equal(fst, []int{0, 1, 2, 3, 4}))
	require.True(t, slices.Equal(snd, []int{5, 6, 7, 8, 9}))
}

func TestZipWith(t *testing.T) {
	eq := func(a, b int) bool { return a == b }
	x := []int{0, 1, 2, 3, 4}
	y := Replicate(5, 1)
	z := ZipWith(x, y, eq)
	require.True(t, slices.Equal(
		z, []bool{false, true, false, false, false},
	))
}

// TestSum checks if the Sum function correctly calculates the sum of the
// numbers in the slice.
func TestSum(t *testing.T) {
	tests := []struct {
		name   string
		items  interface{}
		result interface{}
	}{
		{
			name:   "Sum of positive integers",
			items:  []int{1, 2, 3},
			result: 6,
		},
		{
			name:   "Sum of negative integers",
			items:  []int{-1, -2, -3},
			result: -6,
		},
		{
			name:   "Sum of float numbers",
			items:  []float64{1.1, 2.2, 3.3},
			result: 6.6,
		},
		{
			name: "Sum of complex numbers",
			items: []complex128{
				complex(1, 1),
				complex(2, 2),
				complex(3, 3),
			},
			result: complex(6, 6),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			switch v := tt.items.(type) {
			case []int:
				require.Equal(t, tt.result, Sum(v))
			case []float64:
				require.Equal(t, tt.result, Sum(v))
			case []complex128:
				require.Equal(t, tt.result, Sum(v))
			}
		})
	}
}

// TestSliceToMap tests the SliceToMap function.
func TestSliceToMap(t *testing.T) {
	tests := []struct {
		name      string
		slice     []int
		keyFunc   func(int) int
		valueFunc func(int) string
		expected  map[int]string
	}{
		{
			name:    "Integers to string map",
			slice:   []int{1, 2, 3},
			keyFunc: func(a int) int { return a },
			valueFunc: func(a int) string {
				return fmt.Sprintf("Value%d", a)
			},
			expected: map[int]string{
				1: "Value1",
				2: "Value2",
				3: "Value3",
			},
		},
		{
			name:    "Duplicates in slice",
			slice:   []int{1, 2, 2, 3},
			keyFunc: func(a int) int { return a },
			valueFunc: func(a int) string {
				return fmt.Sprintf("Value%d", a)
			},
			expected: map[int]string{
				1: "Value1",
				2: "Value2",
				3: "Value3",
			},
		},
		{
			name:    "Empty slice",
			slice:   []int{},
			keyFunc: func(a int) int { return a },
			valueFunc: func(a int) string {
				return fmt.Sprintf("Value%d", a)
			},
			expected: map[int]string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(
				t, tt.expected,
				SliceToMap(tt.slice, tt.keyFunc, tt.valueFunc),
			)
		})
	}
}

// TestHasDuplicates tests the HasDuplicates function.
func TestHasDuplicates(t *testing.T) {
	// Define test cases.
	testCases := []struct {
		name  string
		items []int
		want  bool
	}{
		{
			name:  "All unique",
			items: []int{1, 2, 3, 4, 5},
			want:  false,
		},
		{
			name:  "Some duplicates",
			items: []int{1, 2, 2, 3, 4},
			want:  true,
		},
		{
			name:  "No items",
			items: []int{},
			want:  false,
		},
		{
			name:  "All duplicates",
			items: []int{1, 1, 1, 1},
			want:  true,
		},
	}

	// Execute each test case.
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := HasDuplicates(tc.items)

			require.Equal(t, tc.want, got)
		})
	}
}

// TestPropForEachConcMapIsomorphism ensures the property that ForEachConc and
// Map always yield the same results.
func TestPropForEachConcMapIsomorphism(t *testing.T) {
	f := func(incSize int, s []int) bool {
		inc := func(i int) int { return i + incSize }
		mapped := Map(s, inc)
		conc := ForEachConc(s, inc)

		return slices.Equal(mapped, conc)
	}
	if err := quick.Check(f, nil); err != nil {
		t.Fatal(err)
	}
}

func TestPropFindIdxFindIdentity(t *testing.T) {
	f := func(div, mod uint8, s []uint8) bool {
		if div == 0 || div == 1 {
			return true
		}

		pred := func(i uint8) bool {
			return i%div == mod
		}

		foundIdx := FindIdx(s, pred)

		// onlyVal :: Option[T2[A, B]] -> Option[B]
		onlyVal := MapOption(func(t2 T2[int, uint8]) uint8 {
			return t2.Second()
		})

		valuesEqual := Find(s, pred) == onlyVal(foundIdx)

		idxGetsVal := ElimOption(
			foundIdx,
			func() bool { return true },
			func(t2 T2[int, uint8]) bool {
				return s[t2.First()] == t2.Second()
			})

		return valuesEqual && idxGetsVal
	}

	if err := quick.Check(f, nil); err != nil {
		t.Fatal(err)
	}
}

func TestPropLastTailIsLast(t *testing.T) {
	f := func(s []uint8) bool {
		// We exclude the singleton case because the Tail is empty.
		if len(s) <= 1 {
			return true
		}

		return Last(s) == FlatMapOption(Last[uint8])(Tail(s))
	}

	require.NoError(t, quick.Check(f, nil))
}

func TestPropHeadInitIsHead(t *testing.T) {
	f := func(s []uint8) bool {
		// We exclude the singleton case because the Init is empty.
		if len(s) <= 1 {
			return true
		}

		return Head(s) == FlatMapOption(Head[uint8])(Init(s))
	}

	require.NoError(t, quick.Check(f, nil))
}

func TestPropTailDecrementsLength(t *testing.T) {
	f := func(s []uint8) bool {
		if len(s) == 0 {
			return true
		}

		return Some(Len(s)-1) == MapOption(Len[uint8])(Tail(s))
	}

	require.NoError(t, quick.Check(f, nil))
}

func TestSingletonTailIsEmpty(t *testing.T) {
	require.Equal(t, Tail([]int{1}), Some([]int{}))
}

func TestSingletonInitIsEmpty(t *testing.T) {
	require.Equal(t, Init([]int{1}), Some([]int{}))
}

// TestPropAlwaysNoneEmptyFilterMap ensures the property that if we were to
// always return none from our filter function then we would end up with an
// empty slice.
func TestPropAlwaysNoneEmptyFilterMap(t *testing.T) {
	f := func(s []int) bool {
		filtered := FilterMap(s, Const[int](None[int]()))
		return len(filtered) == 0
	}

	require.NoError(t, quick.Check(f, nil))
}

// TestPropFilterMapSomeIdentity ensures that if the filter function is a
// trivial lift into Option space, then we will get back the original slice.
func TestPropFilterMapSomeIdentity(t *testing.T) {
	f := func(s []int) bool {
		filtered := FilterMap(s, Some[int])
		return slices.Equal(s, filtered)
	}

	require.NoError(t, quick.Check(f, nil))
}

// TestPropFilterMapCantGrow ensures that regardless of the filter functions
// return values, we will never end up with a slice larger than the original.
func TestPropFilterMapCantGrow(t *testing.T) {
	f := func(s []int) bool {
		filterFunc := func(i int) Option[int] {
			if rand.Int()%2 == 0 {
				return None[int]()
			}

			return Some(i + rand.Int())
		}

		return len(FilterMap(s, filterFunc)) <= len(s)
	}

	require.NoError(t, quick.Check(f, nil))
}

// TestPropFilterMapBisectIdentity ensures that the concatenation of the
// FilterMaps is the same as the FilterMap of the concatenation.
func TestPropFilterMapBisectIdentity(t *testing.T) {
	f := func(s []int) bool {
		sz := len(s)
		first := s[0 : sz/2]
		second := s[sz/2 : sz]

		filterFunc := func(i int) Option[int] {
			if i%2 == 0 {
				return None[int]()
			}

			return Some(i)
		}

		firstFiltered := FilterMap(first, filterFunc)
		secondFiltered := FilterMap(second, filterFunc)
		allFiltered := FilterMap(s, filterFunc)
		reassembled := slices.Concat(firstFiltered, secondFiltered)

		return slices.Equal(allFiltered, reassembled)
	}

	require.NoError(t, quick.Check(f, nil))
}

// TestTraverseOkIdentity ensures that trivially lifting the elements of a slice
// via the Ok function during a Traverse is equivalent to just lifting the
// entire slice via the Ok function.
func TestPropTraverseOkIdentity(t *testing.T) {
	f := func(s []int) bool {
		traversed := TraverseResult(s, Ok[int])

		traversedOk := traversed.UnwrapOrFail(t)

		return slices.Equal(s, traversedOk)
	}

	require.NoError(t, quick.Check(f, nil))
}

// TestPropTraverseSingleErrEjection ensures that if the traverse function
// returns even a single error, then the entire Traverse will error.
func TestPropTraverseSingleErrEjection(t *testing.T) {
	f := func(s []int, errIdx uint8) bool {
		if len(s) == 0 {
			return true
		}

		errIdxMut := int(errIdx) % len(s)
		f := func(i int) Result[int] {
			if errIdxMut == 0 {
				return Errf[int]("err")
			}

			errIdxMut--

			return Ok(i)
		}

		return TraverseResult(s, f).IsErr()
	}

	require.NoError(t, quick.Check(f, nil))
}

func TestPropInitDecrementsLength(t *testing.T) {
	f := func(s []uint8) bool {
		if len(s) == 0 {
			return true
		}

		return Some(Len(s)-1) == MapOption(Len[uint8])(Init(s))
	}

	require.NoError(t, quick.Check(f, nil))
}

// TestPropTrimNonesEqualsFilterMapIden checks that if we use the Iden
// function when calling FilterMap on a slice of Options that we get the same
// result as we would if we called TrimNones on it.
func TestPropTrimNonesEqualsFilterMapIden(t *testing.T) {
	f := func(s []uint8) bool {
		withNones := make([]Option[uint8], len(s))
		for i, x := range s {
			if x%3 == 0 {
				withNones[i] = None[uint8]()
			} else {
				withNones[i] = Some(x)
			}
		}

		return slices.Equal(
			FilterMap(withNones, Iden[Option[uint8]]),
			TrimNones(withNones),
		)
	}

	require.NoError(t, quick.Check(f, nil))
}

// TestPropCollectResultsSingleErrEjection ensures that if there is even a
// single error in the batch, then CollectResults will return an error.
func TestPropCollectResultsSingleErrEjection(t *testing.T) {
	f := func(s []int, errIdx uint8) bool {
		if len(s) == 0 {
			return true
		}

		errIdxMut := int(errIdx) % len(s)
		f := func(i int) Result[int] {
			if errIdxMut == 0 {
				return Errf[int]("err")
			}

			errIdxMut--

			return Ok(i)
		}

		return CollectResults(Map(s, f)).IsErr()
	}

	require.NoError(t, quick.Check(f, nil))
}

// TestPropCollectResultsNoErrUnwrap ensures that if there are no errors in the
// results then we end up with unwrapping all of the Results in the slice.
func TestPropCollectResultsNoErrUnwrap(t *testing.T) {
	f := func(s []int) bool {
		res := CollectResults(Map(s, Ok[int]))
		return !res.isRight && slices.Equal(res.left, s)
	}

	require.NoError(t, quick.Check(f, nil))
}

// TestPropTraverseSomeIdentity ensures that trivially lifting the elements of a
// slice via the Some function during a Traverse is equivalent to just lifting
// the entire slice via the Some function.
func TestPropTraverseSomeIdentity(t *testing.T) {
	f := func(s []int) bool {
		traversed := TraverseOption(s, Some[int])

		traversedSome := traversed.UnwrapOrFail(t)

		return slices.Equal(s, traversedSome)
	}

	require.NoError(t, quick.Check(f, nil))
}

// TestTraverseSingleNoneEjection ensures that if the traverse function returns
// even a single None, then the entire Traverse will return None.
func TestTraverseSingleNoneEjection(t *testing.T) {
	f := func(s []int, errIdx uint8) bool {
		if len(s) == 0 {
			return true
		}

		errIdxMut := int(errIdx) % len(s)
		f := func(i int) Option[int] {
			if errIdxMut == 0 {
				return None[int]()
			}

			errIdxMut--

			return Some(i)
		}

		return TraverseOption(s, f).IsNone()
	}

	require.NoError(t, quick.Check(f, nil))
}

// TestPropCollectOptionsSingleNoneEjection ensures that if there is even a
// single None in the batch, then CollectOptions will return a None.
func TestPropCollectOptionsSingleNoneEjection(t *testing.T) {
	f := func(s []int, errIdx uint8) bool {
		if len(s) == 0 {
			return true
		}

		errIdxMut := int(errIdx) % len(s)
		f := func(i int) Option[int] {
			if errIdxMut == 0 {
				return None[int]()
			}

			errIdxMut--

			return Some(i)
		}

		return CollectOptions(Map(s, f)).IsNone()
	}

	require.NoError(t, quick.Check(f, nil))
}

// TestPropCollectOptionsNoNoneUnwrap ensures that if there are no nones in the
// options then we end up with unwrapping all of the Options in the slice.
func TestPropCollectOptionsNoNoneUnwrap(t *testing.T) {
	f := func(s []int) bool {
		res := CollectOptions(Map(s, Some[int]))
		return res.isSome && slices.Equal(res.some, s)
	}

	require.NoError(t, quick.Check(f, nil))
}

// BenchmarkMapVsForEachConc benchmarks the performance of Map and ForEachConc
// for different workloads and slice sizes. It is used to show that ForEachConc
// is faster than Map when the workload is expensive.
func BenchmarkMapVsForEachConc(b *testing.B) {
	// Test different workload types.
	workloads := map[string]func(int) int{
		"Light": func(i int) int {
			return i + 1
		},
		"Expensive": func(i int) int {
			time.Sleep(time.Millisecond)
			return i + 1
		},
	}

	// Test different slice sizes.
	for _, size := range []int{10, 100, 1000} {
		s := make([]int, size)
		for i := range s {
			s[i] = i
		}

		for workload, inc := range workloads {
			// Benchmark Map.
			mapConf := fmt.Sprintf("Map-%s-Size-%d", workload, size)
			b.Run(mapConf, func(b *testing.B) {
				b.ResetTimer()
				b.ReportAllocs()

				for i := 0; i < b.N; i++ {
					Map(s, inc)
				}
			})

			// Benchmark ForEachConc.
			concConf := fmt.Sprintf(
				"ForEachConc-%s-Size-%d", workload, size,
			)
			b.Run(concConf, func(b *testing.B) {
				b.ResetTimer()
				b.ReportAllocs()

				for i := 0; i < b.N; i++ {
					ForEachConc(s, inc)
				}
			})
		}
	}
}
