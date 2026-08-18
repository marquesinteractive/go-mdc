package mdc

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"math"
)

// Writer emits the canonical self-contained MDC container. It is not safe for
// concurrent use. Blocks are independent and are flushed automatically when a
// delta cannot be represented, the session/tick size changes, or the configured
// block size is reached.
type Writer struct {
	writer   io.Writer
	metadata Metadata
	config   WriterConfig

	words     []uint32
	overrides []overrideEntry
	index     []blockIndexEntry
	payload   []byte

	offset        uint64
	sequence      uint32
	totalTicks    uint64
	blockSession  uint32
	blockTickSize Rational
	baseTimestamp int64
	baseBidTicks  int64
	previousTime  int64
	previousBid   int64
	havePrevious  bool

	closed bool
	err    error
}

// NewWriter creates a finite MDC writer with an index and canonical defaults.
func NewWriter(w io.Writer, metadata Metadata) (*Writer, error) {
	return NewWriterConfig(w, metadata, DefaultWriterConfig())
}

// NewStreamWriter creates an open-ended MDC writer without a final index.
// Close still flushes the final block.
func NewStreamWriter(w io.Writer, metadata Metadata) (*Writer, error) {
	config := DefaultWriterConfig()
	config.WriteIndex = false
	return NewWriterConfig(w, metadata, config)
}

// NewWriterConfig creates a writer with explicit block and index behavior.
func NewWriterConfig(w io.Writer, metadata Metadata, config WriterConfig) (*Writer, error) {
	if w == nil {
		return nil, fmt.Errorf("%w: nil writer", ErrInvalidMetadata)
	}
	if err := metadata.validate(); err != nil {
		return nil, err
	}
	metadata.TickSize = metadata.TickSize.canonical()
	var err error
	config, err = config.normalized()
	if err != nil {
		return nil, err
	}

	writer := &Writer{
		writer:        w,
		metadata:      metadata,
		config:        config,
		words:         make([]uint32, 0, int(config.MaxBlockTicks)),
		overrides:     make([]overrideEntry, 0),
		blockTickSize: metadata.TickSize,
	}
	if config.WriteIndex {
		writer.index = make([]blockIndexEntry, 0, 64)
	}
	if err := writer.writeHeader(); err != nil {
		return nil, err
	}
	return writer, nil
}

// Metadata returns the immutable file-level interpretation contract.
func (w *Writer) Metadata() Metadata {
	return w.metadata
}

// WriteTick appends one fully interpreted record.
func (w *Writer) WriteTick(record Record) error {
	if w.closed {
		return ErrClosed
	}
	if w.err != nil {
		return w.err
	}

	tickSize := w.blockTickSize
	if record.TickSize.Den == 0 && record.TickSize.Num != 0 {
		return w.fail(fmt.Errorf("%w: tick size numerator requires a denominator", ErrInvalidRecord))
	}
	if record.TickSize.Den != 0 {
		if err := record.TickSize.validate(); err != nil {
			return w.fail(fmt.Errorf("%w: %v", ErrInvalidRecord, err))
		}
		tickSize = record.TickSize.canonical()
	}

	if w.havePrevious {
		if record.Timestamp < w.previousTime && w.metadata.Ordering != SourceOrder {
			return w.fail(fmt.Errorf("%w: timestamp %d follows %d", ErrOrderingViolation, record.Timestamp, w.previousTime))
		}
		if record.Timestamp == w.previousTime && w.metadata.Ordering == StrictlyIncreasing {
			return w.fail(fmt.Errorf("%w: repeated timestamp %d", ErrOrderingViolation, record.Timestamp))
		}
	}

	startNew := len(w.words) == 0
	if !startNew {
		startNew = uint32(len(w.words)) >= w.config.MaxBlockTicks ||
			record.Session != w.blockSession ||
			!tickSize.equal(w.blockTickSize)
	}

	var deltaT uint16
	var deltaBid int8
	if !startNew {
		var fits bool
		deltaT, fits = normalTimeDelta(w.previousTime, record.Timestamp)
		if !fits {
			startNew = true
		}
	}
	if !startNew {
		var fits bool
		deltaBid, fits = normalBidDelta(w.previousBid, record.BidTicks)
		if !fits {
			startNew = true
		}
	}

	if startNew && len(w.words) != 0 {
		if err := w.flushBlock(); err != nil {
			return err
		}
	}
	if len(w.words) == 0 {
		w.blockSession = record.Session
		w.blockTickSize = tickSize
		w.baseTimestamp = record.Timestamp
		w.baseBidTicks = record.BidTicks
		deltaT = 0
		deltaBid = 0
	}

	w.appendWord(deltaT, deltaBid, record.Spread, record.Flags)
	w.previousTime = record.Timestamp
	w.previousBid = record.BidTicks
	w.havePrevious = true
	w.totalTicks++
	return nil
}

// WriteBatch appends records in order and returns the number accepted before an
// error. A successful return always reports len(records).
func (w *Writer) WriteBatch(records []Record) (int, error) {
	for i, record := range records {
		if err := w.WriteTick(record); err != nil {
			return i, err
		}
	}
	return len(records), nil
}

// Flush emits the current independent block without closing the container.
func (w *Writer) Flush() error {
	if w.closed {
		return ErrClosed
	}
	if w.err != nil {
		return w.err
	}
	return w.flushBlock()
}

// Close flushes the final block and writes the finite-file index when enabled.
// It does not close the underlying io.Writer.
func (w *Writer) Close() error {
	if w.closed {
		return w.err
	}
	w.closed = true
	if w.err != nil {
		return w.err
	}
	if err := w.flushBlock(); err != nil {
		return err
	}
	if w.config.WriteIndex {
		if err := w.writeIndex(); err != nil {
			return err
		}
	}
	return nil
}

func (w *Writer) fail(err error) error {
	if err != nil && w.err == nil {
		w.err = err
	}
	return w.err
}

func (w *Writer) writeBytes(data []byte) error {
	if err := writeFull(w.writer, data); err != nil {
		return w.fail(err)
	}
	w.offset += uint64(len(data))
	return nil
}

func (w *Writer) writeHeader() error {
	instrumentBytes := []byte(w.metadata.Instrument)
	priceUnitBytes := []byte(w.metadata.PriceUnit)
	headerBytes := fileHeaderFixedSize + len(instrumentBytes) + len(priceUnitBytes)
	if remainder := headerBytes % 8; remainder != 0 {
		headerBytes += 8 - remainder
	}
	header := make([]byte, headerBytes)
	putMagic(header, fileMagic)
	header[4] = formatMajor
	header[5] = formatMinor
	header[6] = endiannessLittle
	if w.config.WriteIndex {
		header[7] = fileFlagIndex
	} else {
		header[7] = fileFlagStreaming
	}
	binary.LittleEndian.PutUint32(header[8:12], uint32(headerBytes))
	binary.LittleEndian.PutUint32(header[12:16], blockHeaderSize)
	header[16] = byte(w.metadata.TimeUnit)
	header[17] = byte(w.metadata.Ordering)
	header[18] = checksumCRC32C
	header[19] = priceModeTicks
	header[20] = byte(w.metadata.SpreadUnit)
	header[21] = textUTF8
	binary.LittleEndian.PutUint64(header[24:32], uint64(w.metadata.TickSize.Num))
	binary.LittleEndian.PutUint64(header[32:40], w.metadata.TickSize.Den)
	binary.LittleEndian.PutUint32(header[40:44], uint32(len(instrumentBytes)))
	binary.LittleEndian.PutUint32(header[44:48], w.config.MaxBlockTicks)
	binary.LittleEndian.PutUint32(header[48:52], featureOverrides)
	binary.LittleEndian.PutUint32(header[52:56], uint32(len(priceUnitBytes)))
	binary.LittleEndian.PutUint64(header[56:64], unknownTickCount)
	copy(header[fileHeaderFixedSize:], instrumentBytes)
	copy(header[fileHeaderFixedSize+len(instrumentBytes):], priceUnitBytes)
	binary.LittleEndian.PutUint32(header[64:68], crcWithZeroedField(header, 64))
	return w.writeBytes(header)
}

func (w *Writer) appendWord(deltaT uint16, deltaBid int8, spread uint32, flags uint32) {
	index := uint32(len(w.words))
	w.words = append(w.words, Pack(deltaT, deltaBid, uint8(spread), uint8(flags)))
	var mask uint8
	if spread > uint32(MaxSpread) {
		mask |= 1
	}
	if flags > uint32(MaxFlag) {
		mask |= 2
	}
	if mask != 0 {
		override := overrideEntry{tickIndex: index, mask: mask}
		if mask&1 != 0 {
			override.spread = spread
		}
		if mask&2 != 0 {
			override.flags = flags
		}
		w.overrides = append(w.overrides, override)
	}
}

func (w *Writer) flushBlock() error {
	if len(w.words) == 0 {
		return nil
	}
	if w.sequence == math.MaxUint32 {
		return w.fail(fmt.Errorf("%w: block sequence limit reached", ErrInvalidRecord))
	}
	blockBytes, ok := encodedBlockBytes(uint32(len(w.words)), uint32(len(w.overrides)))
	if !ok || blockBytes > math.MaxUint32 || blockBytes > uint64(maxInt()) {
		return w.fail(fmt.Errorf("%w: encoded block length overflow", ErrInvalidRecord))
	}
	payloadBytes := int(blockBytes) - blockHeaderSize
	if cap(w.payload) < payloadBytes {
		w.payload = make([]byte, payloadBytes)
	} else {
		w.payload = w.payload[:payloadBytes]
		clear(w.payload)
	}

	for i, word := range w.words {
		binary.LittleEndian.PutUint32(w.payload[i*PackedWordSize:], word)
	}
	overrideOffset := len(w.words) * PackedWordSize
	for i, override := range w.overrides {
		entry := w.payload[overrideOffset+i*overrideEntrySize:]
		binary.LittleEndian.PutUint32(entry[0:4], override.tickIndex)
		entry[4] = override.mask
		binary.LittleEndian.PutUint32(entry[8:12], override.spread)
		binary.LittleEndian.PutUint32(entry[12:16], override.flags)
	}

	var header [blockHeaderSize]byte
	putMagic(header[:], blockMagic)
	header[4] = 1
	if len(w.overrides) != 0 {
		header[5] = blockFlagOverride
	}
	binary.LittleEndian.PutUint16(header[6:8], blockHeaderSize)
	binary.LittleEndian.PutUint64(header[8:16], blockBytes)
	binary.LittleEndian.PutUint32(header[16:20], uint32(len(w.words)))
	binary.LittleEndian.PutUint32(header[20:24], uint32(len(w.overrides)))
	binary.LittleEndian.PutUint32(header[24:28], w.sequence)
	binary.LittleEndian.PutUint32(header[28:32], w.blockSession)
	binary.LittleEndian.PutUint64(header[32:40], uint64(w.baseTimestamp))
	binary.LittleEndian.PutUint64(header[40:48], uint64(w.baseBidTicks))
	binary.LittleEndian.PutUint64(header[48:56], uint64(w.blockTickSize.Num))
	binary.LittleEndian.PutUint64(header[56:64], w.blockTickSize.Den)
	binary.LittleEndian.PutUint32(header[64:68], crc32.Checksum(w.payload, crcTable))
	binary.LittleEndian.PutUint32(header[68:72], crcWithZeroedField(header[:], 68))

	if w.config.WriteIndex {
		w.index = append(w.index, blockIndexEntry{
			offset:        w.offset,
			baseTimestamp: w.baseTimestamp,
			tickCount:     uint32(len(w.words)),
			sequence:      w.sequence,
			session:       w.blockSession,
		})
	}
	if err := w.writeBytes(header[:]); err != nil {
		return err
	}
	if err := w.writeBytes(w.payload); err != nil {
		return err
	}
	w.sequence++
	w.words = w.words[:0]
	w.overrides = w.overrides[:0]
	return nil
}

func (w *Writer) writeIndex() error {
	if len(w.index) > maxInt()/indexEntrySize {
		return w.fail(fmt.Errorf("%w: index length overflow", ErrInvalidRecord))
	}
	entries := make([]byte, len(w.index)*indexEntrySize)
	for i, item := range w.index {
		entry := entries[i*indexEntrySize:]
		binary.LittleEndian.PutUint64(entry[0:8], item.offset)
		binary.LittleEndian.PutUint64(entry[8:16], uint64(item.baseTimestamp))
		binary.LittleEndian.PutUint32(entry[16:20], item.tickCount)
		binary.LittleEndian.PutUint32(entry[20:24], item.sequence)
		binary.LittleEndian.PutUint32(entry[24:28], item.session)
	}
	var header [indexHeaderSize]byte
	putMagic(header[:], indexMagic)
	header[4] = 1
	binary.LittleEndian.PutUint64(header[8:16], uint64(len(w.index)))
	binary.LittleEndian.PutUint32(header[16:20], indexEntrySize)
	binary.LittleEndian.PutUint32(header[20:24], crc32.Checksum(header[:20], crcTable))

	var trailer [indexTrailerSize]byte
	putMagic(trailer[:], trailerMagic)
	trailer[4] = 1
	binary.LittleEndian.PutUint64(trailer[8:16], uint64(indexHeaderSize+len(entries)))
	binary.LittleEndian.PutUint32(trailer[16:20], crc32.Checksum(entries, crcTable))
	binary.LittleEndian.PutUint32(trailer[20:24], crc32.Checksum(trailer[:20], crcTable))

	if err := w.writeBytes(header[:]); err != nil {
		return err
	}
	if err := w.writeBytes(entries); err != nil {
		return err
	}
	return w.writeBytes(trailer[:])
}

func normalTimeDelta(previous, current int64) (uint16, bool) {
	if current < previous {
		return 0, false
	}
	delta := uint64(current) - uint64(previous)
	if delta > uint64(MaxDeltaT) {
		return 0, false
	}
	return uint16(delta), true
}

func normalBidDelta(previous, current int64) (int8, bool) {
	if current >= previous {
		magnitude := uint64(current) - uint64(previous)
		if magnitude > uint64(MaxDeltaBid) {
			return 0, false
		}
		return int8(magnitude), true
	}
	magnitude := uint64(previous) - uint64(current)
	if magnitude > uint64(-int64(MinDeltaBid)) {
		return 0, false
	}
	return int8(-int64(magnitude)), true
}
