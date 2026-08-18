# Security Policy

## Supported versions

Security fixes are provided for the latest tagged major release. Before the
first tag, the default branch is the only supported revision.

## Report a vulnerability

Do not open a public issue for a vulnerability, memory-exhaustion path, parser
panic, silent data corruption, or cross-language wire disagreement.

Email `security@marquesinteractive.com` with:

- affected version or commit;
- minimal reproducer or malformed file;
- expected and observed behavior;
- impact and any known workaround.

Please do not include live credentials, private market data, or personally
identifiable information. We will acknowledge a valid report, investigate it,
and coordinate disclosure and credit with the reporter.

## Threat model

Canonical readers treat every wire count, size, offset, enum, reserved field,
CRC, delta, override, and index entry as untrusted. `ReaderLimits` bounds
allocations derived from the wire. Callers should lower these limits for
workloads with smaller known envelopes.

CRC32C detects accidental corruption. It is not a MAC, signature, or proof of
origin. An attacker who can edit a file can recompute CRC32C. Authenticate MDC
files with an external signature or authenticated transport when provenance
matters.

Recovery is forensic salvage, not authentication. It copies no source bytes
directly and re-encodes only blocks that pass structural, checksum, and semantic
validation, but a deliberately forged checksum-valid block remains possible.

The headerless packed-word helpers provide no framing, units, checksums,
absolute bases, or allocation limits beyond their explicit file-limit APIs.
Do not accept a headerless stream as a canonical MDC container.

The Python reference reader loads the complete file and defaults to a 1 GiB file
limit and 100 million total records. It is an interoperability implementation,
not a replacement for the bounded streaming Go reader.

## Out of scope

MDC does not interpret venue-specific flag bits or establish whether market
events are economically true. It preserves the declared representation and
rejects structural violations; source-data validation remains the producer's
responsibility.
