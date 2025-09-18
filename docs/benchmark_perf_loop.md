# The Go Performance Optimization Loop: From Benchmarks to Zero Allocations

When optimizing Go code for performance, particularly in hot paths like
cryptographic operations or protocol handling, the journey from identifying
bottlenecks to achieving zero-allocation code follows a well-defined
methodology. This document walks through the complete optimization loop using
Go's built-in tooling, demonstrating how to systematically eliminate allocations
and improve performance.

## Understanding the Performance Baseline

The first step in any optimization effort is establishing a measurable baseline.
Go's benchmark framework provides the foundation for this measurement. When
writing benchmarks for allocation-sensitive code, always include a call to
`b.ReportAllocs()` before `b.ResetTimer()`. This ensures the benchmark reports
both timing and allocation statistics without including setup costs in the
measurements.

Consider a benchmark that exercises a cryptographic write path with the largest
possible message size to stress test allocations:

```go
func BenchmarkWriteMessage(b *testing.B) {
    // Setup code here...
    
    b.ReportAllocs()  // Essential for tracking allocations
    b.ResetTimer()
    
    for i := 0; i < b.N; i++ {
        // Hot path being measured
    }
}
```

Running the benchmark with `go test -bench=BenchmarkWriteMessage -benchmem
-count=10` provides statistical confidence through multiple runs. The
`-benchmem` flag is redundant if you've called `b.ReportAllocs()`, but it
doesn't hurt to include it explicitly. The output reveals three critical
metrics: nanoseconds per operation, bytes allocated per operation, and the
number of distinct allocations per operation.

## Profiling Memory Allocations

Once you have baseline measurements showing undesirable allocations, the next
phase involves profiling to understand where these allocations originate.
Generate memory profiles during benchmark execution using:

```
go test -bench=BenchmarkWriteMessage -memprofile=mem.prof -cpuprofile=cpu.prof -count=1
```

The resulting profile can be analyzed through several lenses. To see which
functions allocate the most memory by total bytes, use:
`go tool pprof -alloc_space -top mem.prof`. 

However, for understanding allocation frequency rather than size, `go tool pprof -alloc_objects -top mem.prof` often provides more actionable insights, especially when hunting small but frequent allocations.

Here's what the allocation object analysis might reveal:

```
$ go tool pprof -alloc_objects -top mem.prof | head -20
File: brontide.test
Type: alloc_objects
Time: Aug 30, 2024 at 2:07pm (WEST)
Showing nodes accounting for 39254, 100% of 39272 total
Dropped 32 nodes (cum <= 196)
      flat  flat%   sum%        cum   cum%
     32768 83.44% 83.44%      32768 83.44%  github.com/lightningnetwork/lnd/brontide.(*cipherState).Encrypt
      5461 13.91% 97.34%       5461 13.91%  runtime.acquireSudog
      1025  2.61%   100%       1025  2.61%  runtime.allocm
```

This output immediately shows that `cipherState.Encrypt` is responsible for 83%
of allocations by count, focusing our investigation.

The most powerful profiling technique involves examining allocations at the
source line level. Running `go tool pprof -list 'FunctionName' mem.prof` shows
exactly which lines within a function trigger heap allocations:

```
$ go tool pprof -list 'cipherState.*Encrypt' mem.prof
Total: 8.73MB
ROUTINE ======================== github.com/lightningnetwork/lnd/brontide.(*cipherState).Encrypt
  512.01kB   512.01kB (flat, cum)  5.73% of Total
         .          .    111:func (c *cipherState) Encrypt(associatedData, cipherText, plainText []byte) []byte {
         .          .    112:	defer func() {
         .          .    113:		c.nonce++
         .          .    114:
         .          .    115:		if c.nonce == keyRotationInterval {
         .          .    116:			c.rotateKey()
         .          .    117:		}
         .          .    118:	}()
         .          .    119:
  512.01kB   512.01kB    120:	var nonce [12]byte
         .          .    121:	binary.LittleEndian.PutUint64(nonce[4:], c.nonce)
         .          .    122:
         .          .    123:	return c.cipher.Seal(cipherText, nonce[:], plainText, associatedData)
```

This granular view reveals that line 120, a seemingly innocent stack array
declaration, is allocating 512KB total across all benchmark iterations.

## CPU Profiling for Hot Spots

While memory allocations often dominate optimization efforts, CPU profiling
reveals where computational time is spent. The CPU profile generated alongside
the memory profile provides complementary insights:

```
$ go tool pprof -top cpu.prof | head -15
File: brontide.test
Type: cpu
Time: Aug 30, 2024 at 2:07pm (WEST)
Duration: 1.8s, Total samples = 1.71s (94.40%)
Showing nodes accounting for 1.65s, 96.49% of 1.71s total
      flat  flat%   sum%        cum   cum%
     0.51s 29.82% 29.82%      0.51s 29.82%  vendor/golang.org/x/crypto/chacha20poly1305.(*chacha20poly1305).sealGeneric
     0.28s 16.37% 46.20%      0.28s 16.37%  vendor/golang.org/x/crypto/internal/poly1305.updateGeneric
     0.24s 14.04% 60.23%      0.24s 14.04%  vendor/golang.org/x/crypto/chacha20.(*Cipher).XORKeyStream
     0.19s 11.11% 71.35%      0.19s 11.11%  runtime.memmove
     0.12s  7.02% 78.36%      0.86s 50.29%  github.com/lightningnetwork/lnd/brontide.(*cipherState).Encrypt
```

This profile shows that cryptographic operations dominate CPU usage, which is
expected. However, note the presence of `runtime.memmove` at 11% - this often
indicates unnecessary copying that could be eliminated through careful buffer
management.

For line-level CPU analysis of a specific function:

```
$ go tool pprof -list 'WriteMessage' cpu.prof
Total: 1.71s
ROUTINE ======================== github.com/lightningnetwork/lnd/brontide.(*Machine).WriteMessage
      10ms      1.21s (flat, cum) 70.76% of Total
         .          .    734:func (b *Machine) WriteMessage(p []byte) error {
         .          .    735:	if len(p) > math.MaxUint16 {
         .          .    736:		return ErrMaxMessageLengthExceeded
         .          .    737:	}
         .          .    738:
         .       10ms    739:	if len(b.nextHeaderSend) > 0 || len(b.nextBodySend) > 0 {
         .          .    740:		return ErrMessageNotFlushed
         .          .    741:	}
         .          .    742:
      10ms       10ms    743:	fullLength := uint16(len(p))
         .          .    744:	var pktLen [2]byte
         .       10ms    745:	binary.BigEndian.PutUint16(pktLen[:], fullLength)
         .          .    746:
         .      580ms    747:	b.nextHeaderSend = b.sendCipher.Encrypt(nil, nil, pktLen[:])
         .      600ms    748:	b.nextBodySend = b.sendCipher.Encrypt(nil, nil, p)
```

This shows that the two `Encrypt` calls consume virtually all the CPU time in
`WriteMessage`, confirming that cryptographic operations are the bottleneck
rather than the message handling logic itself.

## Understanding Escape Analysis

When the profiler indicates that seemingly stack-local variables are being heap
allocated, escape analysis becomes your next investigative tool. The Go
compiler's escape analysis determines whether variables can remain on the stack
or must be moved to the heap. Variables escape to the heap when their lifetime
extends beyond the function that creates them or when the compiler cannot prove
they remain local.

To see the compiler's escape analysis decisions, build with verbose flags:

```
go build -gcflags="-m" ./...
```

For more detailed output including the reasons for escape, use `-m=2`. The
output reveals escape flows, showing exactly why variables move to the heap.
When investigating specific escapes, you can grep for the variable in question:

```
$ go build -gcflags="-m=2" ./... 2>&1 | grep -A2 -B2 "nonce escapes"
./noise.go:183:17: &errors.errorString{...} does not escape
./noise.go:183:17: new(chacha20poly1305.chacha20poly1305) escapes to heap
./noise.go:120:6: nonce escapes to heap:
./noise.go:120:6:   flow: {heap} = &nonce:
./noise.go:120:6:     from nonce (address-of) at ./noise.go:123:40
--
./noise.go:469:21: &keychain.PrivKeyECDH{...} escapes to heap
./noise.go:483:40: []byte{} escapes to heap
./noise.go:138:6: nonce escapes to heap:
./noise.go:138:6:   flow: {heap} = &nonce:
./noise.go:138:6:     from nonce (address-of) at ./noise.go:141:39
```

This output shows the exact flow analysis: the nonce array escapes because its
address is taken when creating a slice (`nonce[:]`) and passed to a function
that the compiler cannot fully analyze.

Common causes include passing pointers to interfaces, storing references in
heap-allocated structures, or passing slices of stack arrays to functions that
might retain them. A particularly instructive example is the seemingly innocent
pattern of passing a stack array to a function:

```go
var nonce [12]byte
binary.LittleEndian.PutUint64(nonce[4:], counter)
return cipher.Seal(ciphertext, nonce[:], plaintext, nil)
```

Here, `nonce[:]` creates a slice backed by the stack array, but if the compiler
cannot prove that `cipher.Seal` won't retain a reference to this slice, the
entire array escapes to the heap.

## The Optimization Strategy

Armed with profiling data and escape analysis insights, the optimization phase
begins. The general strategy for eliminating allocations follows a predictable
pattern: move temporary buffers from function scope to longer-lived structures,
typically as fields in the enclosing type. This transformation changes
allocation from per-operation to per-instance.

For the nonce example above, the optimization involves adding a buffer field to
the containing struct:

```go
type cipherState struct {
    // ... other fields ...
    nonceBuffer [12]byte  // Reusable buffer to avoid allocations
}

func (c *cipherState) Encrypt(...) []byte {
    binary.LittleEndian.PutUint64(c.nonceBuffer[4:], c.nonce)
    return c.cipher.Seal(ciphertext, c.nonceBuffer[:], plaintext, nil)
}
```

This pattern extends to any temporary buffer. When dealing with variable-sized
data up to a known maximum, pre-allocate buffers at that maximum size and slice
into them as needed. The key insight is using the three-index slice notation to
control capacity separately from length:

```go
// Pre-allocated: var buffer [maxSize]byte

// Creating a zero-length slice with full capacity for append:
slice := buffer[:0]  // length=0, capacity=maxSize
```

## Verification and Iteration

After implementing optimizations, the cycle returns to benchmarking. Run the
same benchmark to measure improvement, but don't stop at the aggregate numbers.
Generate new profiles to verify that specific allocations have been eliminated
and to identify any remaining allocation sites.

The benchstat tool provides statistical comparison between runs:

```
go test -bench=BenchmarkWriteMessage -count=10 > old.txt
# Make optimizations
go test -bench=BenchmarkWriteMessage -count=10 > new.txt
benchstat old.txt new.txt
```

This comparison reveals not just whether performance improved, but whether the
improvement is statistically significant. A typical benchstat output after
successful optimization looks like:

```
goos: darwin
goarch: arm64
pkg: github.com/lightningnetwork/lnd/brontide
cpu: Apple M4 Max
                │   old.txt   │              new.txt               │
                │   sec/op    │   sec/op     vs base               │
WriteMessage-16   50.34µ ± 1%   46.48µ ± 0%  -7.68% (p=0.000 n=10)

                │    old.txt     │              new.txt              │
                │      B/op      │    B/op     vs base                │
WriteMessage-16   73788.000 ± 0%   2.000 ± 0%  -100.00% (p=0.000 n=10)

                │  old.txt   │              new.txt              │
                │ allocs/op  │ allocs/op   vs base                │
WriteMessage-16   5.000 ± 0%   0.000 ± 0%  -100.00% (p=0.000 n=10)
```

The key metrics to examine are:
- The percentage change (vs base column) showing the magnitude of improvement

- The p-value (p=0.000) indicating statistical significance - values below 0.05
suggest real improvements rather than noise

- The variance (± percentages) showing consistency across runs

This output confirms both a 7.68% speed improvement and complete elimination of
allocations, with high statistical confidence.

If allocations remain, the cycle continues. Profile again, identify the source,
understand why the allocation occurs through escape analysis, and apply the
appropriate optimization pattern. Each iteration should show measurable progress
toward the goal of zero allocations in the hot path.

## Advanced Techniques

When standard profiling doesn't reveal the allocation source, more advanced
techniques come into play. Memory profiling with different granularities can
help. Instead of looking at total allocations, examine the profile with `go tool
pprof -sample_index=alloc_objects` to focus on allocation count rather than
size. This distinction matters when hunting for small, frequent allocations that
might not show up prominently in byte-focused views.

Additional pprof commands that prove invaluable during optimization:

```bash
# Interactive mode for exploring the profile
go tool pprof mem.prof
(pprof) top10        # Show top 10 memory consumers
(pprof) list regexp  # List functions matching regexp
(pprof) web          # Open visual graph in browser

# Generate a flame graph for visual analysis
go tool pprof -http=:8080 mem.prof

# Compare two profiles directly
go tool pprof -base=old.prof new.prof

# Show allocations only from specific packages
go tool pprof -focus=github.com/lightningnetwork/lnd/brontide mem.prof

# Check for specific small allocations
go tool pprof -alloc_space -inuse_space mem.prof
```

When dealing with elusive allocations, checking what might be escaping to heap
can be done more surgically:

```bash
# Check specific function or type for escapes
go build -gcflags="-m" 2>&1 | grep -E "(YourType|yourFunc)"

# See all heap allocations in a package
go build -gcflags="-m" 2>&1 | grep "moved to heap"

# Check which variables are confirmed to stay on the stack
go build -gcflags="-m=2" 2>&1 | grep "does not escape"
```

For particularly elusive allocations, instrumenting the code with runtime memory
statistics can provide real-time feedback:

```go
var m runtime.MemStats
runtime.ReadMemStats(&m)
before := m.Alloc
// Operation being measured
runtime.ReadMemStats(&m)
allocated := m.Alloc - before
```

While this approach adds overhead and shouldn't be used in production, it can
help isolate allocations to specific code sections during development.

## The Zero-Allocation Goal

Achieving zero allocations in hot paths represents more than just a performance
optimization. It provides predictable latency, reduces garbage collection
pressure, and improves overall system behavior under load. In systems handling
thousands of operations per second, the difference between five allocations per
operation and zero can mean the difference between smooth operation and periodic
latency spikes during garbage collection.

The journey from initial benchmark to zero-allocation code demonstrates the
power of Go's built-in tooling. By systematically applying the
benchmark-profile-optimize loop, even complex code paths can be transformed into
allocation-free implementations. The key lies not in guessing or premature
optimization, but in measuring, understanding, and methodically addressing each
allocation source.

It's best to focus optimization efforts on true hot paths identified through
production profiling or realistic load testing. The techniques described here
provide the tools to achieve zero-allocation code when it matters, but the
judgment of when to apply them remains a critical engineering decision.
