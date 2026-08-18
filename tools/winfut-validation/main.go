// Command winfut-validation validates a JSONL quote projection against MDC.
package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	mdc "github.com/marquesinteractive/go-mdc"
)

type sourceTick struct {
	Symbol    string `json:"symbol"`
	Bid       int64  `json:"bid"`
	Ask       int64  `json:"ask"`
	Volume    int64  `json:"volume"`
	Timestamp int64  `json:"timestamp"`
}

type rejectedTick struct {
	Line   uint64
	Reason string
}

type validationResult struct {
	InputPath            string
	OutputPath           string
	InputSHA256          string
	OutputSHA256         string
	InputBytes           int64
	OutputBytes          int64
	TotalLines           uint64
	Accepted             uint64
	Rejected             []rejectedTick
	Blocks               int
	Instrument           string
	PriceUnit            string
	TickSize             int64
	FirstTimestamp       int64
	LastTimestamp        int64
	TimestampRegressions uint64
}

func main() {
	input := flag.String("input", "", "source JSONL file")
	output := flag.String("output", "", "output MDC file")
	report := flag.String("report", "", "output Markdown report")
	symbol := flag.String("symbol", "WINFUT", "required source symbol")
	instrument := flag.String("instrument", "WINFUT:B3", "canonical instrument identifier")
	priceUnit := flag.String("price-unit", "index-point", "economic price unit")
	tickSize := flag.Int64("tick-size", 5, "positive integer source-price units per tick")
	flag.Parse()

	if *input == "" || *output == "" || *report == "" || *symbol == "" ||
		*instrument == "" || *priceUnit == "" || *tickSize <= 0 {
		flag.Usage()
		os.Exit(2)
	}
	result, err := validate(*input, *output, *symbol, *instrument, *priceUnit, *tickSize)
	if err != nil {
		fmt.Fprintln(os.Stderr, "winfut-validation:", err)
		os.Exit(1)
	}
	if err := writeReport(*report, result); err != nil {
		fmt.Fprintln(os.Stderr, "winfut-validation:", err)
		os.Exit(1)
	}
	fmt.Printf("validated %d accepted records in %d blocks; %d rejected\n", result.Accepted, result.Blocks, len(result.Rejected))
}

func validate(inputPath, outputPath, symbol, instrument, priceUnit string, tickSize int64) (validationResult, error) {
	result := validationResult{
		InputPath: inputPath, OutputPath: outputPath, Instrument: instrument,
		PriceUnit: priceUnit, TickSize: tickSize,
	}
	input, err := os.Open(inputPath)
	if err != nil {
		return result, err
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil {
		return result, err
	}
	result.InputBytes = info.Size()
	if _, err := os.Stat(outputPath); err == nil {
		return result, fmt.Errorf("output already exists: %s", outputPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return result, err
	}
	hasher := sha256.New()
	scanner := bufio.NewScanner(io.TeeReader(input, hasher))
	scanner.Buffer(make([]byte, 64*1024), 1<<20)

	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		return result, err
	}
	temporary, err := os.CreateTemp(filepath.Dir(outputPath), ".mdc-validation-*.tmp")
	if err != nil {
		return result, err
	}
	temporaryPath := temporary.Name()
	committed := false
	defer func() {
		_ = temporary.Close()
		if !committed {
			_ = os.Remove(temporaryPath)
		}
	}()

	metadata := mdc.Metadata{
		Instrument: instrument, PriceUnit: priceUnit, TimeUnit: mdc.TimeSecond,
		Ordering: mdc.SourceOrder, TickSize: mdc.Rational{Num: tickSize, Den: 1},
		SpreadUnit: mdc.SpreadInTicks,
	}
	writer, err := mdc.NewWriter(temporary, metadata)
	if err != nil {
		return result, err
	}
	accepted := make([]sourceTick, 0, 32*1024)
	var previousTimestamp int64
	var havePrevious bool
	for scanner.Scan() {
		result.TotalLines++
		raw := bytes.TrimSpace(scanner.Bytes())
		if len(raw) == 0 {
			return result, fmt.Errorf("line %d is empty", result.TotalLines)
		}
		var source sourceTick
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&source); err != nil {
			return result, fmt.Errorf("line %d: %w", result.TotalLines, err)
		}
		if err := ensureJSONEOF(decoder); err != nil {
			return result, fmt.Errorf("line %d: %w", result.TotalLines, err)
		}
		if source.Symbol != symbol {
			return result, fmt.Errorf("line %d: symbol %q, want %q", result.TotalLines, source.Symbol, symbol)
		}
		if source.Ask < source.Bid {
			result.Rejected = append(result.Rejected, rejectedTick{Line: result.TotalLines, Reason: "crossed quote (ask < bid)"})
			continue
		}
		if source.Bid%tickSize != 0 || source.Ask%tickSize != 0 {
			result.Rejected = append(result.Rejected, rejectedTick{Line: result.TotalLines, Reason: "price is not an exact tick-size multiple"})
			continue
		}
		spreadMagnitude := uint64(source.Ask) - uint64(source.Bid)
		spreadTicks := spreadMagnitude / uint64(tickSize)
		if spreadTicks > math.MaxUint32 {
			result.Rejected = append(result.Rejected, rejectedTick{Line: result.TotalLines, Reason: "spread exceeds uint32 tick range"})
			continue
		}
		if havePrevious && source.Timestamp < previousTimestamp {
			result.TimestampRegressions++
		}
		previousTimestamp = source.Timestamp
		havePrevious = true
		session, err := sessionUTC(source.Timestamp)
		if err != nil {
			return result, fmt.Errorf("line %d: %w", result.TotalLines, err)
		}
		record := mdc.Record{
			Timestamp: source.Timestamp, BidTicks: source.Bid / tickSize,
			Spread: uint32(spreadTicks), Session: session,
		}
		if err := writer.WriteTick(record); err != nil {
			return result, fmt.Errorf("line %d: %w", result.TotalLines, err)
		}
		accepted = append(accepted, source)
	}
	if err := scanner.Err(); err != nil {
		return result, err
	}
	result.InputSHA256 = hex.EncodeToString(hasher.Sum(nil))
	if len(accepted) == 0 {
		return result, errors.New("no encodable records")
	}
	result.Accepted = uint64(len(accepted))
	result.FirstTimestamp = accepted[0].Timestamp
	result.LastTimestamp = accepted[len(accepted)-1].Timestamp
	if err := writer.Close(); err != nil {
		return result, err
	}
	if err := temporary.Sync(); err != nil {
		return result, err
	}
	if err := temporary.Close(); err != nil {
		return result, err
	}
	if err := os.Rename(temporaryPath, outputPath); err != nil {
		return result, err
	}
	committed = true

	if err := verifyProjection(outputPath, accepted, tickSize); err != nil {
		return result, err
	}
	output, err := os.Open(outputPath)
	if err != nil {
		return result, err
	}
	indexed, err := mdc.Open(output)
	if err != nil {
		output.Close()
		return result, err
	}
	result.Blocks = indexed.BlockCount()
	if err := output.Close(); err != nil {
		return result, err
	}
	result.OutputSHA256, result.OutputBytes, err = hashFile(outputPath)
	return result, err
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); errors.Is(err, io.EOF) {
		return nil
	} else if err != nil {
		return err
	}
	return errors.New("multiple JSON values")
}

func sessionUTC(timestamp int64) (uint32, error) {
	date := time.Unix(timestamp, 0).UTC().Format("20060102")
	value, err := strconv.ParseUint(date, 10, 32)
	return uint32(value), err
}

func verifyProjection(path string, accepted []sourceTick, tickSize int64) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	reader, err := mdc.NewReader(file)
	if err != nil {
		return err
	}
	for index, source := range accepted {
		record, err := reader.ReadTick()
		if err != nil {
			return fmt.Errorf("round-trip record %d: %w", index, err)
		}
		bid := record.BidTicks * tickSize
		ask := bid + int64(record.Spread)*tickSize
		if record.Timestamp != source.Timestamp || bid != source.Bid || ask != source.Ask {
			return fmt.Errorf("round-trip mismatch at accepted record %d", index)
		}
	}
	if _, err := reader.ReadTick(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("round-trip trailing record or error: %v", err)
	}
	return nil
}

func hashFile(path string) (string, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	hasher := sha256.New()
	size, err := io.Copy(hasher, file)
	return hex.EncodeToString(hasher.Sum(nil)), size, err
}

func writeReport(path string, result validationResult) error {
	var rejected strings.Builder
	for _, item := range result.Rejected {
		fmt.Fprintf(&rejected, "- line %d: %s\n", item.Line, item.Reason)
	}
	if rejected.Len() == 0 {
		rejected.WriteString("- none\n")
	}
	text := fmt.Sprintf(`# WINFUT Projection Validation

Generated by `+"`tools/winfut-validation`"+`.

## Result

- Source: `+"`%s`"+`
- Source SHA-256: `+"`%s`"+`
- MDC artifact: `+"`%s`"+`
- MDC SHA-256: `+"`%s`"+`
- Source bytes: %d
- MDC bytes: %d
- Source lines: %d
- Accepted projection records: %d
- Rejected source records: %d
- Independent MDC blocks: %d
- Timestamp regressions preserved in source order: %d
- Instrument: `+"`%s`"+`
- Price unit: `+"`%s`"+`
- Tick size: `+"`%d/1`"+`
- Timestamp unit and epoch: whole seconds since Unix Epoch UTC
- First accepted timestamp: %d
- Last accepted timestamp: %d

## Projection boundary

The validation proves exact round-trip preservation of source symbol contract,
timestamp, bid, and ask for every accepted record. Ask is reconstructed as bid
plus integer spread ticks. Source `+"`volume`"+` is deliberately not encoded by the
current MDC record schema, so this is not a claim of full-feed losslessness.

## Rejections

%s`, result.InputPath, result.InputSHA256, result.OutputPath, result.OutputSHA256,
		result.InputBytes, result.OutputBytes, result.TotalLines, result.Accepted,
		len(result.Rejected), result.Blocks, result.TimestampRegressions,
		result.Instrument, result.PriceUnit, result.TickSize,
		result.FirstTimestamp, result.LastTimestamp, rejected.String())
	return atomicText(path, []byte(text))
}

func atomicText(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".validation-report-*.tmp")
	if err != nil {
		return err
	}
	name := temporary.Name()
	ok := false
	defer func() {
		_ = temporary.Close()
		if !ok {
			_ = os.Remove(name)
		}
	}()
	if _, err := temporary.Write(data); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	ok = true
	return nil
}
