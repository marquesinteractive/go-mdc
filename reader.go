package mdc

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
)

// Reader verifies and decodes a canonical MDC container sequentially. It is
// not safe for concurrent use.
type Reader struct {
	reader   io.Reader
	metadata Metadata
	limits   ReaderLimits
	header   fileHeader

	offset           uint64
	expectedSequence uint32
	seen             []blockIndexEntry
	records          []Record
	recordIndex      int
	totalTicks       uint64
	lastTimestamp    int64
	haveTimestamp    bool
	finished         bool
	err              error
}

// NewReader creates a sequential reader with conservative allocation limits.
func NewReader(r io.Reader) (*Reader, error) {
	return NewReaderLimits(r, DefaultReaderLimits())
}

// NewReaderLimits creates a sequential reader with explicit untrusted-input limits.
func NewReaderLimits(r io.Reader, limits ReaderLimits) (*Reader, error) {
	if r == nil {
		return nil, fmt.Errorf("%w: nil reader", ErrInvalidFormat)
	}
	limits = limits.normalized()
	header, consumed, err := readFileHeader(r, limits)
	if err != nil {
		return nil, err
	}
	reader := &Reader{
		reader:   r,
		metadata: header.metadata,
		limits:   limits,
		header:   header,
		offset:   consumed,
	}
	if header.flags&fileFlagIndex != 0 {
		reader.seen = make([]blockIndexEntry, 0, 64)
	}
	return reader, nil
}

// Metadata returns the file-level interpretation contract.
func (r *Reader) Metadata() Metadata {
	return r.metadata
}

// ReadTick returns the next fully reconstructed record.
func (r *Reader) ReadTick() (Record, error) {
	if r.err != nil {
		return Record{}, r.err
	}
	for r.recordIndex >= len(r.records) {
		if r.finished {
			return Record{}, io.EOF
		}
		if err := r.loadNext(); err != nil {
			if errors.Is(err, io.EOF) {
				return Record{}, io.EOF
			}
			r.err = err
			return Record{}, err
		}
	}
	record := r.records[r.recordIndex]
	r.recordIndex++
	return record, nil
}

// ReadBatch fills dst with reconstructed records. It returns io.EOF only when
// no records were produced; a final partial batch returns its count and nil.
func (r *Reader) ReadBatch(dst []Record) (int, error) {
	for i := range dst {
		record, err := r.ReadTick()
		if err != nil {
			if errors.Is(err, io.EOF) && i != 0 {
				return i, nil
			}
			return i, err
		}
		dst[i] = record
	}
	return len(dst), nil
}

// Verify consumes the remainder of the container and validates every block,
// checksum, sequence, index entry, and trailer.
func (r *Reader) Verify() error {
	for {
		_, err := r.ReadTick()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func (r *Reader) readFull(dst []byte) error {
	n, err := io.ReadFull(r.reader, dst)
	r.offset += uint64(n)
	return err
}

func (r *Reader) loadNext() error {
	startOffset := r.offset
	var prefix [4]byte
	err := r.readFull(prefix[:])
	if err != nil {
		if errors.Is(err, io.EOF) {
			if r.header.flags&fileFlagStreaming != 0 {
				if r.header.declaredTicks != unknownTickCount && r.totalTicks != r.header.declaredTicks {
					return fmt.Errorf("%w: decoded %d ticks, header declares %d", ErrInvalidFormat, r.totalTicks, r.header.declaredTicks)
				}
				r.finished = true
				return io.EOF
			}
			return ErrMissingIndex
		}
		return fmt.Errorf("%w: record prefix at offset %d: %v", ErrInvalidFormat, startOffset, err)
	}

	switch {
	case hasMagic(prefix[:], blockMagic):
		header := make([]byte, blockHeaderSize)
		copy(header, prefix[:])
		if err := r.readFull(header[4:]); err != nil {
			return fmt.Errorf("%w: block header at offset %d: %v", ErrInvalidFormat, startOffset, err)
		}
		return r.loadBlock(startOffset, header)
	case hasMagic(prefix[:], indexMagic):
		if r.header.flags&fileFlagIndex == 0 {
			return fmt.Errorf("%w: index found in streaming container", ErrInvalidFormat)
		}
		header := make([]byte, indexHeaderSize)
		copy(header, prefix[:])
		if err := r.readFull(header[4:]); err != nil {
			return fmt.Errorf("%w: index header: %v", ErrInvalidFormat, err)
		}
		if err := r.loadIndex(header); err != nil {
			return err
		}
		r.finished = true
		return io.EOF
	default:
		return fmt.Errorf("%w: unknown section magic %q at offset %d", ErrInvalidFormat, prefix, startOffset)
	}
}

func (r *Reader) loadBlock(offset uint64, header []byte) error {
	if header[4] != 1 || header[5]&^byte(blockFlagOverride) != 0 ||
		binary.LittleEndian.Uint16(header[6:8]) != blockHeaderSize {
		return fmt.Errorf("%w: unsupported block header at offset %d", ErrUnsupportedFormat, offset)
	}
	if !allZero(header[72:80]) {
		return fmt.Errorf("%w: non-zero block reserved bytes at offset %d", ErrInvalidFormat, offset)
	}
	wantHeaderCRC := binary.LittleEndian.Uint32(header[68:72])
	if got := crcWithZeroedField(header, 68); got != wantHeaderCRC {
		return fmt.Errorf("%w: block header at offset %d", ErrChecksumMismatch, offset)
	}

	blockBytes := binary.LittleEndian.Uint64(header[8:16])
	tickCount := binary.LittleEndian.Uint32(header[16:20])
	overrideCount := binary.LittleEndian.Uint32(header[20:24])
	sequence := binary.LittleEndian.Uint32(header[24:28])
	session := binary.LittleEndian.Uint32(header[28:32])
	if tickCount == 0 || tickCount > r.header.maxBlockTicks || uint64(tickCount) > r.limits.MaxBlockTicks {
		return fmt.Errorf("%w: block tick count %d", ErrLimitExceeded, tickCount)
	}
	if uint64(overrideCount) > r.limits.MaxOverrides || overrideCount > tickCount {
		return fmt.Errorf("%w: block override count %d", ErrLimitExceeded, overrideCount)
	}
	wantBlockBytes, ok := encodedBlockBytes(tickCount, overrideCount)
	if !ok || blockBytes != wantBlockBytes || blockBytes > r.limits.MaxBlockBytes {
		return fmt.Errorf("%w: invalid block length %d", ErrLimitExceeded, blockBytes)
	}
	if sequence != r.expectedSequence {
		return fmt.Errorf("%w: got %d, want %d", ErrSequenceMismatch, sequence, r.expectedSequence)
	}
	if (overrideCount != 0) != (header[5]&blockFlagOverride != 0) {
		return fmt.Errorf("%w: override flag/count disagreement", ErrInvalidFormat)
	}
	tickSize, err := parseRational(readInt64(header, 48), readUint64(header, 56))
	if err != nil {
		return fmt.Errorf("%w: block tick size", ErrInvalidFormat)
	}

	payloadBytes := blockBytes - blockHeaderSize
	if payloadBytes > uint64(maxInt()) {
		return fmt.Errorf("%w: block payload does not fit this platform", ErrLimitExceeded)
	}
	payload := make([]byte, int(payloadBytes))
	if err := r.readFull(payload); err != nil {
		return fmt.Errorf("%w: block payload at offset %d: %v", ErrInvalidFormat, offset, err)
	}
	if got, want := crc32.Checksum(payload, crcTable), binary.LittleEndian.Uint32(header[64:68]); got != want {
		return fmt.Errorf("%w: block payload at offset %d", ErrChecksumMismatch, offset)
	}

	baseTimestamp := readInt64(header, 32)
	baseBid := readInt64(header, 40)
	records, err := decodeBlock(payload, tickCount, overrideCount, baseTimestamp, baseBid, session, tickSize)
	if err != nil {
		return fmt.Errorf("block %d: %w", sequence, err)
	}
	for _, record := range records {
		if r.haveTimestamp {
			if record.Timestamp < r.lastTimestamp && r.metadata.Ordering != SourceOrder {
				return fmt.Errorf("%w: timestamp %d follows %d", ErrOrderingViolation, record.Timestamp, r.lastTimestamp)
			}
			if record.Timestamp == r.lastTimestamp && r.metadata.Ordering == StrictlyIncreasing {
				return fmt.Errorf("%w: repeated timestamp %d", ErrOrderingViolation, record.Timestamp)
			}
		}
		r.lastTimestamp = record.Timestamp
		r.haveTimestamp = true
	}

	r.records = records
	r.recordIndex = 0
	if r.totalTicks > ^uint64(0)-uint64(tickCount) {
		return fmt.Errorf("%w: total tick count overflow", ErrLimitExceeded)
	}
	r.totalTicks += uint64(tickCount)
	if r.header.flags&fileFlagIndex != 0 {
		if uint64(len(r.seen)) >= r.limits.MaxIndexEntries {
			return fmt.Errorf("%w: decoded block count exceeds index limit", ErrLimitExceeded)
		}
		r.seen = append(r.seen, blockIndexEntry{
			offset:        offset,
			baseTimestamp: baseTimestamp,
			tickCount:     tickCount,
			sequence:      sequence,
			session:       session,
		})
	}
	r.expectedSequence++
	return nil
}

func (r *Reader) loadIndex(header []byte) error {
	if header[4] != 1 || !allZero(header[5:8]) || binary.LittleEndian.Uint32(header[16:20]) != indexEntrySize {
		return fmt.Errorf("%w: unsupported index header", ErrUnsupportedFormat)
	}
	if got, want := crc32.Checksum(header[:20], crcTable), binary.LittleEndian.Uint32(header[20:24]); got != want {
		return fmt.Errorf("%w: index header", ErrChecksumMismatch)
	}
	count := binary.LittleEndian.Uint64(header[8:16])
	if count > r.limits.MaxIndexEntries || count > uint64(maxInt()/indexEntrySize) {
		return fmt.Errorf("%w: index contains %d entries", ErrLimitExceeded, count)
	}
	entries := make([]byte, int(count)*indexEntrySize)
	if err := r.readFull(entries); err != nil {
		return fmt.Errorf("%w: index entries: %v", ErrInvalidFormat, err)
	}
	trailer := make([]byte, indexTrailerSize)
	if err := r.readFull(trailer); err != nil {
		return fmt.Errorf("%w: index trailer: %v", ErrInvalidFormat, err)
	}
	if !hasMagic(trailer, trailerMagic) || trailer[4] != 1 || !allZero(trailer[5:8]) {
		return fmt.Errorf("%w: invalid index trailer", ErrInvalidFormat)
	}
	if got, want := crc32.Checksum(trailer[:20], crcTable), binary.LittleEndian.Uint32(trailer[20:24]); got != want {
		return fmt.Errorf("%w: index trailer", ErrChecksumMismatch)
	}
	if want := uint64(indexHeaderSize) + uint64(len(entries)); binary.LittleEndian.Uint64(trailer[8:16]) != want {
		return fmt.Errorf("%w: index section length", ErrInvalidFormat)
	}
	if got, want := crc32.Checksum(entries, crcTable), binary.LittleEndian.Uint32(trailer[16:20]); got != want {
		return fmt.Errorf("%w: index entries", ErrChecksumMismatch)
	}
	if uint64(len(r.seen)) != count {
		return fmt.Errorf("%w: decoded %d blocks, index declares %d", ErrIndexMismatch, len(r.seen), count)
	}
	if r.header.declaredTicks != unknownTickCount && r.totalTicks != r.header.declaredTicks {
		return fmt.Errorf("%w: decoded %d ticks, header declares %d", ErrInvalidFormat, r.totalTicks, r.header.declaredTicks)
	}
	for i, seen := range r.seen {
		entry := entries[i*indexEntrySize:]
		indexed := blockIndexEntry{
			offset:        readUint64(entry, 0),
			baseTimestamp: readInt64(entry, 8),
			tickCount:     readUint32(entry, 16),
			sequence:      readUint32(entry, 20),
			session:       readUint32(entry, 24),
		}
		if !allZero(entry[28:32]) || indexed != seen {
			return fmt.Errorf("%w: entry %d", ErrIndexMismatch, i)
		}
	}
	var trailing [1]byte
	n, err := r.reader.Read(trailing[:])
	r.offset += uint64(n)
	if n != 0 || (err != nil && !errors.Is(err, io.EOF)) {
		return fmt.Errorf("%w: trailing bytes after index", ErrInvalidFormat)
	}
	return nil
}

func decodeBlock(payload []byte, tickCount, overrideCount uint32, baseTimestamp, baseBid int64, session uint32, tickSize Rational) ([]Record, error) {
	records := make([]Record, int(tickCount))
	wordsBytes := int(tickCount) * PackedWordSize
	if wordsBytes > len(payload) {
		return nil, fmt.Errorf("%w: short word payload", ErrInvalidFormat)
	}
	timestamp := baseTimestamp
	bid := baseBid
	for i := 0; i < int(tickCount); i++ {
		word := binary.LittleEndian.Uint32(payload[i*PackedWordSize:])
		deltaT, deltaBid, spread, flags := Unpack(word)
		if i == 0 {
			if deltaT != 0 || deltaBid != 0 {
				return nil, fmt.Errorf("%w: first block word must have zero deltas", ErrInvalidFormat)
			}
		} else {
			if deltaT == EscapeTime || deltaBid == EscapePrice {
				return nil, fmt.Errorf("%w: reserved marker in normal block word %d", ErrInvalidFormat, i)
			}
			var ok bool
			timestamp, ok = addInt64(timestamp, int64(deltaT))
			if !ok {
				return nil, fmt.Errorf("%w: timestamp overflow at word %d", ErrInvalidFormat, i)
			}
			bid, ok = addInt64(bid, int64(deltaBid))
			if !ok {
				return nil, fmt.Errorf("%w: bid overflow at word %d", ErrInvalidFormat, i)
			}
		}
		records[i] = Record{
			Timestamp: timestamp,
			BidTicks:  bid,
			Spread:    uint32(spread),
			Flags:     uint32(flags),
			Session:   session,
			TickSize:  tickSize,
		}
	}

	var previousIndex uint32
	for i := uint32(0); i < overrideCount; i++ {
		entry := payload[wordsBytes+int(i)*overrideEntrySize:]
		index := readUint32(entry, 0)
		mask := entry[4]
		spread := readUint32(entry, 8)
		flags := readUint32(entry, 12)
		if index >= tickCount || (i != 0 && index <= previousIndex) || mask == 0 || mask&^byte(3) != 0 || !allZero(entry[5:8]) {
			return nil, fmt.Errorf("%w: invalid override %d", ErrInvalidFormat, i)
		}
		if mask&1 != 0 {
			if spread <= uint32(MaxSpread) || uint8(spread&uint32(MaxSpread)) != uint8(records[index].Spread) {
				return nil, fmt.Errorf("%w: non-canonical spread override %d", ErrInvalidFormat, i)
			}
			records[index].Spread = spread
		} else if spread != 0 {
			return nil, fmt.Errorf("%w: unused spread override data %d", ErrInvalidFormat, i)
		}
		if mask&2 != 0 {
			if flags <= uint32(MaxFlag) || uint8(flags&uint32(MaxFlag)) != uint8(records[index].Flags) {
				return nil, fmt.Errorf("%w: non-canonical flag override %d", ErrInvalidFormat, i)
			}
			records[index].Flags = flags
		} else if flags != 0 {
			return nil, fmt.Errorf("%w: unused flag override data %d", ErrInvalidFormat, i)
		}
		previousIndex = index
	}
	return records, nil
}

func readFileHeader(r io.Reader, limits ReaderLimits) (fileHeader, uint64, error) {
	fixed := make([]byte, fileHeaderFixedSize)
	if _, err := io.ReadFull(r, fixed); err != nil {
		return fileHeader{}, 0, fmt.Errorf("%w: file header: %v", ErrInvalidFormat, err)
	}
	if !hasMagic(fixed, fileMagic) {
		return fileHeader{}, 0, fmt.Errorf("%w: magic", ErrInvalidFormat)
	}
	if fixed[4] != formatMajor || fixed[5] > formatMinor || fixed[6] != endiannessLittle {
		return fileHeader{}, 0, fmt.Errorf("%w: version %d.%d or endianness %d", ErrUnsupportedFormat, fixed[4], fixed[5], fixed[6])
	}
	flags := fixed[7]
	if flags != fileFlagIndex && flags != fileFlagStreaming {
		return fileHeader{}, 0, fmt.Errorf("%w: file flags 0x%02x", ErrUnsupportedFormat, flags)
	}
	headerBytes := readUint32(fixed, 8)
	if headerBytes < fileHeaderFixedSize || headerBytes%8 != 0 || uint64(headerBytes) > limits.MaxHeaderBytes || uint64(headerBytes) > uint64(maxInt()) {
		return fileHeader{}, 0, fmt.Errorf("%w: header length %d", ErrLimitExceeded, headerBytes)
	}
	if readUint32(fixed, 12) != blockHeaderSize || fixed[18] != checksumCRC32C || fixed[19] != priceModeTicks || fixed[21] != textUTF8 {
		return fileHeader{}, 0, fmt.Errorf("%w: unsupported file feature", ErrUnsupportedFormat)
	}
	if !allZero(fixed[22:24]) || !allZero(fixed[68:72]) || readUint32(fixed, 48) != featureOverrides {
		return fileHeader{}, 0, fmt.Errorf("%w: reserved fields or feature bits", ErrUnsupportedFormat)
	}
	full := make([]byte, int(headerBytes))
	copy(full, fixed)
	if _, err := io.ReadFull(r, full[fileHeaderFixedSize:]); err != nil {
		return fileHeader{}, 0, fmt.Errorf("%w: extended header: %v", ErrInvalidFormat, err)
	}
	if got, want := crcWithZeroedField(full, 64), readUint32(full, 64); got != want {
		return fileHeader{}, 0, fmt.Errorf("%w: file header", ErrChecksumMismatch)
	}
	instrumentBytes := readUint32(full, 40)
	priceUnitBytes := readUint32(full, 52)
	variableBytes := uint64(instrumentBytes) + uint64(priceUnitBytes)
	if variableBytes > uint64(headerBytes-fileHeaderFixedSize) {
		return fileHeader{}, 0, fmt.Errorf("%w: metadata string lengths", ErrInvalidFormat)
	}
	instrumentEnd := fileHeaderFixedSize + int(instrumentBytes)
	priceUnitEnd := instrumentEnd + int(priceUnitBytes)
	if !allZero(full[priceUnitEnd:]) {
		return fileHeader{}, 0, fmt.Errorf("%w: non-zero header padding", ErrInvalidFormat)
	}
	tickSize, err := parseRational(readInt64(full, 24), readUint64(full, 32))
	if err != nil {
		return fileHeader{}, 0, err
	}
	metadata := Metadata{
		Instrument: string(full[fileHeaderFixedSize:instrumentEnd]),
		PriceUnit:  string(full[instrumentEnd:priceUnitEnd]),
		TimeUnit:   TimeUnit(full[16]),
		Ordering:   Ordering(full[17]),
		TickSize:   tickSize,
		SpreadUnit: SpreadUnit(full[20]),
	}
	if err := metadata.validate(); err != nil {
		return fileHeader{}, 0, err
	}
	maxBlockTicks := readUint32(full, 44)
	if maxBlockTicks == 0 || uint64(maxBlockTicks) > limits.MaxBlockTicks {
		return fileHeader{}, 0, fmt.Errorf("%w: declared max block ticks %d", ErrLimitExceeded, maxBlockTicks)
	}
	return fileHeader{
		metadata: metadata, flags: flags, headerBytes: headerBytes,
		maxBlockTicks: maxBlockTicks, declaredTicks: readUint64(full, 56),
	}, uint64(headerBytes), nil
}

func allZero(data []byte) bool {
	for _, value := range data {
		if value != 0 {
			return false
		}
	}
	return true
}

func maxInt() int {
	return int(^uint(0) >> 1)
}
