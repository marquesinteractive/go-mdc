package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateProjectionAndRejectCrossedQuote(t *testing.T) {
	directory := t.TempDir()
	input := filepath.Join(directory, "ticks.jsonl")
	output := filepath.Join(directory, "ticks.mdc")
	report := filepath.Join(directory, "validation.md")
	data := strings.Join([]string{
		`{"symbol":"WINFUT","bid":100,"ask":105,"volume":7,"timestamp":1782226175}`,
		`{"symbol":"WINFUT","bid":110,"ask":105,"volume":8,"timestamp":1782226176}`,
		`{"symbol":"WINFUT","bid":95,"ask":105,"volume":9,"timestamp":1782226100}`,
	}, "\n") + "\n"
	if err := os.WriteFile(input, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := validate(input, output, "WINFUT", "WINFUT:B3", "index-point", 5)
	if err != nil {
		t.Fatal(err)
	}
	if result.TotalLines != 3 || result.Accepted != 2 || len(result.Rejected) != 1 ||
		result.Rejected[0].Line != 2 || result.TimestampRegressions != 1 || result.Blocks != 2 {
		t.Fatalf("unexpected result: %+v", result)
	}
	if err := writeReport(report, result); err != nil {
		t.Fatal(err)
	}
	text, err := os.ReadFile(report)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(text), "not a claim of full-feed losslessness") {
		t.Fatalf("projection boundary missing from report: %s", text)
	}
}
