package mdc_test

import (
	"bytes"
	"fmt"

	"github.com/marquesinteractive/go-mdc"
)

func ExamplePack() {
	// Pack 4 market parameters into a single 32-bit uint32
	deltaT := uint16(25) // 25 milliseconds since last tick
	deltaBid := int8(5)  // +5 price steps (e.g. +25 points on WINFUT)
	spread := uint8(1)   // 1 step spread
	flag := uint8(2)     // Aggressor flag (e.g. 2 = Buyer Aggression)

	packed := mdc.Pack(deltaT, deltaBid, spread, flag)
	fmt.Printf("Packed: 0x%08X\n", packed)

	// Output:
	// Packed: 0x21050019
}

func ExampleUnpack() {
	packed := uint32(0x21050019)

	deltaT, deltaBid, spread, flag := mdc.Unpack(packed)
	fmt.Printf("deltaT: %dms, deltaBid: %+d, spread: %d, flag: %d\n",
		deltaT, deltaBid, spread, flag)

	// Output:
	// deltaT: 25ms, deltaBid: +5, spread: 1, flag: 2
}

func ExampleDecodeWord() {
	packed := mdc.Pack(150, -10, 2, 4)
	tick := mdc.DecodeWord(packed)

	fmt.Printf("Decoded Tick: deltaT=%dms, deltaBid=%+d, spread=%d, flag=%d\n",
		tick.DeltaT, tick.DeltaBid, tick.Spread, tick.Flag)

	// Output:
	// Decoded Tick: deltaT=150ms, deltaBid=-10, spread=2, flag=4
}

func ExampleNewWriter() {
	metadata := mdc.Metadata{
		Instrument: "EXAMPLE:X",
		PriceUnit:  "index-point",
		TimeUnit:   mdc.TimeMillisecond,
		Ordering:   mdc.NonDecreasing,
		TickSize:   mdc.Rational{Num: 5, Den: 1},
		SpreadUnit: mdc.SpreadInTicks,
	}
	var output bytes.Buffer
	writer, _ := mdc.NewWriter(&output, metadata)
	_ = writer.WriteTick(mdc.Record{Timestamp: 1_000, BidTicks: 24_000, Spread: 1})
	_ = writer.Close()
	reader, _ := mdc.NewReader(bytes.NewReader(output.Bytes()))
	record, _ := reader.ReadTick()
	fmt.Printf("%s %d %d\n", reader.Metadata().Instrument, record.Timestamp, record.BidTicks)

	// Output:
	// EXAMPLE:X 1000 24000
}
