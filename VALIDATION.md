# Validation Evidence

This document records reproducible evidence for the first public MDC release.
The source market corpus is not distributed by this repository.

## WINFUT quote projection

The repository tool `tools/winfut-validation` processed the non-empty
`ticks_20260623.jsonl` corpus available to the project on 2026-08-18.

```powershell
go run ./tools/winfut-validation `
  --input <path-to>/ticks_20260623.jsonl `
  --output <output>/winfut_projection_20260623.mdc `
  --report <output>/winfut_projection_20260623.md `
  --symbol WINFUT `
  --instrument WINFUT:B3 `
  --price-unit index-point `
  --tick-size 5
```

Observed evidence:

| Measure | Result |
| --- | ---: |
| Source SHA-256 | `d355214a511cfc7da7e704ed3c8bf5f5c479088af64292dc59570f118aa5f24a` |
| Generated MDC SHA-256 | `918921a38cb807cf93c1a5997de0282edcd67cda3357244a9441948bb20f1a54` |
| Source bytes | 1,824,922 |
| MDC bytes | 91,828 |
| Source lines | 22,755 |
| Accepted records | 22,753 |
| Rejected records | 2 |
| Independent blocks | 6 |
| Source-order timestamp regressions | 1 |

The two rejected records were crossed quotes (`ask < bid`) at source lines
18,231 and 21,866. They were not silently repaired.

For every accepted record, the tool decoded the generated MDC artifact and
compared timestamp, bid, and reconstructed ask against the source JSONL. It
then verified every header, CRC32C, block, index entry, and trailer.

## Claim boundary

This proves exact preservation of the encodable quote projection: source symbol
contract, timestamp, bid, and ask. Ask is represented as bid plus integer spread
ticks. The source `volume` field is not part of the current MDC record schema.
This is therefore not a claim of full-feed losslessness.

The byte-size comparison is reported as an observed artifact measurement, not
as a universal compression ratio. JSON syntax and the omitted volume field make
the two representations semantically different in scope.
