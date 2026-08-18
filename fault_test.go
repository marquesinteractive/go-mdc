package mdc

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"math"
	"os"
	"path/filepath"
	"testing"
)

func goldenContainerBytes(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "golden-container.bin"))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func firstBlockOffset(t *testing.T, data []byte) int {
	t.Helper()
	if len(data) < fileHeaderFixedSize {
		t.Fatal("golden container is shorter than its fixed header")
	}
	offset := int(binary.LittleEndian.Uint32(data[8:12]))
	if offset+blockHeaderSize > len(data) {
		t.Fatal("golden container has no complete first block")
	}
	return offset
}

func refreshBlockChecksums(t *testing.T, data []byte, offset int) {
	t.Helper()
	header := data[offset : offset+blockHeaderSize]
	blockBytes := binary.LittleEndian.Uint64(header[8:16])
	if blockBytes < blockHeaderSize || blockBytes > uint64(len(data)-offset) {
		t.Fatalf("invalid test block length %d", blockBytes)
	}
	payload := data[offset+blockHeaderSize : offset+int(blockBytes)]
	binary.LittleEndian.PutUint32(header[64:68], crc32.Checksum(payload, crcTable))
	binary.LittleEndian.PutUint32(header[68:72], crcWithZeroedField(header, 68))
}

func refreshFileHeaderChecksum(t *testing.T, data []byte) {
	t.Helper()
	headerBytes := int(binary.LittleEndian.Uint32(data[8:12]))
	if headerBytes > len(data) {
		t.Fatal("invalid test header length")
	}
	binary.LittleEndian.PutUint32(data[64:68], crcWithZeroedField(data[:headerBytes], 64))
}

func verifyContainerBytes(data []byte) error {
	reader, err := NewReader(bytes.NewReader(data))
	if err != nil {
		return err
	}
	return reader.Verify()
}

func TestFaultValidCRCDoesNotBypassFirstWordInvariant(t *testing.T) {
	data := goldenContainerBytes(t)
	offset := firstBlockOffset(t, data)
	data[offset+blockHeaderSize] = 1
	refreshBlockChecksums(t, data, offset)

	if err := verifyContainerBytes(data); !errors.Is(err, ErrInvalidFormat) {
		t.Fatalf("got %v, want ErrInvalidFormat", err)
	}
}

func TestFaultValidCRCDoesNotBypassOverrideValidation(t *testing.T) {
	data := goldenContainerBytes(t)
	offset := firstBlockOffset(t, data)
	header := data[offset : offset+blockHeaderSize]
	ticks := binary.LittleEndian.Uint32(header[16:20])
	overrides := binary.LittleEndian.Uint32(header[20:24])
	if overrides == 0 {
		t.Fatal("golden first block must contain an override")
	}
	overrideOffset := offset + blockHeaderSize + int(ticks)*PackedWordSize
	binary.LittleEndian.PutUint32(data[overrideOffset:overrideOffset+4], ticks)
	refreshBlockChecksums(t, data, offset)

	if err := verifyContainerBytes(data); !errors.Is(err, ErrInvalidFormat) {
		t.Fatalf("got %v, want ErrInvalidFormat", err)
	}
}

func TestFaultForgedCountIsRejectedBeforeAllocation(t *testing.T) {
	data := goldenContainerBytes(t)
	offset := firstBlockOffset(t, data)
	header := data[offset : offset+blockHeaderSize]
	binary.LittleEndian.PutUint32(header[16:20], math.MaxUint32)
	binary.LittleEndian.PutUint32(header[68:72], crcWithZeroedField(header, 68))

	if err := verifyContainerBytes(data); !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("got %v, want ErrLimitExceeded", err)
	}
}

func TestFaultNonCanonicalWireRationalIsRejected(t *testing.T) {
	data := goldenContainerBytes(t)
	offset := firstBlockOffset(t, data)
	header := data[offset : offset+blockHeaderSize]
	binary.LittleEndian.PutUint64(header[48:56], 10)
	binary.LittleEndian.PutUint64(header[56:64], 2)
	binary.LittleEndian.PutUint32(header[68:72], crcWithZeroedField(header, 68))

	if err := verifyContainerBytes(data); !errors.Is(err, ErrInvalidFormat) {
		t.Fatalf("got %v, want ErrInvalidFormat", err)
	}
}

func TestFaultDeclaredTickCountMustMatchDecodedContent(t *testing.T) {
	data := goldenContainerBytes(t)
	binary.LittleEndian.PutUint64(data[56:64], 999)
	refreshFileHeaderChecksum(t, data)

	if err := verifyContainerBytes(data); !errors.Is(err, ErrInvalidFormat) {
		t.Fatalf("got %v, want ErrInvalidFormat", err)
	}
}

func TestFaultBlockCannotExceedHeaderMaximum(t *testing.T) {
	data := goldenContainerBytes(t)
	binary.LittleEndian.PutUint32(data[44:48], 1)
	refreshFileHeaderChecksum(t, data)

	if err := verifyContainerBytes(data); !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("sequential: got %v, want ErrLimitExceeded", err)
	}
	if _, err := Open(bytes.NewReader(data)); !errors.Is(err, ErrIndexMismatch) {
		t.Fatalf("indexed: got %v, want ErrIndexMismatch", err)
	}
}

func TestFaultSequentialReaderBoundsRememberedBlocks(t *testing.T) {
	data := goldenContainerBytes(t)
	limits := DefaultReaderLimits()
	limits.MaxIndexEntries = 1
	reader, err := NewReaderLimits(bytes.NewReader(data), limits)
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.Verify(); !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("got %v, want ErrLimitExceeded", err)
	}
}

func TestRecoveryFindsBlocksAfterByteInsertion(t *testing.T) {
	original := goldenContainerBytes(t)
	offset := firstBlockOffset(t, original)
	insertAt := offset + blockHeaderSize + 1
	damaged := make([]byte, 0, len(original)+1)
	damaged = append(damaged, original[:insertAt]...)
	damaged = append(damaged, 0xA5)
	damaged = append(damaged, original[insertAt:]...)

	var recovered bytes.Buffer
	report, err := Recover(bytes.NewReader(damaged), &recovered, DefaultReaderLimits())
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if report.RecoveredBlocks == 0 || report.RecoveredTicks == 0 || len(report.Damage) == 0 {
		t.Fatalf("incomplete report: %+v", report)
	}
	if err := verifyContainerBytes(recovered.Bytes()); err != nil {
		t.Fatalf("verify recovered container: %v", err)
	}
}
