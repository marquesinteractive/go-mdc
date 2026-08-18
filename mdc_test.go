package mdc_test

import (
	"bytes"
	"encoding/binary"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/marquesinteractive/go-mdc"
)

var (
	benchmarkWord   uint32
	benchmarkSum    uint64
	benchmarkFields mdc.Delta
	benchmarkWords  []uint32
)

func TestPackUnpackRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		tick mdc.Delta
	}{
		{name: "zero", tick: mdc.Delta{}},
		{name: "maximum word", tick: mdc.Delta{DeltaT: 65535, DeltaBid: 127, Spread: 15, Flag: 15}},
		{name: "minimum signed byte", tick: mdc.Delta{DeltaBid: -128}},
		{name: "negative mid range", tick: mdc.Delta{DeltaT: 1234, DeltaBid: -50, Spread: 7, Flag: 3}},
		{name: "positive mid range", tick: mdc.Delta{DeltaT: 25, DeltaBid: 5, Spread: 1, Flag: 2}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			packed := tt.tick.Encode()
			if got := mdc.DecodeWord(packed); got != tt.tick {
				t.Fatalf("round trip: got %+v, want %+v", got, tt.tick)
			}
		})
	}
}

func TestPackRoundTripsEverySignedByte(t *testing.T) {
	for raw := 0; raw <= 255; raw++ {
		want := int8(uint8(raw))
		_, got, _, _ := mdc.Unpack(mdc.Pack(0, want, 0, 0))
		if got != want {
			t.Fatalf("raw byte 0x%02X: got %d, want %d", raw, got, want)
		}
	}
}

func TestExactBitAndByteLayout(t *testing.T) {
	const wantWord uint32 = 0x21050019
	gotWord := mdc.Pack(25, 5, 1, 2)
	if gotWord != wantWord {
		t.Fatalf("word: got 0x%08X, want 0x%08X", gotWord, wantWord)
	}

	var buf bytes.Buffer
	if err := mdc.NewPackedWordEncoder(&buf).Encode(gotWord); err != nil {
		t.Fatalf("encode: %v", err)
	}
	wantBytes := []byte{0x19, 0x00, 0x05, 0x21}
	if !bytes.Equal(buf.Bytes(), wantBytes) {
		t.Fatalf("bytes: got % X, want % X", buf.Bytes(), wantBytes)
	}
}

func TestGoldenVectors(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "golden-packed-words.tsv"))
	if err != nil {
		t.Fatalf("read vectors: %v", err)
	}
	reader := csv.NewReader(strings.NewReader(string(raw)))
	reader.Comma = '\t'
	reader.FieldsPerRecord = 7
	records, err := reader.ReadAll()
	if err != nil {
		t.Fatalf("parse vectors: %v", err)
	}
	if len(records) < 2 || strings.Join(records[0], "\t") != "name\tdelta_t\tdelta_bid\tspread\tflag\tword_hex\tbytes_le_hex" {
		t.Fatalf("invalid fixture header: %v", records)
	}
	for _, record := range records[1:] {
		t.Run(record[0], func(t *testing.T) {
			deltaT, err := strconv.ParseUint(record[1], 10, 16)
			if err != nil {
				t.Fatalf("delta_t: %v", err)
			}
			deltaBid, err := strconv.ParseInt(record[2], 10, 8)
			if err != nil {
				t.Fatalf("delta_bid: %v", err)
			}
			spread, err := strconv.ParseUint(record[3], 10, 8)
			if err != nil {
				t.Fatalf("spread: %v", err)
			}
			flag, err := strconv.ParseUint(record[4], 10, 8)
			if err != nil {
				t.Fatalf("flag: %v", err)
			}
			want64, err := strconv.ParseUint(record[5], 16, 32)
			if err != nil {
				t.Fatalf("word hex: %v", err)
			}
			wantWord := uint32(want64)
			gotWord := mdc.Pack(uint16(deltaT), int8(deltaBid), uint8(spread), uint8(flag))
			if gotWord != wantWord {
				t.Fatalf("word: got %08X, want %08X", gotWord, wantWord)
			}
			wantBytes, err := hex.DecodeString(record[6])
			if err != nil {
				t.Fatalf("bytes hex: %v", err)
			}
			gotBytes := make([]byte, mdc.PackedWordSize)
			binary.LittleEndian.PutUint32(gotBytes, gotWord)
			if !bytes.Equal(gotBytes, wantBytes) {
				t.Fatalf("bytes: got % X, want % X", gotBytes, wantBytes)
			}
		})
	}
}

func TestNormalSemanticValidationBeforeNarrowing(t *testing.T) {
	word, err := mdc.PackNormalChecked(65534, -127, 15, 15)
	if err != nil {
		t.Fatalf("normal boundary: %v", err)
	}
	if got := mdc.DecodeWord(word); got != (mdc.Delta{DeltaT: 65534, DeltaBid: -127, Spread: 15, Flag: 15}) {
		t.Fatalf("normal boundary decode: %+v", got)
	}

	for _, test := range []struct {
		name string
		dt   int64
		db   int64
		want error
	}{
		{name: "negative time", dt: -1, want: mdc.ErrDeltaTOutOfRange},
		{name: "escape time", dt: 65535, want: mdc.ErrDeltaTOutOfRange},
		{name: "below bid range", db: -128, want: mdc.ErrDeltaBidOutOfRange},
		{name: "above bid range", db: 128, want: mdc.ErrDeltaBidOutOfRange},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := mdc.PackNormalChecked(test.dt, test.db, 0, 0); !errors.Is(err, test.want) {
				t.Fatalf("got %v, want %v", err, test.want)
			}
		})
	}
}

func TestPackAbsoluteCheckedUsesWideArithmetic(t *testing.T) {
	word, err := mdc.PackAbsoluteChecked(1_000_000, 1_065_534, 50_000, 49_873, 2, 1)
	if err != nil {
		t.Fatalf("valid absolute delta: %v", err)
	}
	if got := mdc.DecodeWord(word); got != (mdc.Delta{DeltaT: 65534, DeltaBid: -127, Spread: 2, Flag: 1}) {
		t.Fatalf("absolute decode: %+v", got)
	}

	tests := []struct {
		name                      string
		previousTime, currentTime int64
		previousBid, currentBid   int64
		want                      error
	}{
		{name: "out of order", previousTime: 10, currentTime: 9, want: mdc.ErrDeltaTOutOfRange},
		{name: "time gap", previousTime: 10, currentTime: 65_545, want: mdc.ErrDeltaTOutOfRange},
		{name: "positive price jump", previousBid: 10, currentBid: 138, want: mdc.ErrDeltaBidOutOfRange},
		{name: "negative price jump", previousBid: 10, currentBid: -118, want: mdc.ErrDeltaBidOutOfRange},
		{name: "time subtraction overflow", previousTime: math.MinInt64, currentTime: math.MaxInt64, want: mdc.ErrDeltaTOutOfRange},
		{name: "bid subtraction overflow", previousBid: math.MinInt64, currentBid: math.MaxInt64, want: mdc.ErrDeltaBidOutOfRange},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := mdc.PackAbsoluteChecked(test.previousTime, test.currentTime, test.previousBid, test.currentBid, 0, 0); !errors.Is(err, test.want) {
				t.Fatalf("got %v, want %v", err, test.want)
			}
		})
	}
}

func TestCheckedValidationAndCompatibilityMasking(t *testing.T) {
	if _, err := mdc.PackChecked(0, 0, 16, 0); !errors.Is(err, mdc.ErrSpreadOutOfRange) {
		t.Fatalf("spread error: got %v", err)
	}
	if _, err := mdc.PackChecked(0, 0, 0, 16); !errors.Is(err, mdc.ErrFlagOutOfRange) {
		t.Fatalf("flag error: got %v", err)
	}
	if _, err := mdc.PackChecked(0, 0, 15, 15); err != nil {
		t.Fatalf("valid checked pack: %v", err)
	}

	_, _, spread, flag := mdc.Unpack(mdc.Pack(0, 0, 0xF2, 0xA5))
	if spread != 2 || flag != 5 {
		t.Fatalf("compatibility mask: got spread=%d flag=%d", spread, flag)
	}
}

func TestTickAndPackedTickAPI(t *testing.T) {
	tick := mdc.Delta{DeltaT: mdc.EscapeTime, DeltaBid: mdc.EscapePrice, Spread: 2, Flag: 5}
	if err := tick.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if !tick.UsesEscapeMarker() {
		t.Fatal("expected escape marker")
	}
	if err := tick.ValidateNormal(); !errors.Is(err, mdc.ErrDeltaTOutOfRange) {
		t.Fatalf("normal validation: got %v, want ErrDeltaTOutOfRange", err)
	}

	packed := tick.Pack()
	if packed.Decode() != tick || packed.Uint32() != tick.Encode() {
		t.Fatalf("packed type round trip failed: packed=%08X", packed.Uint32())
	}
}

func TestPackDeltasAndUnpackWords(t *testing.T) {
	src := []mdc.Delta{
		{DeltaT: 1, DeltaBid: -1, Spread: 1, Flag: 1},
		{DeltaT: 2, DeltaBid: 2, Spread: 2, Flag: 2},
		{DeltaT: 3, DeltaBid: -3, Spread: 3, Flag: 3},
	}
	packed := make([]uint32, 2)
	if got := mdc.PackDeltas(packed, src); got != 2 {
		t.Fatalf("packed count: got %d, want 2", got)
	}
	decoded := make([]mdc.Delta, 3)
	if got := mdc.UnpackWords(decoded, packed); got != 2 {
		t.Fatalf("unpacked count: got %d, want 2", got)
	}
	if decoded[0] != src[0] || decoded[1] != src[1] {
		t.Fatalf("batch round trip: got %+v", decoded[:2])
	}
}

func TestFileIORoundTrip(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "ticks.mdc")
	ticks := []uint32{
		mdc.Pack(10, 5, 2, 1),
		mdc.Pack(20, -3, 4, 3),
		mdc.Pack(65535, 127, 15, 15),
		mdc.Pack(0, -128, 0, 0),
	}

	if err := mdc.WritePackedWordsFile(filename, ticks); err != nil {
		t.Fatalf("write: %v", err)
	}
	raw, err := os.ReadFile(filename)
	if err != nil {
		t.Fatalf("read raw file: %v", err)
	}
	if len(raw) != len(ticks)*mdc.PackedWordSize {
		t.Fatalf("file size: got %d, want %d", len(raw), len(ticks)*mdc.PackedWordSize)
	}
	for i, tick := range ticks {
		if got := binary.LittleEndian.Uint32(raw[i*mdc.PackedWordSize:]); got != tick {
			t.Fatalf("raw tick %d: got %08X, want %08X", i, got, tick)
		}
	}

	got, err := mdc.ReadPackedWordsFile(filename)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != len(ticks) {
		t.Fatalf("tick count: got %d, want %d", len(got), len(ticks))
	}
	for i := range ticks {
		if got[i] != ticks[i] {
			t.Fatalf("tick %d: got %08X, want %08X", i, got[i], ticks[i])
		}
	}
}

func TestReadFileRejectsMisalignedInput(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "truncated.mdc")
	if err := os.WriteFile(filename, []byte{1, 2, 3}, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if _, err := mdc.ReadPackedWordsFile(filename); !errors.Is(err, mdc.ErrPackedWordAlignment) {
		t.Fatalf("got %v, want ErrPackedWordAlignment", err)
	}
}

func TestEmptyAndInvalidFileOperations(t *testing.T) {
	if err := mdc.WritePackedWords(io.Discard, nil); err != nil {
		t.Fatalf("empty write: %v", err)
	}
	if err := mdc.WritePackedWordsBuffer(io.Discard, nil, nil); err != nil {
		t.Fatalf("empty buffered write: %v", err)
	}

	empty := filepath.Join(t.TempDir(), "empty.mdc")
	if err := mdc.WritePackedWordsFile(empty, nil); err != nil {
		t.Fatalf("empty file write: %v", err)
	}
	ticks, err := mdc.ReadPackedWordsFile(empty)
	if err != nil || len(ticks) != 0 {
		t.Fatalf("empty file read: len=%d err=%v", len(ticks), err)
	}
	if _, err := mdc.ReadPackedWordsFile(filepath.Join(t.TempDir(), "missing.mdc")); err == nil {
		t.Fatal("expected missing-file error")
	}
	if err := mdc.WritePackedWordsFile(t.TempDir(), []uint32{1}); err == nil {
		t.Fatal("expected directory write error")
	}
}

func TestReadFileLimitGuardsAllocation(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "ticks.mdc")
	if err := mdc.WritePackedWordsFile(filename, []uint32{1, 2}); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if _, err := mdc.ReadPackedWordsFileLimit(filename, 1); !errors.Is(err, mdc.ErrPackedWordLimitExceeded) {
		t.Fatalf("got %v, want ErrPackedWordLimitExceeded", err)
	}
	ticks, err := mdc.ReadPackedWordsFileLimit(filename, 2)
	if err != nil || len(ticks) != 2 {
		t.Fatalf("exact limit: len=%d err=%v", len(ticks), err)
	}
}

func TestStreamingEOFAndTruncation(t *testing.T) {
	packed := mdc.Pack(100, -2, 3, 4)
	var encoded bytes.Buffer
	if err := mdc.NewPackedWordEncoder(&encoded).Encode(packed); err != nil {
		t.Fatalf("encode: %v", err)
	}

	decoder := mdc.NewPackedWordDecoder(&encoded)
	got, err := decoder.Decode()
	if err != nil || got != packed {
		t.Fatalf("decode: got %08X, %v", got, err)
	}
	if _, err := decoder.Decode(); !errors.Is(err, io.EOF) {
		t.Fatalf("terminal error: got %v, want EOF", err)
	}

	for length := 1; length < mdc.PackedWordSize; length++ {
		decoder := mdc.NewPackedWordDecoder(bytes.NewReader([]byte{1, 2, 3}[:length]))
		if _, err := decoder.Decode(); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("length %d: got %v, want ErrUnexpectedEOF", length, err)
		}
	}
}

func TestStreamingHandlesPartialReads(t *testing.T) {
	want := mdc.Pack(25, -7, 3, 9)
	var raw [mdc.PackedWordSize]byte
	binary.LittleEndian.PutUint32(raw[:], want)
	decoder := mdc.NewPackedWordDecoder(&chunkReader{reader: bytes.NewReader(raw[:]), max: 1})
	got, err := decoder.Decode()
	if err != nil || got != want {
		t.Fatalf("partial read: got=%08X err=%v, want=%08X", got, err, want)
	}
}

func TestStreamingHandlesPartialWrites(t *testing.T) {
	w := &chunkWriter{max: 1}
	packed := mdc.Pack(25, 5, 1, 2)
	if err := mdc.NewPackedWordEncoder(w).Encode(packed); err != nil {
		t.Fatalf("encode: %v", err)
	}
	if got := binary.LittleEndian.Uint32(w.Bytes()); got != packed {
		t.Fatalf("got %08X, want %08X", got, packed)
	}

	if err := mdc.NewPackedWordEncoder(zeroWriter{}).Encode(packed); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("zero-progress writer: got %v, want ErrShortWrite", err)
	}
	if err := mdc.NewPackedWordEncoder(invalidCountWriter{}).Encode(packed); !errors.Is(err, mdc.ErrInvalidWrite) {
		t.Fatalf("invalid-count writer: got %v, want ErrInvalidWrite", err)
	}
}

func TestStreamTickHelpers(t *testing.T) {
	want := mdc.Delta{DeltaT: 42, DeltaBid: -7, Spread: 3, Flag: 9}
	var buf bytes.Buffer
	encoder := mdc.NewPackedWordEncoder(&buf)
	if err := encoder.EncodeDelta(want); err != nil {
		t.Fatalf("encode tick: %v", err)
	}
	if err := encoder.EncodeDelta(mdc.Delta{Spread: 16}); !errors.Is(err, mdc.ErrSpreadOutOfRange) {
		t.Fatalf("invalid spread: got %v", err)
	}
	if err := encoder.EncodeDelta(mdc.Delta{Flag: 16}); !errors.Is(err, mdc.ErrFlagOutOfRange) {
		t.Fatalf("invalid flag: got %v", err)
	}

	got, err := mdc.NewPackedWordDecoder(&buf).DecodeDelta()
	if err != nil || got != want {
		t.Fatalf("decode tick: got=%+v err=%v", got, err)
	}
	if _, err := mdc.NewPackedWordDecoder(bytes.NewReader(nil)).DecodeDelta(); !errors.Is(err, io.EOF) {
		t.Fatalf("decode EOF: got %v", err)
	}
}

func TestWritePropagatesWriterFailure(t *testing.T) {
	wantErr := errors.New("storage failure")
	w := &failingWriter{remaining: 5, err: wantErr}
	err := mdc.WritePackedWords(w, []uint32{1, 2, 3})
	if !errors.Is(err, wantErr) {
		t.Fatalf("got %v, want %v", err, wantErr)
	}
}

func TestWriteBufferValidationAndRoundTrip(t *testing.T) {
	if err := mdc.WritePackedWordsBuffer(io.Discard, []uint32{1}, make([]byte, 3)); !errors.Is(err, mdc.ErrPackedWordBufferTooSmall) {
		t.Fatalf("small buffer: got %v, want ErrPackedWordBufferTooSmall", err)
	}
	ticks := []uint32{0x21050019, 0xFF80FFFF, 0x12345678}
	var output bytes.Buffer
	if err := mdc.WritePackedWordsBuffer(&output, ticks, make([]byte, 7)); err != nil {
		t.Fatalf("write buffer: %v", err)
	}
	if output.Len() != len(ticks)*mdc.PackedWordSize {
		t.Fatalf("output bytes: got %d, want %d", output.Len(), len(ticks)*mdc.PackedWordSize)
	}
	for i, want := range ticks {
		if got := binary.LittleEndian.Uint32(output.Bytes()[i*mdc.PackedWordSize:]); got != want {
			t.Fatalf("tick %d: got %08X, want %08X", i, got, want)
		}
	}
}

func TestRandomizedRoundTrip(t *testing.T) {
	r := rand.New(rand.NewSource(42))
	for i := 0; i < 100_000; i++ {
		want := mdc.Delta{
			DeltaT:   uint16(r.Intn(1 << 16)),
			DeltaBid: int8(r.Intn(1<<8) - 128),
			Spread:   uint8(r.Intn(1 << 4)),
			Flag:     uint8(r.Intn(1 << 4)),
		}
		if got := mdc.DecodeWord(want.Encode()); got != want {
			t.Fatalf("iteration %d: got %+v, want %+v", i, got, want)
		}
	}
}

func TestHotPathAllocations(t *testing.T) {
	packAllocs := testing.AllocsPerRun(1_000, func() {
		benchmarkWord = mdc.Pack(150, -5, 2, 1)
	})
	if packAllocs != 0 {
		t.Fatalf("Pack allocations: got %v, want 0", packAllocs)
	}

	unpackAllocs := testing.AllocsPerRun(1_000, func() {
		dt, db, sp, fl := mdc.Unpack(benchmarkWord)
		benchmarkFields = mdc.Delta{DeltaT: dt, DeltaBid: db, Spread: sp, Flag: fl}
	})
	if unpackAllocs != 0 {
		t.Fatalf("Unpack allocations: got %v, want 0", unpackAllocs)
	}

	w := &fixedWriter{}
	encoder := mdc.NewPackedWordEncoder(w)
	var encodeErr error
	streamAllocs := testing.AllocsPerRun(1_000, func() {
		encodeErr = encoder.Encode(benchmarkWord)
	})
	if encodeErr != nil {
		t.Fatalf("stream encode: %v", encodeErr)
	}
	if streamAllocs != 0 {
		t.Fatalf("PackedWordEncoder.Encode allocations: got %v, want 0", streamAllocs)
	}
}

func BenchmarkPack(b *testing.B) {
	var result uint32
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		result ^= mdc.Pack(uint16(i), int8(i>>8), uint8(i>>16), uint8(i>>20))
	}
	benchmarkWord = result
}

func BenchmarkUnpack(b *testing.B) {
	var sum uint64
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dt, db, spread, flag := mdc.Unpack(uint32(i) * 0x9E3779B1)
		sum += uint64(dt) + uint64(uint8(db)) + uint64(spread) + uint64(flag)
	}
	benchmarkSum = sum
}

func BenchmarkPackParallel(b *testing.B) {
	var sink atomic.Uint64
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		var i uint32
		var local uint32
		for pb.Next() {
			local ^= mdc.Pack(uint16(i), int8(i>>8), uint8(i>>16), uint8(i>>20))
			i++
		}
		sink.Add(uint64(local))
	})
	benchmarkSum = sink.Load()
}

func BenchmarkUnpackParallel(b *testing.B) {
	var sink atomic.Uint64
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		var i uint32
		var local uint64
		for pb.Next() {
			dt, db, spread, flag := mdc.Unpack(i * 0x9E3779B1)
			local += uint64(dt) + uint64(uint8(db)) + uint64(spread) + uint64(flag)
			i++
		}
		sink.Add(local)
	})
	benchmarkSum = sink.Load()
}

func BenchmarkPackDeltas(b *testing.B) {
	const batchSize = 4096
	src := make([]mdc.Delta, batchSize)
	dst := make([]uint32, batchSize)
	for i := range src {
		src[i] = mdc.Delta{DeltaT: uint16(i), DeltaBid: int8(i), Spread: uint8(i & 15), Flag: uint8((i >> 4) & 15)}
	}
	b.SetBytes(batchSize * mdc.PackedWordSize)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		mdc.PackDeltas(dst, src)
	}
	benchmarkWords = dst
}

func BenchmarkWritePackedWords(b *testing.B) {
	ticks := make([]uint32, 16_384)
	for i := range ticks {
		ticks[i] = uint32(i) * 0x9E3779B1
	}
	b.SetBytes(int64(len(ticks) * mdc.PackedWordSize))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := mdc.WritePackedWords(io.Discard, ticks); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkWritePackedWordsBuffer(b *testing.B) {
	ticks := make([]uint32, 16_384)
	for i := range ticks {
		ticks[i] = uint32(i) * 0x9E3779B1
	}
	buf := make([]byte, 64*1024)
	b.SetBytes(int64(len(ticks) * mdc.PackedWordSize))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := mdc.WritePackedWordsBuffer(io.Discard, ticks, buf); err != nil {
			b.Fatal(err)
		}
	}
}

type chunkWriter struct {
	bytes.Buffer
	max int
}

type chunkReader struct {
	reader *bytes.Reader
	max    int
}

func (r *chunkReader) Read(p []byte) (int, error) {
	if len(p) > r.max {
		p = p[:r.max]
	}
	return r.reader.Read(p)
}

func (w *chunkWriter) Write(p []byte) (int, error) {
	if len(p) > w.max {
		p = p[:w.max]
	}
	return w.Buffer.Write(p)
}

type zeroWriter struct{}

func (zeroWriter) Write([]byte) (int, error) { return 0, nil }

type invalidCountWriter struct{}

func (invalidCountWriter) Write(p []byte) (int, error) { return len(p) + 1, nil }

type failingWriter struct {
	remaining int
	err       error
}

func (w *failingWriter) Write(p []byte) (int, error) {
	if w.remaining <= 0 {
		return 0, w.err
	}
	n := min(len(p), w.remaining)
	w.remaining -= n
	if n < len(p) {
		return n, w.err
	}
	return n, nil
}

type fixedWriter struct {
	buf [mdc.PackedWordSize]byte
}

func (w *fixedWriter) Write(p []byte) (int, error) {
	copy(w.buf[:], p)
	return len(p), nil
}
