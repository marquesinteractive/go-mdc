package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	mdc "github.com/marquesinteractive/go-mdc"
)

func runCLI(args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestCLIEncodeInspectVerifyDecode(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "ticks.csv")
	container := filepath.Join(dir, "ticks.mdc")
	decoded := filepath.Join(dir, "decoded.csv")
	csvData := strings.Join([]string{
		"timestamp,bid_ticks,spread,flags,session,tick_size_num,tick_size_den",
		"1000,100,1,2,1,,",
		"1010,105,31,4660,1,,",
		"70000,300,2,3,2,25,10",
	}, "\n") + "\n"
	if err := os.WriteFile(input, []byte(csvData), 0o600); err != nil {
		t.Fatalf("write input: %v", err)
	}

	code, stdout, stderr := runCLI("encode", "--instrument", "WINFUT:B3", "--price-unit", "index-point", "--time-unit", "ms", "--tick-size", "5/1", input, container)
	if code != 0 {
		t.Fatalf("encode exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "Encoded 3 ticks") {
		t.Fatalf("encode output: %q", stdout)
	}

	code, stdout, stderr = runCLI("verify", container)
	if code != 0 || !strings.Contains(stdout, "valid MDC container") {
		t.Fatalf("verify exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	code, stdout, stderr = runCLI("inspect", container)
	if code != 0 || !strings.Contains(stdout, "Instrument: WINFUT:B3") ||
		!strings.Contains(stdout, "Integrity:  verified") || !strings.Contains(stdout, "Ticks:      3") {
		t.Fatalf("inspect exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	code, stdout, stderr = runCLI("decode", container, decoded)
	if code != 0 {
		t.Fatalf("decode exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	raw, err := os.ReadFile(decoded)
	if err != nil {
		t.Fatalf("read decoded: %v", err)
	}
	text := string(raw)
	if !strings.Contains(text, "1010,105,31,4660,1,5,1") || !strings.Contains(text, "70000,300,2,3,2,5,2") {
		t.Fatalf("decoded CSV: %q", text)
	}

	file, err := os.Open(container)
	if err != nil {
		t.Fatalf("open container: %v", err)
	}
	reader, err := mdc.NewReader(file)
	if err != nil {
		t.Fatalf("new reader: %v", err)
	}
	if reader.Metadata().Instrument != "WINFUT:B3" {
		t.Fatalf("metadata: %+v", reader.Metadata())
	}
	_ = file.Close()
}

func TestCLIRejectsInvalidCSVWithoutOutput(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "bad.csv")
	output := filepath.Join(dir, "bad.mdc")
	if err := os.WriteFile(input, []byte("timestamp,bid_ticks,spread,flags,session\n10,20,nope,0,1\n"), 0o600); err != nil {
		t.Fatalf("write input: %v", err)
	}
	code, _, stderr := runCLI("encode", "--instrument", "TEST", "--price-unit", "quote-unit", input, output)
	if code != 1 || !strings.Contains(stderr, "CSV line 2") {
		t.Fatalf("exit=%d stderr=%q", code, stderr)
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatalf("invalid encode left output: %v", err)
	}
}

func TestCLIRejectsOverwrite(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "ticks.csv")
	output := filepath.Join(dir, "existing.mdc")
	if err := os.WriteFile(input, []byte("timestamp,bid_ticks,spread,flags,session\n1,2,3,4,5\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	want := []byte("preserve")
	if err := os.WriteFile(output, want, 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, _ := runCLI("encode", "--instrument", "TEST", "--price-unit", "quote-unit", input, output)
	if code != 1 {
		t.Fatalf("exit=%d, want 1", code)
	}
	got, err := os.ReadFile(output)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("existing output changed: got=%q err=%v", got, err)
	}
}

func TestCLIRecover(t *testing.T) {
	dir := t.TempDir()
	inputCSV := filepath.Join(dir, "ticks.csv")
	original := filepath.Join(dir, "original.mdc")
	damaged := filepath.Join(dir, "damaged.mdc")
	recovered := filepath.Join(dir, "recovered.mdc")
	data := "timestamp,bid_ticks,spread,flags,session\n10,100,1,1,1\n20,101,1,1,1\n30,102,1,1,1\n40,103,1,1,1\n"
	if err := os.WriteFile(inputCSV, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := runCLI("encode", "--instrument", "TEST", "--price-unit", "quote-unit", "--block-ticks", "2", inputCSV, original)
	if code != 0 {
		t.Fatalf("encode exit=%d stderr=%q", code, stderr)
	}
	raw, err := os.ReadFile(original)
	if err != nil {
		t.Fatal(err)
	}
	first := bytes.Index(raw, []byte("MDBK"))
	secondRelative := bytes.Index(raw[first+4:], []byte("MDBK"))
	if first < 0 || secondRelative < 0 {
		t.Fatal("expected two blocks")
	}
	second := first + 4 + secondRelative
	raw[second+80] ^= 1
	if err := os.WriteFile(damaged, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runCLI("recover", damaged, recovered)
	if code != 0 || !strings.Contains(stdout, "Recovered 2 ticks") {
		t.Fatalf("recover exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	code, _, stderr = runCLI("verify", recovered)
	if code != 0 {
		t.Fatalf("verify recovered exit=%d stderr=%q", code, stderr)
	}
}

func TestCLIUsageAndVersion(t *testing.T) {
	code, stdout, _ := runCLI("version")
	if code != 0 || !strings.Contains(stdout, version) {
		t.Fatalf("version exit=%d stdout=%q", code, stdout)
	}
	code, _, _ = runCLI("encode")
	if code != 1 {
		t.Fatalf("encode usage exit=%d, want 1", code)
	}
	code, _, _ = runCLI("unknown")
	if code != 2 {
		t.Fatalf("unknown exit=%d, want 2", code)
	}
}
