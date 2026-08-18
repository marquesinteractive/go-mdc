package main

import (
	"bufio"
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	mdc "github.com/marquesinteractive/go-mdc"
)

var version = "1.0.0"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printHelp(stdout)
		return 0
	}

	var err error
	switch args[0] {
	case "encode":
		err = runEncode(args[1:], stdout, stderr)
	case "decode":
		err = runDecode(args[1:], stdout)
	case "inspect":
		if len(args) != 2 {
			fmt.Fprintln(stderr, "usage: mdc inspect <file.mdc>")
			return 2
		}
		err = inspectFile(args[1], stdout)
	case "verify":
		if len(args) != 2 {
			fmt.Fprintln(stderr, "usage: mdc verify <file.mdc>")
			return 2
		}
		err = verifyFile(args[1], stdout)
	case "recover":
		if len(args) != 3 {
			fmt.Fprintln(stderr, "usage: mdc recover <damaged.mdc> <recovered.mdc>")
			return 2
		}
		err = recoverFile(args[1], args[2], stdout)
	case "bench", "benchmark":
		err = runBenchmark(stdout)
	case "version", "-v", "--version":
		fmt.Fprintf(stdout, "MDC CLI version %s (MarquesInteractive)\n", version)
		return 0
	case "help", "-h", "--help":
		printHelp(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown command %q\n", args[0])
		printHelp(stderr)
		return 2
	}
	if err != nil {
		fmt.Fprintf(stderr, "mdc %s: %v\n", args[0], err)
		return 1
	}
	return 0
}

func printHelp(w io.Writer) {
	fmt.Fprint(w, `MDC: self-contained blocked market-data codec

Usage:
  mdc <command> [arguments]

Commands:
  encode [options] <input.csv> <output.mdc>
  decode <input.mdc> <output.csv>
  inspect <file.mdc>
  verify <file.mdc>
  recover <damaged.mdc> <recovered.mdc>
  bench
  version

Encode requires --instrument and --price-unit, and accepts:
  --time-unit ns|us|ms|s       (default: ms)
  --ordering source|nondecreasing|strict
  --tick-size NUM/DEN          (default: 1/1)
  --block-ticks N              (default: 4096)

CSV schema:
  timestamp,bid_ticks,spread,flags,session
or:
  timestamp,bid_ticks,spread,flags,session,tick_size_num,tick_size_den

The extended tick-size fields must be both blank or both present. Commands that
write files reject existing outputs and commit through a temporary file.
`)
}

func runEncode(args []string, stdout, stderr io.Writer) error {
	set := flag.NewFlagSet("encode", flag.ContinueOnError)
	set.SetOutput(stderr)
	instrument := set.String("instrument", "", "instrument identifier")
	priceUnit := set.String("price-unit", "", "economic price unit, for example BRL, USD, or index-point")
	timeUnitText := set.String("time-unit", "ms", "ns, us, ms, or s")
	orderingText := set.String("ordering", "source", "source, nondecreasing, or strict")
	tickSizeText := set.String("tick-size", "1/1", "positive rational NUM/DEN")
	blockTicks := set.Uint("block-ticks", 4096, "maximum ticks per independent block")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 2 {
		return errors.New("usage: mdc encode [options] <input.csv> <output.mdc>")
	}
	if *instrument == "" {
		return errors.New("--instrument is required")
	}
	if *priceUnit == "" {
		return errors.New("--price-unit is required")
	}
	timeUnit, err := parseTimeUnit(*timeUnitText)
	if err != nil {
		return err
	}
	ordering, err := parseOrdering(*orderingText)
	if err != nil {
		return err
	}
	tickSize, err := parseTickSize(*tickSizeText)
	if err != nil {
		return err
	}
	if *blockTicks == 0 || uint64(*blockTicks) > uint64(^uint32(0)) {
		return fmt.Errorf("invalid --block-ticks %d", *blockTicks)
	}

	inputPath, outputPath := set.Arg(0), set.Arg(1)
	input, err := os.Open(inputPath)
	if err != nil {
		return fmt.Errorf("open input: %w", err)
	}
	defer input.Close()

	metadata := mdc.Metadata{
		Instrument: *instrument,
		PriceUnit:  *priceUnit,
		TimeUnit:   timeUnit,
		Ordering:   ordering,
		TickSize:   tickSize,
		SpreadUnit: mdc.SpreadInTicks,
	}
	config := mdc.DefaultWriterConfig()
	config.MaxBlockTicks = uint32(*blockTicks)
	var count uint64
	err = atomicOutput(outputPath, func(output *os.File) error {
		writer, err := mdc.NewWriterConfig(output, metadata, config)
		if err != nil {
			return err
		}
		reader := csv.NewReader(bufio.NewReaderSize(input, 64*1024))
		reader.FieldsPerRecord = -1
		reader.ReuseRecord = true
		reader.TrimLeadingSpace = true

		header, err := reader.Read()
		if err != nil {
			return fmt.Errorf("read CSV header: %w", err)
		}
		extended, err := parseCSVHeader(header)
		if err != nil {
			return err
		}
		line := 1
		for {
			record, readErr := reader.Read()
			if errors.Is(readErr, io.EOF) {
				break
			}
			line++
			if readErr != nil {
				return fmt.Errorf("CSV line %d: %w", line, readErr)
			}
			tick, err := parseCSVRecord(record, extended)
			if err != nil {
				return fmt.Errorf("CSV line %d: %w", line, err)
			}
			if err := writer.WriteTick(tick); err != nil {
				return fmt.Errorf("CSV line %d: %w", line, err)
			}
			count++
		}
		return writer.Close()
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Encoded %s ticks into %q.\n", formatNumber(count), outputPath)
	return nil
}

func runDecode(args []string, stdout io.Writer) error {
	if len(args) != 2 {
		return errors.New("usage: mdc decode <input.mdc> <output.csv>")
	}
	input, err := os.Open(args[0])
	if err != nil {
		return fmt.Errorf("open input: %w", err)
	}
	defer input.Close()
	reader, err := mdc.NewReader(input)
	if err != nil {
		return err
	}
	var count uint64
	err = atomicOutput(args[1], func(output *os.File) error {
		buffered := bufio.NewWriterSize(output, 64*1024)
		csvWriter := csv.NewWriter(buffered)
		if err := csvWriter.Write([]string{"timestamp", "bid_ticks", "spread", "flags", "session", "tick_size_num", "tick_size_den"}); err != nil {
			return err
		}
		for {
			record, readErr := reader.ReadTick()
			if errors.Is(readErr, io.EOF) {
				break
			}
			if readErr != nil {
				return readErr
			}
			row := []string{
				strconv.FormatInt(record.Timestamp, 10),
				strconv.FormatInt(record.BidTicks, 10),
				strconv.FormatUint(uint64(record.Spread), 10),
				strconv.FormatUint(uint64(record.Flags), 10),
				strconv.FormatUint(uint64(record.Session), 10),
				strconv.FormatInt(record.TickSize.Num, 10),
				strconv.FormatUint(record.TickSize.Den, 10),
			}
			if err := csvWriter.Write(row); err != nil {
				return err
			}
			count++
		}
		csvWriter.Flush()
		if err := csvWriter.Error(); err != nil {
			return err
		}
		return buffered.Flush()
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Decoded %s ticks into %q.\n", formatNumber(count), args[1])
	return nil
}

func inspectFile(path string, output io.Writer) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	indexed, err := mdc.Open(file)
	if err != nil {
		return err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	reader, err := mdc.NewReader(file)
	if err != nil {
		return err
	}
	metadata := reader.Metadata()
	var sample []mdc.Record
	var count uint64
	for {
		record, readErr := reader.ReadTick()
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return readErr
		}
		if len(sample) < 5 {
			sample = append(sample, record)
		}
		count++
	}
	fmt.Fprintf(output, "File:       %s\n", path)
	fmt.Fprintf(output, "Instrument: %s\n", metadata.Instrument)
	fmt.Fprintf(output, "Price unit: %s\n", metadata.PriceUnit)
	fmt.Fprintf(output, "Time unit:  %s\n", formatTimeUnit(metadata.TimeUnit))
	fmt.Fprintf(output, "Ordering:   %s\n", formatOrdering(metadata.Ordering))
	fmt.Fprintf(output, "Tick size:  %d/%d\n", metadata.TickSize.Num, metadata.TickSize.Den)
	fmt.Fprintf(output, "Blocks:     %s\n", formatNumber(uint64(indexed.BlockCount())))
	fmt.Fprintf(output, "Ticks:      %s\n", formatNumber(count))
	fmt.Fprintln(output, "Integrity:  verified (headers, blocks, CRC32C, index, trailer)")
	for i, record := range sample {
		fmt.Fprintf(output, "  [%02d] ts=%d bid=%d spread=%d flags=%d session=%d tick=%d/%d\n",
			i+1, record.Timestamp, record.BidTicks, record.Spread, record.Flags,
			record.Session, record.TickSize.Num, record.TickSize.Den)
	}
	return nil
}

func verifyFile(path string, output io.Writer) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	reader, err := mdc.NewReader(file)
	if err != nil {
		return err
	}
	if err := reader.Verify(); err != nil {
		return err
	}
	fmt.Fprintf(output, "%q is a valid MDC container.\n", path)
	return nil
}

func recoverFile(inputPath, outputPath string, output io.Writer) error {
	input, err := os.Open(inputPath)
	if err != nil {
		return err
	}
	defer input.Close()
	var report mdc.RecoveryReport
	err = atomicOutput(outputPath, func(destination *os.File) error {
		var recoverErr error
		report, recoverErr = mdc.Recover(input, destination, mdc.DefaultReaderLimits())
		return recoverErr
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(output, "Recovered %s ticks from %s valid blocks into %q.\n",
		formatNumber(report.RecoveredTicks), formatNumber(report.RecoveredBlocks), outputPath)
	fmt.Fprintf(output, "Discarded regions: %d.\n", len(report.Damage))
	for _, damage := range report.Damage {
		fmt.Fprintf(output, "  bytes [%d,%d)\n", damage.Start, damage.End)
	}
	return nil
}

func atomicOutput(path string, write func(*os.File) error) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("output already exists: %s", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect output: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".mdc-write-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	committed := false
	defer func() {
		_ = temporary.Close()
		if !committed {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := write(temporary); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync temporary output: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary output: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("commit output: %w", err)
	}
	committed = true
	return nil
}

func parseCSVHeader(record []string) (bool, error) {
	base := []string{"timestamp", "bid_ticks", "spread", "flags", "session"}
	extended := append(append([]string(nil), base...), "tick_size_num", "tick_size_den")
	normalized := make([]string, len(record))
	for i, value := range record {
		normalized[i] = normalizeHeader(value)
	}
	if equalStrings(normalized, base) {
		return false, nil
	}
	if equalStrings(normalized, extended) {
		return true, nil
	}
	return false, fmt.Errorf("unsupported CSV header %q", strings.Join(record, ","))
}

func parseCSVRecord(record []string, extended bool) (mdc.Record, error) {
	wantFields := 5
	if extended {
		wantFields = 7
	}
	if len(record) != wantFields {
		return mdc.Record{}, fmt.Errorf("expected %d fields, got %d", wantFields, len(record))
	}
	timestamp, err := strconv.ParseInt(strings.TrimSpace(record[0]), 10, 64)
	if err != nil {
		return mdc.Record{}, fmt.Errorf("timestamp: %w", err)
	}
	bid, err := strconv.ParseInt(strings.TrimSpace(record[1]), 10, 64)
	if err != nil {
		return mdc.Record{}, fmt.Errorf("bid_ticks: %w", err)
	}
	spread, err := strconv.ParseUint(strings.TrimSpace(record[2]), 10, 32)
	if err != nil {
		return mdc.Record{}, fmt.Errorf("spread: %w", err)
	}
	flags, err := strconv.ParseUint(strings.TrimSpace(record[3]), 10, 32)
	if err != nil {
		return mdc.Record{}, fmt.Errorf("flags: %w", err)
	}
	session, err := strconv.ParseUint(strings.TrimSpace(record[4]), 10, 32)
	if err != nil {
		return mdc.Record{}, fmt.Errorf("session: %w", err)
	}
	tick := mdc.Record{Timestamp: timestamp, BidTicks: bid, Spread: uint32(spread), Flags: uint32(flags), Session: uint32(session)}
	if extended {
		numText, denText := strings.TrimSpace(record[5]), strings.TrimSpace(record[6])
		if (numText == "") != (denText == "") {
			return mdc.Record{}, errors.New("tick-size numerator and denominator must be both blank or both present")
		}
		if numText != "" {
			num, err := strconv.ParseInt(numText, 10, 64)
			if err != nil {
				return mdc.Record{}, fmt.Errorf("tick_size_num: %w", err)
			}
			den, err := strconv.ParseUint(denText, 10, 64)
			if err != nil {
				return mdc.Record{}, fmt.Errorf("tick_size_den: %w", err)
			}
			tick.TickSize = mdc.Rational{Num: num, Den: den}
		}
	}
	return tick, nil
}

func parseTimeUnit(value string) (mdc.TimeUnit, error) {
	switch strings.ToLower(value) {
	case "ns":
		return mdc.TimeNanosecond, nil
	case "us", "µs":
		return mdc.TimeMicrosecond, nil
	case "ms":
		return mdc.TimeMillisecond, nil
	case "s":
		return mdc.TimeSecond, nil
	default:
		return 0, fmt.Errorf("invalid time unit %q", value)
	}
}

func parseOrdering(value string) (mdc.Ordering, error) {
	switch strings.ToLower(value) {
	case "source":
		return mdc.SourceOrder, nil
	case "nondecreasing":
		return mdc.NonDecreasing, nil
	case "strict":
		return mdc.StrictlyIncreasing, nil
	default:
		return 0, fmt.Errorf("invalid ordering %q", value)
	}
}

func parseTickSize(value string) (mdc.Rational, error) {
	parts := strings.Split(value, "/")
	if len(parts) != 2 {
		return mdc.Rational{}, fmt.Errorf("invalid tick size %q, want NUM/DEN", value)
	}
	num, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || num <= 0 {
		return mdc.Rational{}, fmt.Errorf("invalid tick-size numerator %q", parts[0])
	}
	den, err := strconv.ParseUint(parts[1], 10, 64)
	if err != nil || den == 0 {
		return mdc.Rational{}, fmt.Errorf("invalid tick-size denominator %q", parts[1])
	}
	return mdc.Rational{Num: num, Den: den}, nil
}

func normalizeHeader(value string) string {
	value = strings.TrimPrefix(strings.TrimSpace(value), "\ufeff")
	return strings.ToLower(value)
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func formatTimeUnit(unit mdc.TimeUnit) string {
	switch unit {
	case mdc.TimeNanosecond:
		return "ns"
	case mdc.TimeMicrosecond:
		return "us"
	case mdc.TimeMillisecond:
		return "ms"
	case mdc.TimeSecond:
		return "s"
	default:
		return strconv.Itoa(int(unit))
	}
}

func formatOrdering(ordering mdc.Ordering) string {
	switch ordering {
	case mdc.SourceOrder:
		return "source"
	case mdc.NonDecreasing:
		return "nondecreasing"
	case mdc.StrictlyIncreasing:
		return "strict"
	default:
		return strconv.Itoa(int(ordering))
	}
}

func runBenchmark(w io.Writer) error {
	const count = 10_000_000
	random := rand.New(rand.NewSource(1337))
	deltaT := make([]uint16, count)
	deltaBid := make([]int8, count)
	spread := make([]uint8, count)
	flags := make([]uint8, count)
	for i := 0; i < count; i++ {
		deltaT[i] = uint16(random.Intn(int(mdc.MaxDeltaT) + 1))
		deltaBid[i] = int8(random.Intn(int(mdc.MaxDeltaBid)-int(mdc.MinDeltaBid)+1) + int(mdc.MinDeltaBid))
		spread[i] = uint8(random.Intn(int(mdc.MaxSpread) + 1))
		flags[i] = uint8(random.Intn(int(mdc.MaxFlag) + 1))
	}
	packed := make([]uint32, count)
	packStart := time.Now()
	for i := range packed {
		packed[i] = mdc.Pack(deltaT[i], deltaBid[i], spread[i], flags[i])
	}
	packDuration := time.Since(packStart)
	unpackStart := time.Now()
	for i, word := range packed {
		dt, db, sp, fl := mdc.Unpack(word)
		if dt != deltaT[i] || db != deltaBid[i] || sp != spread[i] || fl != flags[i] {
			return fmt.Errorf("data mismatch at tick %d", i)
		}
	}
	unpackDuration := time.Since(unpackStart)
	fmt.Fprintln(w, "MDC 16/8/4/4 primitive benchmark (single thread, deterministic input)")
	fmt.Fprintf(w, "Pack:   %v (%.2f million ticks/s)\n", packDuration, float64(count)/packDuration.Seconds()/1e6)
	fmt.Fprintf(w, "Unpack: %v (%.2f million ticks/s)\n", unpackDuration, float64(count)/unpackDuration.Seconds()/1e6)
	fmt.Fprintln(w, "This excludes container framing, CRC, CSV parsing, disk, and network I/O.")
	return nil
}

func formatNumber(value uint64) string {
	in := strconv.FormatUint(value, 10)
	out := make([]byte, 0, len(in)+len(in)/3)
	for i := range in {
		if i > 0 && (len(in)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, in[i])
	}
	return string(out)
}
