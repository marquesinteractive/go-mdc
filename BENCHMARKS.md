# Benchmarks

Measurements below were collected on 2026-08-18 from the release candidate,
using compiler-resistant sinks and 20 samples per benchmark.

## Environment

- OS: Windows amd64
- CPU: 13th Gen Intel Core i5-13400F
- Go: 1.26.2
- `GOMAXPROCS=1`
- input sample count: 20
- benchmark duration: 500 ms per sample
- statistics: `golang.org/x/perf/cmd/benchstat`

Command:

```powershell
$env:GOMAXPROCS = "1"
go test -run=^$ `
  -bench='Benchmark(Pack$|Unpack$|PackDeltas$|WritePackedWordsBuffer$|ContainerWrite4096$|ContainerRead4096$|IndexedSeekTimestamp4096$)' `
  -benchmem -benchtime=500ms -count=20 .
```

## Packed-word primitive

| Benchmark | Time | Throughput | Heap |
| --- | ---: | ---: | ---: |
| `Pack` | 2.804 ns/op +/- 10% | arithmetic operation | 0 B/op, 0 allocs/op |
| `Unpack` | 1.685 ns/op +/- 11% | arithmetic operation | 0 B/op, 0 allocs/op |
| `PackDeltas` (4,096 words) | 7.526 us/op +/- 9% | 2.028 GiB/s +/- 10% | 0 B/op, 0 allocs/op |
| `WritePackedWordsBuffer` (16,384 words) | 26.95 us/op +/- 5% | 2.264 GiB/s +/- 5% | 0 B/op, 0 allocs/op |

`Pack` and `Unpack` consume changing inputs and publish their accumulated result
to package-level sinks. Batch output remains observable after the timed loop.
The caller-owned write buffer removes scratch allocation from the measured
operation.

## Canonical container

The 4,096-record fixture uses exact metadata, normal deltas, CRC32C, a finite
index, and sparse wide-field overrides every 257 records.

| Benchmark | Time | Records/s | Encoded bytes/s | Heap |
| --- | ---: | ---: | ---: | ---: |
| `ContainerWrite4096` | 94.43 us/op +/- 10% | 43.38 M +/- 11% | 170.6 MiB/s +/- 11% | 37.02 KiB, 16 allocs/op |
| `ContainerRead4096` | 202.3 us/op +/- 5% | 20.26 M +/- 5% | 79.72 MiB/s +/- 5% | 212.7 KiB, 18 allocs/op |
| `IndexedSeekTimestamp4096` | 120.8 us/op +/- 6% | one indexed seek | not applicable | 210.7 KiB, 15 allocs/op |

Write timing includes metadata header creation, block construction, packed
words, sparse overrides, payload/header CRC32C, index, and trailer. The sink is
`io.Discard`, so this is codec throughput rather than storage throughput.

Read timing starts from in-memory canonical bytes and includes file-header,
block, payload, semantic, index, and trailer validation plus absolute-record
reconstruction. The reader materializes one independent block of records; its
allocation result is therefore not comparable to the zero-allocation primitive.

The seek benchmark reopens and validates the index, finds a target timestamp,
loads the containing 4,096-record block, verifies it, and returns one record. It
measures the current block-granularity access path, not a constant-time record
lookup.

## Interpretation limits

- These are single-machine results, not universal performance claims.
- No disk, network, decompression, parser, or exchange-feed cost is included.
- Primitive and container measurements have different semantics and must not be
  presented as direct competitors.
- The displayed `+/-` intervals are benchstat estimates from the 20-sample set;
  variance was retained rather than selecting the fastest run.
- Re-run the command on the target hardware before capacity planning.
