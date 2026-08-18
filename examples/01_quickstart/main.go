package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	mdc "github.com/marquesinteractive/go-mdc"
)

func main() {
	metadata := mdc.Metadata{
		Instrument: "EXAMPLE:X",
		PriceUnit:  "index-point",
		TimeUnit:   mdc.TimeMillisecond,
		Ordering:   mdc.NonDecreasing,
		TickSize:   mdc.Rational{Num: 5, Den: 1},
		SpreadUnit: mdc.SpreadInTicks,
	}
	records := []mdc.Record{
		{Timestamp: 1_000, BidTicks: 24_000, Spread: 1, Flags: 2, Session: 1},
		{Timestamp: 1_012, BidTicks: 24_002, Spread: 1, Flags: 2, Session: 1},
		{Timestamp: 90_000, BidTicks: 25_000, Spread: 31, Flags: 0x120, Session: 2},
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
	fmt.Printf("instrument=%s bytes=%d\n", reader.Metadata().Instrument, encoded.Len())
	for {
		record, err := reader.ReadTick()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			panic(err)
		}
		fmt.Printf("ts=%d bid=%d spread=%d flags=%d session=%d tick=%d/%d\n",
			record.Timestamp, record.BidTicks, record.Spread, record.Flags,
			record.Session, record.TickSize.Num, record.TickSize.Den)
	}
}
