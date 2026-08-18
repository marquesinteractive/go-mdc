package mdc_test

import (
	"bytes"
	"io"
	"testing"

	mdc "github.com/marquesinteractive/go-mdc"
)

var benchmarkRecordSink mdc.Record

func benchmarkMetadata() mdc.Metadata {
	return mdc.Metadata{
		Instrument: "BENCH:TEST", PriceUnit: "quote-unit", TimeUnit: mdc.TimeNanosecond,
		Ordering: mdc.NonDecreasing, TickSize: mdc.Rational{Num: 1, Den: 100},
		SpreadUnit: mdc.SpreadInTicks,
	}
}

func benchmarkRecords(count int) []mdc.Record {
	records := make([]mdc.Record, count)
	for i := range records {
		spread := uint32(1)
		flags := uint32(i & 7)
		if i%257 == 0 {
			spread = 31
			flags = 0x1234
		}
		records[i] = mdc.Record{
			Timestamp: int64(i * 10), BidTicks: 100_000 + int64(i%101),
			Spread: spread, Flags: flags, Session: 1,
		}
	}
	return records
}

func encodeBenchmarkContainer(b *testing.B, records []mdc.Record) []byte {
	b.Helper()
	var output bytes.Buffer
	writer, err := mdc.NewWriter(&output, benchmarkMetadata())
	if err != nil {
		b.Fatal(err)
	}
	if _, err := writer.WriteBatch(records); err != nil {
		b.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		b.Fatal(err)
	}
	return output.Bytes()
}

func BenchmarkContainerWrite4096(b *testing.B) {
	records := benchmarkRecords(4096)
	encoded := encodeBenchmarkContainer(b, records)
	b.SetBytes(int64(len(encoded)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		writer, err := mdc.NewWriter(io.Discard, benchmarkMetadata())
		if err != nil {
			b.Fatal(err)
		}
		if _, err := writer.WriteBatch(records); err != nil {
			b.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(b.N*len(records))/b.Elapsed().Seconds(), "records/s")
}

func BenchmarkContainerRead4096(b *testing.B) {
	records := benchmarkRecords(4096)
	encoded := encodeBenchmarkContainer(b, records)
	b.SetBytes(int64(len(encoded)))
	b.ReportAllocs()
	b.ResetTimer()
	var sink mdc.Record
	for i := 0; i < b.N; i++ {
		reader, err := mdc.NewReader(bytes.NewReader(encoded))
		if err != nil {
			b.Fatal(err)
		}
		for {
			record, err := reader.ReadTick()
			if err == io.EOF {
				break
			}
			if err != nil {
				b.Fatal(err)
			}
			sink = record
		}
	}
	benchmarkRecordSink = sink
	b.StopTimer()
	b.ReportMetric(float64(b.N*len(records))/b.Elapsed().Seconds(), "records/s")
}

func BenchmarkIndexedSeekTimestamp4096(b *testing.B) {
	records := benchmarkRecords(4096)
	encoded := encodeBenchmarkContainer(b, records)
	b.ReportAllocs()
	b.ResetTimer()
	var sink mdc.Record
	for i := 0; i < b.N; i++ {
		reader, err := mdc.Open(bytes.NewReader(encoded))
		if err != nil {
			b.Fatal(err)
		}
		target := int64((i % len(records)) * 10)
		if err := reader.SeekTimestamp(target); err != nil {
			b.Fatal(err)
		}
		sink, err = reader.ReadTick()
		if err != nil {
			b.Fatal(err)
		}
	}
	benchmarkRecordSink = sink
}
