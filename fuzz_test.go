package mdc_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/marquesinteractive/go-mdc"
)

func FuzzPackUnpack(f *testing.F) {
	f.Add(uint16(0), int8(0), uint8(0), uint8(0))
	f.Add(uint16(65535), int8(127), uint8(15), uint8(15))
	f.Add(uint16(65535), int8(-128), uint8(15), uint8(15))
	f.Add(uint16(1), int8(-1), uint8(1), uint8(1))
	f.Add(uint16(32768), int8(64), uint8(8), uint8(8))

	f.Fuzz(func(t *testing.T, deltaT uint16, deltaBid int8, spread uint8, flag uint8) {
		spread &= mdc.MaxSpread
		flag &= mdc.MaxFlag
		packed := mdc.Pack(deltaT, deltaBid, spread, flag)
		dt, db, sp, fl := mdc.Unpack(packed)
		if dt != deltaT || db != deltaBid || sp != spread || fl != flag {
			t.Fatalf("round trip mismatch: got (%d,%d,%d,%d), want (%d,%d,%d,%d)",
				dt, db, sp, fl, deltaT, deltaBid, spread, flag)
		}
	})
}

func FuzzPackedWordIdentity(f *testing.F) {
	f.Add(uint32(0))
	f.Add(uint32(0xFFFFFFFF))
	f.Add(uint32(0x21050019))
	f.Add(uint32(0x00800000))

	f.Fuzz(func(t *testing.T, word uint32) {
		dt, db, spread, flag := mdc.Unpack(word)
		if got := mdc.Pack(dt, db, spread, flag); got != word {
			t.Fatalf("word identity: got 0x%08X, want 0x%08X", got, word)
		}
	})
}

func FuzzPackedWordDecoder(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0x19, 0x00, 0x05, 0x21})
	f.Add([]byte{1, 2, 3})
	f.Add([]byte{1, 2, 3, 4, 5})

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 4096 {
			t.Skip()
		}
		decoder := mdc.NewPackedWordDecoder(bytes.NewReader(data))
		complete := len(data) / mdc.PackedWordSize
		for i := 0; i < complete; i++ {
			got, err := decoder.Decode()
			if err != nil {
				t.Fatalf("word %d: %v", i, err)
			}
			want := binary.LittleEndian.Uint32(data[i*mdc.PackedWordSize:])
			if got != want {
				t.Fatalf("word %d: got 0x%08X, want 0x%08X", i, got, want)
			}
		}

		_, err := decoder.Decode()
		if len(data)%mdc.PackedWordSize == 0 {
			if !errors.Is(err, io.EOF) {
				t.Fatalf("aligned terminal error: got %v, want EOF", err)
			}
		} else if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("truncated terminal error: got %v, want ErrUnexpectedEOF", err)
		}
	})
}

func FuzzContainerReader(f *testing.F) {
	golden, err := os.ReadFile(filepath.Join("testdata", "golden-container.bin"))
	if err != nil {
		f.Fatal(err)
	}
	f.Add(golden)
	f.Add([]byte("MDCF"))
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 64<<10 {
			t.Skip()
		}
		reader, err := mdc.NewReader(bytes.NewReader(data))
		if err == nil {
			_ = reader.Verify()
		}
	})
}

func FuzzContainerRoundTrip(f *testing.F) {
	f.Add(int64(0), int64(0), int64(1), int64(1), uint32(0), uint32(0), uint32(0))
	f.Add(int64(-1), int64(2), int64(-3), int64(4), uint32(31), uint32(4660), uint32(7))

	f.Fuzz(func(t *testing.T, timestampA, bidA, timestampB, bidB int64, spread, flags, session uint32) {
		metadata := mdc.Metadata{
			Instrument: "FUZZ:TEST", PriceUnit: "quote-unit", TimeUnit: mdc.TimeNanosecond,
			Ordering: mdc.SourceOrder, TickSize: mdc.Rational{Num: 1, Den: 1},
			SpreadUnit: mdc.SpreadInTicks,
		}
		want := []mdc.Record{
			{Timestamp: timestampA, BidTicks: bidA, Spread: spread, Flags: flags, Session: session},
			{Timestamp: timestampB, BidTicks: bidB, Spread: flags, Flags: spread, Session: session + 1},
		}
		var encoded bytes.Buffer
		writer, err := mdc.NewWriter(&encoded, metadata)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writer.WriteBatch(want); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		reader, err := mdc.NewReader(bytes.NewReader(encoded.Bytes()))
		if err != nil {
			t.Fatal(err)
		}
		for i := range want {
			got, err := reader.ReadTick()
			if err != nil {
				t.Fatal(err)
			}
			want[i].TickSize = metadata.TickSize
			if got != want[i] {
				t.Fatalf("record %d: got %+v, want %+v", i, got, want[i])
			}
		}
		if err := reader.Verify(); err != nil {
			t.Fatal(err)
		}
	})
}

func FuzzRecover(f *testing.F) {
	golden, err := os.ReadFile(filepath.Join("testdata", "golden-container.bin"))
	if err != nil {
		f.Fatal(err)
	}
	f.Add(golden)

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 64<<10 {
			t.Skip()
		}
		var output bytes.Buffer
		if _, err := mdc.Recover(bytes.NewReader(data), &output, mdc.DefaultReaderLimits()); err == nil {
			reader, err := mdc.NewReader(bytes.NewReader(output.Bytes()))
			if err != nil {
				t.Fatal(err)
			}
			if err := reader.Verify(); err != nil {
				t.Fatal(err)
			}
		}
	})
}
