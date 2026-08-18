# Contributing

Contributions are welcome when they preserve wire determinism, explicit failure
semantics, and evidence-based performance claims.

## Local gates

```bash
gofmt -w .
go vet ./...
go test -count=1 ./...
go test -count=1 -race ./...
python -m unittest discover -s examples/04_python_polars_pandas -p "test_*.py"
```

Compile the independent C11 validators where a C compiler is available:

```bash
cc -std=c11 -Wall -Wextra -Werror examples/05_c_interop/main.c -o mdc-c-words
cc -std=c11 -Wall -Wextra -Werror examples/05_c_interop/container.c -o mdc-c-container
./mdc-c-words testdata/golden-packed-words.tsv
./mdc-c-container testdata/golden-container.bin
```

Run each fuzz target independently:

```bash
go test -run=^$ -fuzz=^FuzzContainerReader$ -fuzztime=30s .
go test -run=^$ -fuzz=^FuzzContainerRoundTrip$ -fuzztime=30s .
go test -run=^$ -fuzz=^FuzzRecover$ -fuzztime=30s .
```

## Wire changes

`SPEC.md` is normative. A wire change must include:

- explicit compatibility analysis and version behavior;
- deterministic Go fixture generation;
- updated Go, Python, and C golden validation;
- malformed-input and fault-injection coverage;
- migration notes for existing files.

Do not repurpose a reserved field silently.

## Performance changes

Benchmarks must consume their results, use `-benchmem`, and report the machine,
Go version, `GOMAXPROCS`, sample count, and command. Compare operations with the
same semantics. Primitive bit packing and a checksummed indexed container are
different benchmark layers.

`unsafe`, assembly, CGo, memory mapping, and additional compression are not
accepted without reproducible evidence, a portable fallback, fault analysis,
and end-to-end gains that justify maintenance cost.

## Pull requests

Keep changes scoped. Describe correctness properties, compatibility impact,
test evidence, and benchmark evidence where relevant. Never commit proprietary
market data, credentials, generated benchmark binaries, or private system
references.
