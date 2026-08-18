package main

import (
	"errors"
	"fmt"
	"io"
	"net"

	mdc "github.com/marquesinteractive/go-mdc"
)

func main() {
	server, client := net.Pipe()
	defer client.Close()
	errorsFromServer := make(chan error, 1)
	metadata := mdc.Metadata{
		Instrument: "EXAMPLE:X",
		PriceUnit:  "USD",
		TimeUnit:   mdc.TimeMicrosecond,
		Ordering:   mdc.NonDecreasing,
		TickSize:   mdc.Rational{Num: 1, Den: 100},
		SpreadUnit: mdc.SpreadInTicks,
	}

	go func() {
		defer server.Close()
		writer, err := mdc.NewStreamWriter(server, metadata)
		if err != nil {
			errorsFromServer <- err
			return
		}
		for i := int64(0); i < 10_000; i++ {
			record := mdc.Record{
				Timestamp: i * 10,
				BidTicks:  1_000_000 + i%5,
				Spread:    1,
				Flags:     uint32(i % 3),
				Session:   1,
			}
			if err := writer.WriteTick(record); err != nil {
				errorsFromServer <- err
				return
			}
		}
		errorsFromServer <- writer.Close()
	}()

	reader, err := mdc.NewReader(client)
	if err != nil {
		panic(err)
	}
	count := 0
	for {
		_, err := reader.ReadTick()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			panic(err)
		}
		count++
	}
	if err := <-errorsFromServer; err != nil {
		panic(err)
	}
	fmt.Printf("received=%d instrument=%s\n", count, reader.Metadata().Instrument)
}
