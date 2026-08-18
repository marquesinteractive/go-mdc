package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	mdc "github.com/marquesinteractive/go-mdc"
)

type marketState struct {
	ticks     uint64
	lastBid   int64
	imbalance int64
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: go run ./examples/02_market_replay_backtest <file.mdc>")
		os.Exit(2)
	}
	file, err := os.Open(os.Args[1])
	if err != nil {
		panic(err)
	}
	defer file.Close()
	reader, err := mdc.NewReader(file)
	if err != nil {
		panic(err)
	}

	var state marketState
	for {
		record, err := reader.ReadTick()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			panic(err)
		}
		state.ticks++
		state.lastBid = record.BidTicks
		if record.Flags&2 != 0 {
			state.imbalance++
		}
		if record.Flags&1 != 0 {
			state.imbalance--
		}
	}
	fmt.Printf("instrument=%s ticks=%d last_bid_ticks=%d imbalance=%d\n",
		reader.Metadata().Instrument, state.ticks, state.lastBid, state.imbalance)
}
