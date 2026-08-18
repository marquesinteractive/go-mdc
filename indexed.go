package mdc

import (
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"sort"
)

// FileReader provides validated random access to a finite indexed MDC file.
// The underlying ReadSeeker remains owned by the caller.
type FileReader struct {
	reader   io.ReadSeeker
	metadata Metadata
	limits   ReaderLimits
	header   fileHeader
	entries  []blockIndexEntry

	nextEntry int
	records   []Record
	recordPos int
	err       error
}

// Open validates the file header, trailer, and complete index without scanning
// every block payload. Each block is checksum-validated when read.
func Open(reader io.ReadSeeker) (*FileReader, error) {
	return OpenLimits(reader, DefaultReaderLimits())
}

// OpenLimits is Open with explicit untrusted-input allocation limits.
func OpenLimits(reader io.ReadSeeker, limits ReaderLimits) (*FileReader, error) {
	if reader == nil {
		return nil, fmt.Errorf("%w: nil seekable reader", ErrInvalidFormat)
	}
	limits = limits.normalized()
	if _, err := reader.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	header, _, err := readFileHeader(reader, limits)
	if err != nil {
		return nil, err
	}
	if header.flags&fileFlagIndex == 0 {
		return nil, ErrMissingIndex
	}
	entries, err := readIndexFromEnd(reader, header, limits)
	if err != nil {
		return nil, err
	}
	if header.declaredTicks != unknownTickCount {
		var indexedTicks uint64
		for _, entry := range entries {
			indexedTicks += uint64(entry.tickCount)
		}
		if indexedTicks != header.declaredTicks {
			return nil, fmt.Errorf("%w: index contains %d ticks, header declares %d", ErrIndexMismatch, indexedTicks, header.declaredTicks)
		}
	}
	return &FileReader{
		reader:   reader,
		metadata: header.metadata,
		limits:   limits,
		header:   header,
		entries:  entries,
	}, nil
}

// Metadata returns the file-level interpretation contract.
func (r *FileReader) Metadata() Metadata {
	return r.metadata
}

// BlockCount returns the number of independently checksummed blocks.
func (r *FileReader) BlockCount() int {
	return len(r.entries)
}

// SeekBlock positions the next read at the first tick of sequence.
func (r *FileReader) SeekBlock(sequence uint32) error {
	index := sort.Search(len(r.entries), func(i int) bool {
		return r.entries[i].sequence >= sequence
	})
	if index == len(r.entries) || r.entries[index].sequence != sequence {
		return fmt.Errorf("%w: block sequence %d", ErrInvalidRecord, sequence)
	}
	r.nextEntry = index
	r.records = nil
	r.recordPos = 0
	r.err = nil
	return nil
}

// SeekTimestamp positions the next read at the first tick whose timestamp is
// greater than or equal to target. SourceOrder files do not support temporal
// binary search because block bases may regress.
func (r *FileReader) SeekTimestamp(target int64) error {
	if r.metadata.Ordering == SourceOrder {
		return fmt.Errorf("%w: temporal seek requires monotonic ordering", ErrOrderingViolation)
	}
	if len(r.entries) == 0 {
		r.nextEntry = 0
		r.records = nil
		r.recordPos = 0
		return nil
	}
	index := sort.Search(len(r.entries), func(i int) bool {
		return r.entries[i].baseTimestamp >= target
	})
	if index == len(r.entries) || r.entries[index].baseTimestamp > target {
		index--
	}
	if index < 0 {
		index = 0
	}
	if err := r.loadEntry(index); err != nil {
		return err
	}
	for {
		r.recordPos = sort.Search(len(r.records), func(i int) bool {
			return r.records[i].Timestamp >= target
		})
		if r.recordPos < len(r.records) || r.nextEntry >= len(r.entries) {
			return nil
		}
		if err := r.loadEntry(r.nextEntry); err != nil {
			return err
		}
	}
}

// ReadTick returns the next reconstructed record from the current position.
func (r *FileReader) ReadTick() (Record, error) {
	if r.err != nil {
		return Record{}, r.err
	}
	for r.recordPos >= len(r.records) {
		if r.nextEntry >= len(r.entries) {
			return Record{}, io.EOF
		}
		if err := r.loadEntry(r.nextEntry); err != nil {
			r.err = err
			return Record{}, err
		}
	}
	record := r.records[r.recordPos]
	r.recordPos++
	return record, nil
}

// ReadBatch fills dst from the current random-access position.
func (r *FileReader) ReadBatch(dst []Record) (int, error) {
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

func (r *FileReader) loadEntry(index int) error {
	entry := r.entries[index]
	if _, err := r.reader.Seek(int64(entry.offset), io.SeekStart); err != nil {
		return err
	}
	decoder := &Reader{
		reader:           r.reader,
		metadata:         r.metadata,
		limits:           r.limits,
		header:           fileHeader{metadata: r.metadata, flags: fileFlagStreaming, maxBlockTicks: r.header.maxBlockTicks},
		offset:           entry.offset,
		expectedSequence: entry.sequence,
	}
	if err := decoder.loadNext(); err != nil {
		return err
	}
	if len(decoder.records) != int(entry.tickCount) || len(decoder.records) == 0 ||
		decoder.records[0].Timestamp != entry.baseTimestamp ||
		decoder.records[0].Session != entry.session {
		return fmt.Errorf("%w: block %d", ErrIndexMismatch, entry.sequence)
	}
	r.records = decoder.records
	r.recordPos = 0
	r.nextEntry = index + 1
	return nil
}

func readIndexFromEnd(reader io.ReadSeeker, header fileHeader, limits ReaderLimits) ([]blockIndexEntry, error) {
	end, err := reader.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, err
	}
	if end < int64(header.headerBytes)+indexHeaderSize+indexTrailerSize {
		return nil, ErrMissingIndex
	}
	if _, err := reader.Seek(-indexTrailerSize, io.SeekEnd); err != nil {
		return nil, err
	}
	trailer := make([]byte, indexTrailerSize)
	if _, err := io.ReadFull(reader, trailer); err != nil {
		return nil, fmt.Errorf("%w: index trailer: %v", ErrInvalidFormat, err)
	}
	if !hasMagic(trailer, trailerMagic) || trailer[4] != 1 || !allZero(trailer[5:8]) {
		return nil, ErrMissingIndex
	}
	if got, want := crc32.Checksum(trailer[:20], crcTable), readUint32(trailer, 20); got != want {
		return nil, fmt.Errorf("%w: index trailer", ErrChecksumMismatch)
	}
	sectionBytes := readUint64(trailer, 8)
	if sectionBytes < indexHeaderSize || sectionBytes > uint64(end-indexTrailerSize) || sectionBytes > limits.MaxBlockBytes {
		return nil, fmt.Errorf("%w: index section length %d", ErrLimitExceeded, sectionBytes)
	}
	sectionOffset := end - indexTrailerSize - int64(sectionBytes)
	if sectionOffset < int64(header.headerBytes) {
		return nil, fmt.Errorf("%w: index overlaps file header", ErrInvalidFormat)
	}
	if _, err := reader.Seek(sectionOffset, io.SeekStart); err != nil {
		return nil, err
	}
	section := make([]byte, int(sectionBytes))
	if _, err := io.ReadFull(reader, section); err != nil {
		return nil, fmt.Errorf("%w: index section: %v", ErrInvalidFormat, err)
	}
	indexHeader := section[:indexHeaderSize]
	if !hasMagic(indexHeader, indexMagic) || indexHeader[4] != 1 || !allZero(indexHeader[5:8]) || readUint32(indexHeader, 16) != indexEntrySize {
		return nil, fmt.Errorf("%w: index header", ErrUnsupportedFormat)
	}
	if got, want := crc32.Checksum(indexHeader[:20], crcTable), readUint32(indexHeader, 20); got != want {
		return nil, fmt.Errorf("%w: index header", ErrChecksumMismatch)
	}
	count := readUint64(indexHeader, 8)
	if count > limits.MaxIndexEntries || count > uint64(maxInt()/indexEntrySize) ||
		uint64(indexHeaderSize)+count*indexEntrySize != sectionBytes {
		return nil, fmt.Errorf("%w: index entry count %d", ErrLimitExceeded, count)
	}
	entriesBytes := section[indexHeaderSize:]
	if got, want := crc32.Checksum(entriesBytes, crcTable), readUint32(trailer, 16); got != want {
		return nil, fmt.Errorf("%w: index entries", ErrChecksumMismatch)
	}
	entries := make([]blockIndexEntry, int(count))
	var previousOffset uint64
	for i := range entries {
		encoded := entriesBytes[i*indexEntrySize:]
		entry := blockIndexEntry{
			offset:        readUint64(encoded, 0),
			baseTimestamp: readInt64(encoded, 8),
			tickCount:     readUint32(encoded, 16),
			sequence:      readUint32(encoded, 20),
			session:       readUint32(encoded, 24),
		}
		if !allZero(encoded[28:32]) || entry.sequence != uint32(i) || entry.tickCount == 0 ||
			entry.tickCount > header.maxBlockTicks ||
			uint64(entry.tickCount) > limits.MaxBlockTicks || entry.offset < uint64(header.headerBytes) ||
			entry.offset >= uint64(sectionOffset) || (i != 0 && entry.offset <= previousOffset) {
			return nil, fmt.Errorf("%w: invalid entry %d", ErrIndexMismatch, i)
		}
		entries[i] = entry
		previousOffset = entry.offset
	}
	return entries, nil
}
