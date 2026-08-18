# MDC

[![CI](https://github.com/marquesinteractive/go-mdc/actions/workflows/ci.yml/badge.svg)](https://github.com/marquesinteractive/go-mdc/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/marquesinteractive/go-mdc.svg)](https://pkg.go.dev/github.com/marquesinteractive/go-mdc)
[![License: MIT](https://img.shields.io/badge/license-MIT-black.svg)](LICENSE)

MDC is a deterministic, self-contained market-data container for compact quote
records. It combines a zero-allocation 32-bit `16/8/4/4` delta primitive with
absolute block bases, explicit units, exact tick sizes, sessions, CRC32C,
streaming, random access, and block-level recovery.

The format has one public identity: **MDC**. `formatMajor=1` and
`formatMinor=0` are wire-version fields, not separate product names.

## What one MDC file carries

- instrument identifier and economic price unit;
- Unix timestamp unit (`ns`, `us`, `ms`, or `s`) and ordering contract;
- exact rational tick size, normalized to lowest terms;
- independent blocks with absolute timestamp and bid bases;
- four-byte packed words for normal deltas;
- sparse `uint32` overrides for wide spread or flag values;
- session and tick-size changes at block boundaries;
- CRC32C for every file header, block header, block payload, and index section;
- optional finite-file index for block and timestamp seeking.

There is no silent truncation in the canonical writer. A time gap, price jump,
timestamp regression under source ordering, session change, or tick-size change
starts a new independent block. Monotonic ordering violations are errors.

## Install

```bash
go get github.com/marquesinteractive/go-mdc@v1.0.0
go install github.com/marquesinteractive/go-mdc/cmd/mdc@v1.0.0
```

MDC requires Go 1.22 or newer and has no runtime dependencies outside the
standard library.

## Go quick start

```go
package main

import (
    "bytes"
    "fmt"

    mdc "github.com/marquesinteractive/go-mdc"
)

func main() {
    metadata := mdc.Metadata{
        Instrument: "WINFUT:B3",
        PriceUnit:  "index-point",
        TimeUnit:   mdc.TimeMillisecond,
        Ordering:   mdc.NonDecreasing,
        TickSize:   mdc.Rational{Num: 5, Den: 1},
        SpreadUnit: mdc.SpreadInTicks,
    }
    records := []mdc.Record{
        {Timestamp: 1_787_000_000_000, BidTicks: 34_910, Spread: 1, Session: 20260818},
        {Timestamp: 1_787_000_000_010, BidTicks: 34_911, Spread: 31, Flags: 0x120, Session: 20260818},
    }

    var encoded bytes.Buffer
    writer, err := mdc.NewWriter(&encoded, metadata)
    if err != nil {
        panic(err)
    }
    if _, err := writer.WriteBatch(records); err != nil {
        panic(err)
    }
    if err := writer.Close(); err != nil {
        panic(err)
    }

    reader, err := mdc.NewReader(bytes.NewReader(encoded.Bytes()))
    if err != nil {
        panic(err)
    }
    record, err := reader.ReadTick()
    if err != nil {
        panic(err)
    }
    fmt.Println(reader.Metadata().Instrument, record.BidTicks)
}
```

`Writer.Close` flushes the final block and writes the finite-file index. It does
not close the underlying `io.Writer`.

## CLI

The CLI accepts a strict five- or seven-column CSV schema and commits output
through a synced temporary file. It refuses to overwrite an existing path.

```bash
mdc encode \
  --instrument WINFUT:B3 \
  --price-unit index-point \
  --time-unit ms \
  --ordering source \
  --tick-size 5/1 \
  ticks.csv ticks.mdc

mdc inspect ticks.mdc
mdc verify ticks.mdc
mdc decode ticks.mdc decoded.csv
mdc recover damaged.mdc recovered.mdc
```

Base CSV schema:

```text
timestamp,bid_ticks,spread,flags,session
```

To change tick size within a file, append `tick_size_num,tick_size_den`. Both
fields must be blank or both present. Input rationals are normalized before
serialization.

## Random access

`Open` validates the header, index, and trailer immediately. The selected block
is validated when read.

```go
file, _ := os.Open("ticks.mdc")
reader, err := mdc.Open(file)
if err != nil {
    panic(err)
}
if err := reader.SeekTimestamp(targetUnixMillis); err != nil {
    panic(err)
}
record, err := reader.ReadTick()
```

Timestamp seeking requires `NonDecreasing` or `StrictlyIncreasing` ordering.
`SourceOrder` files support block seeking but not temporal binary search.

## Streaming

`NewStreamWriter` emits the same headers and independently verifiable blocks but
omits the final index. A clean EOF is accepted only at a block boundary. A
stream cannot prove that its producer intended to terminate.

```go
writer, _ := mdc.NewStreamWriter(connection, metadata)
_ = writer.WriteTick(record)
_ = writer.Flush()
_ = writer.Close()
```

## Recovery

`Recover` requires an intact metadata header. It scans byte boundaries for block
magic and accepts a candidate only after dimensions, sequence-local semantics,
header CRC32C, payload CRC32C, reserved fields, and overrides validate. Every
accepted block is re-encoded into a new finite MDC file; unverified bytes are
reported as damage ranges.

CRC32C detects accidental corruption. It does not authenticate hostile edits.
Use a signature or authenticated envelope when provenance is adversarial.

## Packed-word primitive

The low-level primitive remains available for applications that already own a
separate schema and framing layer:

```go
word, err := mdc.PackNormalChecked(25, -5, 1, 2)
delta := mdc.DecodeWord(word)
```

Its bit layout is `deltaT:16 | deltaBid:8 | spread:4 | flags:4`, serialized
little-endian. `Pack` round-trips the complete `uint32` word domain and masks
the two four-bit fields by design. Use checked APIs for untrusted semantic
input.

`WritePackedWordsFile`, `ReadPackedWordsFile`, `PackedWordEncoder`, and
`PackedWordDecoder` operate on **headerless words**, not canonical `.mdc`
containers. Their explicit names are intended to prevent format confusion.

## Interoperability

- [`examples/04_python_polars_pandas`](examples/04_python_polars_pandas) contains
  an independent NumPy reader with CRC32C and semantic validation.
- [`examples/05_c_interop`](examples/05_c_interop) contains independent C11
  validators for both shared packed-word vectors and the canonical container.
- [`testdata/golden-container.bin`](testdata/golden-container.bin) freezes the
  deterministic wire output used by Go, Python, and C.

## Security and limits

Readers validate counts in wide integer types before narrowing or allocating.
`ReaderLimits` bounds headers, block ticks, block bytes, overrides, and index
entries. The defaults allow at most 1,048,576 ticks per block and 64 MiB per
block. Writers cannot emit a block larger than the canonical default reader can
accept.

The package contains no `unsafe`, assembly, CGo, memory mapping, or secondary
compression layer. Such changes require measured evidence and a compatible
failure model.

## Scope

The current record schema stores timestamp, bid ticks, spread ticks, flags,
session, and effective tick size. It does not encode volume, ask independently
of spread, order-book depth, trade side, or venue-specific flag semantics.
Applications must not claim full-feed losslessness unless every required source
field is represented.

See:

- [`SPEC.md`](SPEC.md) for the normative wire contract;
- [`VALIDATION.md`](VALIDATION.md) for corpus evidence and claim boundaries;
- [`BENCHMARKS.md`](BENCHMARKS.md) for reproducible measurements;
- [`SECURITY.md`](SECURITY.md) for the threat model and reporting process.

## License

MIT. Copyright 2026 MarquesInteractive.
