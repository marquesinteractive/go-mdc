# MDC Format Specification

Status: normative for the first public release of `go-mdc`.

MDC is a self-contained, block-oriented format for bounded financial tick data.
Each block uses a stable 16/8/4/4 packed word when a record fits and adds sparse
side metadata only for wide spread or flag values. Absolute bases, interpretation
metadata, CRC32C, sessions, indexing, and recovery boundaries belong to the same
MDC format.

All integers are little-endian. Reserved bytes must be zero. Sizes and counts
must be validated in a wide unsigned type before allocation or conversion to a
platform `int`.

## Technical versioning

`formatMajor=1`, `formatMinor=0` identifies this wire contract. This is internal
protocol versioning, not a separate MDC product name. A reader must reject an
unknown major version, unknown required feature, or non-little-endian marker.

The four-byte magics are:

| Section | Magic |
| --- | --- |
| File header | `MDCF` |
| Independent block | `MDBK` |
| Finite-file index | `MDCI` |
| Final trailer | `MDCE` |

Magic bytes are framing aids, not authentication.

## File header

The fixed portion is 72 bytes. It is followed by the UTF-8 instrument identifier,
the UTF-8 price-unit identifier, and zero padding until `headerBytes` is a
multiple of eight.

| Offset | Bytes | Field | Rule |
| ---: | ---: | --- | --- |
| `0x00` | 4 | magic | `MDCF` |
| `0x04` | 1 | formatMajor | `1` |
| `0x05` | 1 | formatMinor | `0` |
| `0x06` | 1 | endianness | `1` = little-endian |
| `0x07` | 1 | fileFlags | exactly `1` = finite/indexed or `2` = open stream |
| `0x08` | 4 | headerBytes | total header bytes, multiple of eight |
| `0x0C` | 4 | blockHeaderBytes | `80` |
| `0x10` | 1 | timeUnit | `1=ns`, `2=us`, `3=ms`, `4=s` |
| `0x11` | 1 | ordering | `0=source`, `1=nondecreasing`, `2=strict` |
| `0x12` | 1 | checksum | `1=CRC32C/Castagnoli` |
| `0x13` | 1 | priceMode | `1=integer ticks` |
| `0x14` | 1 | spreadUnit | `1=integer ticks` |
| `0x15` | 1 | textEncoding | `1=UTF-8` for both metadata strings |
| `0x16` | 2 | reserved | zero |
| `0x18` | 8 | defaultTickSizeNum | positive signed numerator |
| `0x20` | 8 | defaultTickSizeDen | positive unsigned denominator |
| `0x28` | 4 | instrumentBytes | UTF-8 byte count |
| `0x2C` | 4 | maxBlockTicks | writer-declared positive maximum |
| `0x30` | 4 | featureBits | `1` = sparse wide-field overrides |
| `0x34` | 4 | priceUnitBytes | UTF-8 byte count |
| `0x38` | 8 | declaredTickCount | exact count or `UINT64_MAX` when unknown |
| `0x40` | 4 | headerCRC32C | full header with these four bytes zeroed |
| `0x44` | 4 | reserved | zero |

One file describes one instrument. The identifier is an opaque namespace-local
string; it is not assumed to be globally unique.

Instrument and price-unit strings must be non-empty valid UTF-8, contain no
Unicode control code points, and have no leading or trailing Unicode whitespace.
Their bytes are otherwise opaque; the wire does not perform Unicode
normalization.

Tick size is the exact positive rational `Num/Den` in lowest terms; floating
point is not part of the wire definition. The economic bid value is
`BidTicks * Num / Den` in the explicit price unit carried by the header. A
writer may accept an equivalent non-reduced rational, but must normalize it
before serialization. Readers reject non-reduced wire values.

Absolute timestamps are signed integer counts in `timeUnit` since Unix Epoch
(`1970-01-01T00:00:00Z`). Leap-second interpretation follows Unix time. This
fixed epoch, the explicit time unit, and the ordering field make timestamp
semantics independent of an external dataset contract.

## Independent block

Every block has an 80-byte header, a fixed-word region, and an optional sparse
override region. A valid block is independently reconstructible and
checksum-verifiable.

| Offset | Bytes | Field | Rule |
| ---: | ---: | --- | --- |
| `0x00` | 4 | magic | `MDBK` |
| `0x04` | 1 | blockVersion | `1` |
| `0x05` | 1 | blockFlags | bit 0 iff overrides exist |
| `0x06` | 2 | headerBytes | `80` |
| `0x08` | 8 | blockBytes | `80 + 4*N + 16*E` |
| `0x10` | 4 | tickCount | `N > 0` |
| `0x14` | 4 | overrideCount | `0 <= E <= N` |
| `0x18` | 4 | blockSequence | starts at zero, increments by one |
| `0x1C` | 4 | session | application-defined session identifier |
| `0x20` | 8 | baseTimestamp | absolute timestamp of record zero |
| `0x28` | 8 | baseBidTicks | absolute bid of record zero |
| `0x30` | 8 | tickSizeNum | effective positive numerator |
| `0x38` | 8 | tickSizeDen | effective positive denominator |
| `0x40` | 4 | payloadCRC32C | CRC32C over words plus overrides |
| `0x44` | 4 | headerCRC32C | 80-byte header with this field zeroed |
| `0x48` | 8 | reserved | zero |

Payload order:

```text
N * 4 bytes   packed 16/8/4/4 words
E * 16 bytes  sparse overrides
```

There is no padding between blocks. All section boundaries remain aligned to
four bytes.

## Packed word primitive

The 32-bit word is:

```text
31            28 27            24 23            16 15             0
+----------------+----------------+----------------+----------------+
| flags (4 bits) | spread (4 bits)| deltaBid (8b) | deltaT (16 bits)|
+----------------+----------------+----------------+----------------+
```

```text
word =
    uint32(deltaT)
  | uint32(uint8(deltaBid)) << 16
  | uint32(spread & 0x0F)   << 24
  | uint32(flags  & 0x0F)   << 28
```

The complete word-domain is `deltaT=0..65535`, `deltaBid=-128..127`, and two
four-bit fields. The complete word-domain must round-trip every `uint32`.

Inside a canonical block, the normal semantic-domain is stricter:

```text
deltaT    = 0..65534
deltaBid  = -127..127
spread    = 0..15 unless overridden
flags     = 0..15 unless overridden
```

`65535` and `-128` remain round-trippable by the low-level primitive but are
reserved in canonical block records. The first word has zero time and bid
deltas because the block header already carries both absolute bases.

For record `i > 0`:

```text
Timestamp[i] = Timestamp[i-1] + deltaT[i]
BidTicks[i]  = BidTicks[i-1]  + deltaBid[i]
```

Writers must calculate absolute differences in a wide type before narrowing.
When time or bid delta does not fit, the current block ends and the exceptional
record becomes the absolute base of the next block. A session or tick-size
change also starts a new block. Under source ordering, timestamp regression
starts a new block; monotonic ordering modes reject it.

## Sparse override

Each 16-byte entry extends spread and/or flags to `uint32`:

| Offset | Bytes | Field | Rule |
| ---: | ---: | --- | --- |
| `0x00` | 4 | tickIndex | `0..N-1`, strictly increasing |
| `0x04` | 1 | mask | bit 0=spread, bit 1=flags; non-zero |
| `0x05` | 3 | reserved | zero |
| `0x08` | 4 | spread | full value when bit 0 is set, else zero |
| `0x0C` | 4 | flags | full value when bit 1 is set, else zero |

An overridden value must exceed 15. Its low nibble must equal the nibble stored
in the packed word. Entries are unique and sorted by `tickIndex`; redundant or
unused override data is non-canonical and must be rejected.

## Finite-file index and trailer

Finite files end with an index. Streaming files omit it and end at a block
boundary.

Index header, 24 bytes:

| Offset | Bytes | Field |
| ---: | ---: | --- |
| `0x00` | 4 | `MDCI` |
| `0x04` | 1 | version=`1` |
| `0x05` | 3 | zero |
| `0x08` | 8 | entryCount |
| `0x10` | 4 | entryBytes=`32` |
| `0x14` | 4 | CRC32C of bytes `0x00..0x13` |

Each 32-byte entry contains block offset (`uint64`), base timestamp (`int64`),
tick count, block sequence, session, and four zero bytes. Entries are ordered by
sequence and file offset.

Trailer, 24 bytes:

| Offset | Bytes | Field |
| ---: | ---: | --- |
| `0x00` | 4 | `MDCE` |
| `0x04` | 1 | version=`1` |
| `0x05` | 3 | zero |
| `0x08` | 8 | indexSectionBytes = header plus entries |
| `0x10` | 4 | CRC32C of all index entries |
| `0x14` | 4 | CRC32C of bytes `0x00..0x13` |

The trailer permits direct index discovery from EOF. Indexed readers must still
verify each selected block when its payload is read.

## Validation and recovery

A conforming finite reader verifies the file header, all selected block headers
and payloads, sequence numbers, semantic reconstruction, index/header/trailer
checksums, and agreement between decoded blocks and index entries. A full
verification consumes every block.

A streaming EOF is clean only at a block boundary; it cannot prove that the
producer intended to terminate. A finite file without a complete valid index
and trailer is incomplete.

Recovery requires a valid metadata header. A recovery scanner searches byte
boundaries for `MDBK` candidates and accepts a block only after its
version, dimensions, header CRC32C, payload CRC32C, and semantic contents pass.
Because every accepted block carries absolute bases, damaged intervals can be
discarded without contaminating later valid blocks. Recovery creates a new
canonical file and never copies an unverified block.

CRC32C detects accidental corruption; it does not authenticate hostile edits.
Authenticity requires a cryptographic envelope or signature outside this format.

## Golden interoperability fixtures

- `testdata/golden-packed-words.tsv` freezes the low-level packed-word arithmetic.
- `testdata/golden-container.bin` freezes canonical MDC bytes.
- `testdata/golden-container-input.txt` is the independent semantic oracle.

Go, Python, and C implementations validate these fixtures independently.
`testdata/SHA256SUMS` records their release-candidate byte identities.
