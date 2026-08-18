package mdc_test

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	mdc "github.com/marquesinteractive/go-mdc"
)

func testMetadata(ordering mdc.Ordering) mdc.Metadata {
	return mdc.Metadata{
		Instrument: "WINFUT:B3",
		PriceUnit:  "index-point",
		TimeUnit:   mdc.TimeMillisecond,
		Ordering:   ordering,
		TickSize:   mdc.Rational{Num: 5, Den: 1},
		SpreadUnit: mdc.SpreadInTicks,
	}
}

func complexRecords() []mdc.Record {
	return []mdc.Record{
		{Timestamp: 1_000, BidTicks: 100, Spread: 1, Flags: 2, Session: 1},
		{Timestamp: 1_010, BidTicks: 105, Spread: 2, Flags: 3, Session: 1},
		{Timestamp: 70_000, BidTicks: 106, Spread: 3, Flags: 4, Session: 1},
		{Timestamp: 70_001, BidTicks: 234, Spread: 4, Flags: 5, Session: 1},
		{Timestamp: 70_002, BidTicks: 235, Spread: 31, Flags: 0x1234, Session: 1},
		{Timestamp: 70_003, BidTicks: 236, Spread: 6, Flags: 7, Session: 2},
		{Timestamp: 70_004, BidTicks: 237, Spread: 8, Flags: 9, Session: 2, TickSize: mdc.Rational{Num: 5, Den: 2}},
		{Timestamp: 69_900, BidTicks: 238, Spread: 10, Flags: 11, Session: 2},
	}
}

func normalizeRecords(records []mdc.Record, initial mdc.Rational) []mdc.Record {
	result := make([]mdc.Record, len(records))
	current := initial
	for i, record := range records {
		if record.TickSize.Den != 0 {
			current = record.TickSize
		}
		record.TickSize = current
		result[i] = record
	}
	return result
}

func encodeContainer(t *testing.T, metadata mdc.Metadata, records []mdc.Record, config mdc.WriterConfig) []byte {
	t.Helper()
	var output bytes.Buffer
	writer, err := mdc.NewWriterConfig(&output, metadata, config)
	if err != nil {
		t.Fatalf("new writer: %v", err)
	}
	if n, err := writer.WriteBatch(records); err != nil || n != len(records) {
		t.Fatalf("write batch: n=%d err=%v", n, err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	return output.Bytes()
}

func readAllRecords(t *testing.T, reader *mdc.Reader) []mdc.Record {
	t.Helper()
	var records []mdc.Record
	for {
		record, err := reader.ReadTick()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read tick %d: %v", len(records), err)
		}
		records = append(records, record)
	}
	return records
}

func TestContainerRoundTripResetsOverridesSessionsAndTickSize(t *testing.T) {
	metadata := testMetadata(mdc.SourceOrder)
	config := mdc.DefaultWriterConfig()
	config.MaxBlockTicks = 2
	input := complexRecords()
	encoded := encodeContainer(t, metadata, input, config)

	reader, err := mdc.NewReader(bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("new reader: %v", err)
	}
	if reader.Metadata() != metadata {
		t.Fatalf("metadata: got %+v, want %+v", reader.Metadata(), metadata)
	}
	got := readAllRecords(t, reader)
	want := normalizeRecords(input, metadata.TickSize)
	if len(got) != len(want) {
		t.Fatalf("record count: got %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("record %d: got %+v, want %+v", i, got[i], want[i])
		}
	}

	fileReader, err := mdc.Open(bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("open indexed: %v", err)
	}
	if fileReader.BlockCount() != 6 {
		t.Fatalf("block count: got %d, want 6", fileReader.BlockCount())
	}
	if err := fileReader.SeekBlock(3); err != nil {
		t.Fatalf("seek block: %v", err)
	}
	record, err := fileReader.ReadTick()
	if err != nil || record != want[5] {
		t.Fatalf("block 3 first record: got=%+v err=%v, want=%+v", record, err, want[5])
	}
}

func TestContainerTemporalSeek(t *testing.T) {
	metadata := testMetadata(mdc.NonDecreasing)
	config := mdc.DefaultWriterConfig()
	config.MaxBlockTicks = 2
	records := []mdc.Record{
		{Timestamp: 10, BidTicks: 100},
		{Timestamp: 20, BidTicks: 101},
		{Timestamp: 30, BidTicks: 102},
		{Timestamp: 40, BidTicks: 103},
		{Timestamp: 50, BidTicks: 104},
	}
	encoded := encodeContainer(t, metadata, records, config)
	reader, err := mdc.Open(bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := reader.SeekTimestamp(35); err != nil {
		t.Fatalf("seek timestamp: %v", err)
	}
	got, err := reader.ReadTick()
	if err != nil || got.Timestamp != 40 {
		t.Fatalf("seek result: got=%+v err=%v", got, err)
	}
	if err := reader.SeekTimestamp(100); err != nil {
		t.Fatalf("seek past end: %v", err)
	}
	if _, err := reader.ReadTick(); !errors.Is(err, io.EOF) {
		t.Fatalf("seek past end read: got %v, want EOF", err)
	}
}

func TestContainerTemporalSeekReturnsFirstEqualTimestampAcrossBlocks(t *testing.T) {
	metadata := testMetadata(mdc.NonDecreasing)
	config := mdc.DefaultWriterConfig()
	config.MaxBlockTicks = 1
	records := []mdc.Record{
		{Timestamp: 10, BidTicks: 101},
		{Timestamp: 10, BidTicks: 102},
		{Timestamp: 10, BidTicks: 103},
		{Timestamp: 20, BidTicks: 104},
	}
	encoded := encodeContainer(t, metadata, records, config)
	reader, err := mdc.Open(bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.SeekTimestamp(10); err != nil {
		t.Fatal(err)
	}
	record, err := reader.ReadTick()
	if err != nil {
		t.Fatal(err)
	}
	if record.BidTicks != 101 {
		t.Fatalf("got bid %d, want first equal-timestamp bid 101", record.BidTicks)
	}
}

func TestSourceOrderRejectsTemporalSeek(t *testing.T) {
	encoded := encodeContainer(t, testMetadata(mdc.SourceOrder), complexRecords(), mdc.DefaultWriterConfig())
	reader, err := mdc.Open(bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := reader.SeekTimestamp(1); !errors.Is(err, mdc.ErrOrderingViolation) {
		t.Fatalf("got %v, want ErrOrderingViolation", err)
	}
}

func TestStreamingContainerRoundTripWithPartialIO(t *testing.T) {
	metadata := testMetadata(mdc.SourceOrder)
	output := &chunkWriter{max: 3}
	writer, err := mdc.NewStreamWriter(output, metadata)
	if err != nil {
		t.Fatalf("new stream writer: %v", err)
	}
	input := complexRecords()
	if _, err := writer.WriteBatch(input); err != nil {
		t.Fatalf("write stream: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close stream: %v", err)
	}

	reader, err := mdc.NewReader(&chunkReader{reader: bytes.NewReader(output.Bytes()), max: 2})
	if err != nil {
		t.Fatalf("new partial reader: %v", err)
	}
	got := readAllRecords(t, reader)
	want := normalizeRecords(input, metadata.TickSize)
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("record %d: got %+v, want %+v", i, got[i], want[i])
		}
	}
	if _, err := mdc.Open(bytes.NewReader(output.Bytes())); !errors.Is(err, mdc.ErrMissingIndex) {
		t.Fatalf("open streaming file: got %v, want ErrMissingIndex", err)
	}
}

func TestContainerOrderingContracts(t *testing.T) {
	for _, ordering := range []mdc.Ordering{mdc.NonDecreasing, mdc.StrictlyIncreasing} {
		var output bytes.Buffer
		writer, err := mdc.NewWriter(&output, testMetadata(ordering))
		if err != nil {
			t.Fatalf("new writer: %v", err)
		}
		if err := writer.WriteTick(mdc.Record{Timestamp: 10, BidTicks: 1}); err != nil {
			t.Fatalf("first tick: %v", err)
		}
		if err := writer.WriteTick(mdc.Record{Timestamp: 9, BidTicks: 1}); !errors.Is(err, mdc.ErrOrderingViolation) {
			t.Fatalf("ordering %d regression: got %v", ordering, err)
		}
	}

	var output bytes.Buffer
	writer, err := mdc.NewWriter(&output, testMetadata(mdc.StrictlyIncreasing))
	if err != nil {
		t.Fatalf("new strict writer: %v", err)
	}
	if err := writer.WriteTick(mdc.Record{Timestamp: 10}); err != nil {
		t.Fatalf("first strict tick: %v", err)
	}
	if err := writer.WriteTick(mdc.Record{Timestamp: 10}); !errors.Is(err, mdc.ErrOrderingViolation) {
		t.Fatalf("strict equality: got %v", err)
	}
}

func TestContainerDetectsHeaderPayloadIndexAndTruncationDamage(t *testing.T) {
	encoded := encodeContainer(t, testMetadata(mdc.NonDecreasing), []mdc.Record{
		{Timestamp: 10, BidTicks: 100},
		{Timestamp: 20, BidTicks: 101},
	}, mdc.DefaultWriterConfig())

	t.Run("header checksum", func(t *testing.T) {
		damaged := append([]byte(nil), encoded...)
		damaged[72] ^= 1
		if _, err := mdc.NewReader(bytes.NewReader(damaged)); !errors.Is(err, mdc.ErrChecksumMismatch) {
			t.Fatalf("got %v, want ErrChecksumMismatch", err)
		}
	})

	blockOffset := bytes.Index(encoded, []byte("MDBK"))
	indexOffset := bytes.Index(encoded, []byte("MDCI"))
	if blockOffset < 0 || indexOffset < 0 {
		t.Fatal("missing block or index magic")
	}
	t.Run("payload checksum", func(t *testing.T) {
		damaged := append([]byte(nil), encoded...)
		damaged[blockOffset+80] ^= 1
		reader, err := mdc.NewReader(bytes.NewReader(damaged))
		if err != nil {
			t.Fatalf("new reader: %v", err)
		}
		if err := reader.Verify(); !errors.Is(err, mdc.ErrChecksumMismatch) {
			t.Fatalf("got %v, want ErrChecksumMismatch", err)
		}
	})
	t.Run("index checksum", func(t *testing.T) {
		damaged := append([]byte(nil), encoded...)
		damaged[indexOffset+24] ^= 1
		if _, err := mdc.Open(bytes.NewReader(damaged)); !errors.Is(err, mdc.ErrChecksumMismatch) {
			t.Fatalf("got %v, want ErrChecksumMismatch", err)
		}
	})
	t.Run("truncated trailer", func(t *testing.T) {
		damaged := encoded[:len(encoded)-1]
		reader, err := mdc.NewReader(bytes.NewReader(damaged))
		if err != nil {
			t.Fatalf("new reader: %v", err)
		}
		if err := reader.Verify(); err == nil {
			t.Fatal("expected truncation error")
		}
	})
}

func TestContainerReaderLimits(t *testing.T) {
	config := mdc.DefaultWriterConfig()
	config.MaxBlockTicks = 4
	encoded := encodeContainer(t, testMetadata(mdc.NonDecreasing), []mdc.Record{{Timestamp: 1}}, config)
	limits := mdc.DefaultReaderLimits()
	limits.MaxBlockTicks = 2
	if _, err := mdc.NewReaderLimits(bytes.NewReader(encoded), limits); !errors.Is(err, mdc.ErrLimitExceeded) {
		t.Fatalf("got %v, want ErrLimitExceeded", err)
	}
}

func TestWriterRejectsBlockSizeAboveCanonicalReaderLimit(t *testing.T) {
	config := mdc.DefaultWriterConfig()
	config.MaxBlockTicks = 1<<20 + 1
	if _, err := mdc.NewWriterConfig(io.Discard, testMetadata(mdc.NonDecreasing), config); !errors.Is(err, mdc.ErrInvalidMetadata) {
		t.Fatalf("got %v, want ErrInvalidMetadata", err)
	}
}

func TestWriterRejectsNonCanonicalMetadataText(t *testing.T) {
	for _, mutate := range []func(*mdc.Metadata){
		func(metadata *mdc.Metadata) { metadata.Instrument = "BAD\nLOG" },
		func(metadata *mdc.Metadata) { metadata.Instrument = " PADDED" },
		func(metadata *mdc.Metadata) { metadata.PriceUnit = "" },
		func(metadata *mdc.Metadata) { metadata.PriceUnit = "USD\t" },
	} {
		metadata := testMetadata(mdc.NonDecreasing)
		mutate(&metadata)
		if _, err := mdc.NewWriter(io.Discard, metadata); !errors.Is(err, mdc.ErrInvalidMetadata) {
			t.Fatalf("metadata %+v: got %v, want ErrInvalidMetadata", metadata, err)
		}
	}
}

func TestEmptyFiniteContainer(t *testing.T) {
	encoded := encodeContainer(t, testMetadata(mdc.NonDecreasing), nil, mdc.DefaultWriterConfig())
	reader, err := mdc.NewReader(bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("new reader: %v", err)
	}
	if err := reader.Verify(); err != nil {
		t.Fatalf("verify empty: %v", err)
	}
	fileReader, err := mdc.Open(bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("open empty: %v", err)
	}
	if fileReader.BlockCount() != 0 {
		t.Fatalf("empty block count: %d", fileReader.BlockCount())
	}
}

func TestRecoverSkipsCorruptedBlockAndRebuildsCanonicalFile(t *testing.T) {
	metadata := testMetadata(mdc.NonDecreasing)
	config := mdc.DefaultWriterConfig()
	config.MaxBlockTicks = 2
	input := []mdc.Record{
		{Timestamp: 10, BidTicks: 100},
		{Timestamp: 20, BidTicks: 101},
		{Timestamp: 30, BidTicks: 102},
		{Timestamp: 40, BidTicks: 103},
		{Timestamp: 50, BidTicks: 104},
		{Timestamp: 60, BidTicks: 105},
	}
	encoded := encodeContainer(t, metadata, input, config)
	var blocks []int
	for offset := 0; ; {
		index := bytes.Index(encoded[offset:], []byte("MDBK"))
		if index < 0 {
			break
		}
		blocks = append(blocks, offset+index)
		offset += index + 4
	}
	if len(blocks) != 3 {
		t.Fatalf("block count in fixture: got %d, want 3", len(blocks))
	}
	damaged := append([]byte(nil), encoded...)
	damaged[blocks[1]+80] ^= 1

	var recovered bytes.Buffer
	report, err := mdc.Recover(bytes.NewReader(damaged), &recovered, mdc.DefaultReaderLimits())
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if report.RecoveredBlocks != 2 || report.RecoveredTicks != 4 || len(report.Damage) != 1 {
		t.Fatalf("report: %+v", report)
	}
	reader, err := mdc.NewReader(bytes.NewReader(recovered.Bytes()))
	if err != nil {
		t.Fatalf("new recovered reader: %v", err)
	}
	got := readAllRecords(t, reader)
	want := normalizeRecords([]mdc.Record{input[0], input[1], input[4], input[5]}, metadata.TickSize)
	if len(got) != len(want) {
		t.Fatalf("recovered count: got %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("recovered %d: got %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestRecoverRequiresValidMetadataHeader(t *testing.T) {
	encoded := encodeContainer(t, testMetadata(mdc.NonDecreasing), []mdc.Record{{Timestamp: 1}}, mdc.DefaultWriterConfig())
	encoded[72] ^= 1
	var output bytes.Buffer
	if _, err := mdc.Recover(bytes.NewReader(encoded), &output, mdc.DefaultReaderLimits()); !errors.Is(err, mdc.ErrChecksumMismatch) {
		t.Fatalf("got %v, want ErrChecksumMismatch", err)
	}
}

func TestCanonicalGoldenContainerIsDeterministic(t *testing.T) {
	metadata := mdc.Metadata{
		Instrument: "WINFUT:B3",
		PriceUnit:  "index-point",
		TimeUnit:   mdc.TimeMillisecond,
		Ordering:   mdc.SourceOrder,
		TickSize:   mdc.Rational{Num: 5, Den: 1},
		SpreadUnit: mdc.SpreadInTicks,
	}
	records := []mdc.Record{
		{Timestamp: 1_000_000, BidTicks: 34_910, Spread: 1, Flags: 2, Session: 20_260_817},
		{Timestamp: 1_000_010, BidTicks: 34_911, Spread: 31, Flags: 4660, Session: 20_260_817},
		{Timestamp: 1_066_000, BidTicks: 35_050, Spread: 2, Flags: 3, Session: 20_260_817},
		{Timestamp: 1_066_010, BidTicks: 35_049, Spread: 4, Flags: 5, Session: 20_260_818},
		{Timestamp: 1_065_900, BidTicks: 35_048, Spread: 6, Flags: 7, Session: 20_260_818, TickSize: mdc.Rational{Num: 5, Den: 2}},
	}
	config := mdc.DefaultWriterConfig()
	config.MaxBlockTicks = 2
	got := encodeContainer(t, metadata, records, config)
	want, err := os.ReadFile(filepath.Join("testdata", "golden-container.bin"))
	if err != nil {
		t.Fatalf("read golden container: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("canonical bytes changed: got %d bytes, want %d", len(got), len(want))
	}
}

func TestSingleFieldOverridesAreCanonical(t *testing.T) {
	metadata := testMetadata(mdc.NonDecreasing)
	records := []mdc.Record{
		{Timestamp: 1, BidTicks: 100, Spread: 31, Flags: 2, Session: 1},
		{Timestamp: 2, BidTicks: 101, Spread: 3, Flags: 0x1234, Session: 1},
	}
	encoded := encodeContainer(t, metadata, records, mdc.DefaultWriterConfig())
	reader, err := mdc.NewReader(bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	got := make([]mdc.Record, 2)
	if n, err := reader.ReadBatch(got); err != nil || n != len(got) {
		t.Fatalf("read batch: n=%d err=%v", n, err)
	}
	want := normalizeRecords(records, metadata.TickSize)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if err := reader.Verify(); err != nil {
		t.Fatal(err)
	}
}
