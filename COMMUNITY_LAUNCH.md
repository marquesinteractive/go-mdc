# MDC v1.0.0: deterministic market-data blocks for Go

MarquesInteractive is releasing MDC as an MIT-licensed Go library, CLI, and
documented binary format for compact quote data.

MDC began with a four-byte `16/8/4/4` delta word. The public release keeps that
primitive where it is effective, but surrounds it with the information and
failure boundaries required for a real file format: absolute bases, instrument,
price and time units, exact tick size, sessions, ordering, CRC32C, independent
blocks, a finite-file index, streaming, and recovery.

## Design position

MDC is one format, not a family of public variants. Normal records use the
four-byte packed word. Exceptional time gaps, price jumps, timestamp
regressions, session changes, and tick-size changes create a new block with new
absolute bases. Wide spread or flag values use sparse, canonical overrides.

This design keeps the common path compact without making uncommon values
lossy. It also gives every block an independent validation and recovery
boundary.

## Evidence in the repository

- normative byte-level specification;
- deterministic golden container shared by Go, Python, and C;
- full-word-domain property tests for the packed primitive;
- malformed-input, partial-I/O, fault-injection, fuzz, and allocation gates;
- indexed random access and byte-shift recovery tests;
- Go 1.22-1.26 and Linux/macOS/Windows CI matrices;
- a reproducible WINFUT projection validation with hashes and explicit claim
  boundaries;
- DCE-resistant benchmarks that keep primitive and container layers separate.

## What we do not claim

MDC is not a universal exchange schema. The current record stores timestamp,
bid ticks, spread ticks, flags, session, and tick size. It does not currently
store volume, depth, trade side, or an independently quoted ask. CRC32C is
corruption detection, not authentication. Recovery is salvage, not proof of
origin.

The WINFUT validation proves exact round-trip preservation of the encodable
quote projection for 22,753 accepted records. It does not claim full-feed
losslessness because source volume is intentionally outside the current schema.

## Use it

```bash
go get github.com/marquesinteractive/go-mdc@v1.0.0
go install github.com/marquesinteractive/go-mdc/cmd/mdc@v1.0.0
```

Start with [`README.md`](README.md), inspect the wire contract in
[`SPEC.md`](SPEC.md), and review the measured boundaries in
[`VALIDATION.md`](VALIDATION.md) and [`BENCHMARKS.md`](BENCHMARKS.md).

Issues and technically grounded contributions are welcome. Wire changes,
performance claims, and low-level optimizations must arrive with reproducible
evidence.
