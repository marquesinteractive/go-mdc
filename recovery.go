package mdc

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
)

// DamageRange identifies a byte interval that did not belong to a validated
// block during recovery. End is exclusive.
type DamageRange struct {
	Start uint64
	End   uint64
}

// RecoveryReport describes the evidence preserved while rebuilding a damaged
// finite container.
type RecoveryReport struct {
	ScannedBytes    uint64
	RecoveredBlocks uint64
	RecoveredTicks  uint64
	Damage          []DamageRange
}

// Recover scans a container with a valid file header and writes a new finite
// MDC container containing every independently checksum-valid block it can
// recover. It never copies an unverified block. Input and output must be
// different storage objects.
func Recover(input io.ReadSeeker, output io.Writer, limits ReaderLimits) (RecoveryReport, error) {
	var report RecoveryReport
	if input == nil || output == nil {
		return report, fmt.Errorf("%w: nil recovery endpoint", ErrInvalidRecord)
	}
	limits = limits.normalized()
	if _, err := input.Seek(0, io.SeekStart); err != nil {
		return report, err
	}
	header, headerBytes, err := readFileHeader(input, limits)
	if err != nil {
		return report, err
	}
	end, err := input.Seek(0, io.SeekEnd)
	if err != nil {
		return report, err
	}
	if end < int64(headerBytes) {
		return report, fmt.Errorf("%w: file shorter than header", ErrInvalidFormat)
	}
	scanEnd := uint64(end)
	if indexOffset, ok := validIndexOffset(input, header, limits); ok {
		scanEnd = indexOffset
	}

	config := DefaultWriterConfig()
	config.MaxBlockTicks = header.maxBlockTicks
	writer, err := NewWriterConfig(output, header.metadata, config)
	if err != nil {
		return report, err
	}

	position := headerBytes
	var damageStart uint64
	inDamage := false
	for position+4 <= scanEnd {
		if _, err := input.Seek(int64(position), io.SeekStart); err != nil {
			return report, err
		}
		var magic [4]byte
		if _, err := io.ReadFull(input, magic[:]); err != nil {
			break
		}
		if !hasMagic(magic[:], blockMagic) {
			if !inDamage {
				damageStart = position
				inDamage = true
			}
			position++
			continue
		}

		if _, err := input.Seek(int64(position), io.SeekStart); err != nil {
			return report, err
		}
		var headerPreview [blockHeaderSize]byte
		if _, err := io.ReadFull(input, headerPreview[:]); err != nil {
			if !inDamage {
				damageStart = position
				inDamage = true
			}
			position++
			continue
		}
		sequence := binary.LittleEndian.Uint32(headerPreview[24:28])
		if _, err := input.Seek(int64(position), io.SeekStart); err != nil {
			return report, err
		}
		candidate := &Reader{
			reader:           input,
			metadata:         header.metadata,
			limits:           limits,
			header:           fileHeader{metadata: header.metadata, flags: fileFlagStreaming, maxBlockTicks: header.maxBlockTicks},
			offset:           position,
			expectedSequence: sequence,
		}
		if err := candidate.loadNext(); err != nil {
			if !inDamage {
				damageStart = position
				inDamage = true
			}
			position++
			continue
		}
		if candidate.offset > scanEnd || candidate.offset <= position {
			if !inDamage {
				damageStart = position
				inDamage = true
			}
			position += 4
			continue
		}
		if inDamage {
			report.Damage = append(report.Damage, DamageRange{Start: damageStart, End: position})
			inDamage = false
		}
		if _, err := writer.WriteBatch(candidate.records); err != nil {
			return report, fmt.Errorf("re-encode recovered block at offset %d: %w", position, err)
		}
		if err := writer.Flush(); err != nil {
			return report, fmt.Errorf("flush recovered block at offset %d: %w", position, err)
		}
		report.RecoveredBlocks++
		report.RecoveredTicks += uint64(len(candidate.records))
		position = candidate.offset
	}
	if position < scanEnd {
		if !inDamage {
			damageStart = position
			inDamage = true
		}
	}
	if inDamage {
		report.Damage = append(report.Damage, DamageRange{Start: damageStart, End: scanEnd})
	}
	report.ScannedBytes = scanEnd - headerBytes
	if err := writer.Close(); err != nil {
		return report, err
	}
	return report, nil
}

func validIndexOffset(reader io.ReadSeeker, header fileHeader, limits ReaderLimits) (uint64, bool) {
	if header.flags&fileFlagIndex == 0 {
		return 0, false
	}
	if _, err := readIndexFromEnd(reader, header, limits); err != nil {
		return 0, false
	}
	end, err := reader.Seek(0, io.SeekEnd)
	if err != nil || end < indexTrailerSize {
		return 0, false
	}
	if _, err := reader.Seek(-indexTrailerSize, io.SeekEnd); err != nil {
		return 0, false
	}
	var trailer [indexTrailerSize]byte
	if _, err := io.ReadFull(reader, trailer[:]); err != nil ||
		!hasMagic(trailer[:], trailerMagic) || trailer[4] != 1 || !allZero(trailer[5:8]) ||
		crc32.Checksum(trailer[:20], crcTable) != readUint32(trailer[:], 20) {
		return 0, false
	}
	sectionBytes := readUint64(trailer[:], 8)
	if sectionBytes < indexHeaderSize || sectionBytes > uint64(end-indexTrailerSize) {
		return 0, false
	}
	return uint64(end-indexTrailerSize) - sectionBytes, true
}
