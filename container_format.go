package mdc

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"math"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	formatMajor = 1
	formatMinor = 0

	fileHeaderFixedSize  = 72
	blockHeaderSize      = 80
	indexHeaderSize      = 24
	indexEntrySize       = 32
	indexTrailerSize     = 24
	overrideEntrySize    = 16
	unknownTickCount     = math.MaxUint64
	defaultMaxBlockTicks = 4096

	fileFlagIndex     = 1 << 0
	fileFlagStreaming = 1 << 1
	blockFlagOverride = 1 << 0
	featureOverrides  = 1 << 0
	checksumCRC32C    = 1
	endiannessLittle  = 1
	priceModeTicks    = 1
	textUTF8          = 1
)

var (
	fileMagic    = [4]byte{'M', 'D', 'C', 'F'}
	blockMagic   = [4]byte{'M', 'D', 'B', 'K'}
	indexMagic   = [4]byte{'M', 'D', 'C', 'I'}
	trailerMagic = [4]byte{'M', 'D', 'C', 'E'}
	crcTable     = crc32.MakeTable(crc32.Castagnoli)
)

var (
	ErrInvalidMetadata   = errors.New("mdc: invalid metadata")
	ErrInvalidRecord     = errors.New("mdc: invalid record")
	ErrOrderingViolation = errors.New("mdc: ordering contract violated")
	ErrClosed            = errors.New("mdc: writer is closed")
	ErrInvalidFormat     = errors.New("mdc: invalid container format")
	ErrUnsupportedFormat = errors.New("mdc: unsupported container version or feature")
	ErrChecksumMismatch  = errors.New("mdc: checksum mismatch")
	ErrLimitExceeded     = errors.New("mdc: configured reader limit exceeded")
	ErrMissingIndex      = errors.New("mdc: finite container index is missing")
	ErrIndexMismatch     = errors.New("mdc: index does not match decoded blocks")
	ErrSequenceMismatch  = errors.New("mdc: block sequence mismatch")
)

// TimeUnit defines the unit used by absolute timestamps and deltaT.
type TimeUnit uint8

const (
	TimeNanosecond  TimeUnit = 1
	TimeMicrosecond TimeUnit = 2
	TimeMillisecond TimeUnit = 3
	TimeSecond      TimeUnit = 4
)

// Ordering defines the timestamp contract of a container.
type Ordering uint8

const (
	// SourceOrder preserves event order and starts a new block on timestamp regression.
	SourceOrder Ordering = iota
	// NonDecreasing rejects timestamp regressions.
	NonDecreasing
	// StrictlyIncreasing rejects equal or regressing timestamps.
	StrictlyIncreasing
)

// SpreadUnit defines how spread values are interpreted.
type SpreadUnit uint8

const (
	SpreadInTicks SpreadUnit = 1
)

// Rational defines an exact positive price increment without floating point.
type Rational struct {
	Num int64
	Den uint64
}

func (r Rational) validate() error {
	if r.Num <= 0 || r.Den == 0 {
		return fmt.Errorf("%w: tick size must be a positive rational", ErrInvalidMetadata)
	}
	return nil
}

func (r Rational) canonical() Rational {
	divisor := gcd(uint64(r.Num), r.Den)
	if divisor > math.MaxInt64 {
		return r
	}
	return Rational{Num: r.Num / int64(divisor), Den: r.Den / divisor}
}

func (r Rational) equal(other Rational) bool {
	return r.Num == other.Num && r.Den == other.Den
}

// Metadata is the self-contained interpretation contract for one MDC file.
// One file contains one instrument; session and tick-size changes are encoded
// at block boundaries.
type Metadata struct {
	Instrument string
	PriceUnit  string
	TimeUnit   TimeUnit
	Ordering   Ordering
	TickSize   Rational
	SpreadUnit SpreadUnit
}

func (m Metadata) validate() error {
	if err := validateMetadataText("instrument", m.Instrument); err != nil {
		return err
	}
	if uint64(len(m.Instrument)) > math.MaxUint32 {
		return fmt.Errorf("%w: instrument identifier is too large", ErrInvalidMetadata)
	}
	if err := validateMetadataText("price unit", m.PriceUnit); err != nil {
		return err
	}
	if uint64(len(m.PriceUnit)) > math.MaxUint32 {
		return fmt.Errorf("%w: price unit is too large", ErrInvalidMetadata)
	}
	if uint64(fileHeaderFixedSize)+uint64(len(m.Instrument))+uint64(len(m.PriceUnit))+7 > DefaultReaderLimits().MaxHeaderBytes {
		return fmt.Errorf("%w: metadata exceeds the canonical header limit", ErrInvalidMetadata)
	}
	switch m.TimeUnit {
	case TimeNanosecond, TimeMicrosecond, TimeMillisecond, TimeSecond:
	default:
		return fmt.Errorf("%w: unsupported time unit %d", ErrInvalidMetadata, m.TimeUnit)
	}
	switch m.Ordering {
	case SourceOrder, NonDecreasing, StrictlyIncreasing:
	default:
		return fmt.Errorf("%w: unsupported ordering %d", ErrInvalidMetadata, m.Ordering)
	}
	if err := m.TickSize.validate(); err != nil {
		return err
	}
	if m.SpreadUnit != SpreadInTicks {
		return fmt.Errorf("%w: unsupported spread unit %d", ErrInvalidMetadata, m.SpreadUnit)
	}
	return nil
}

func validateMetadataText(label, value string) error {
	if value == "" || !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return fmt.Errorf("%w: %s must be canonical non-empty UTF-8", ErrInvalidMetadata, label)
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return fmt.Errorf("%w: %s contains a control character", ErrInvalidMetadata, label)
		}
	}
	return nil
}

// Record is one fully interpreted market tick. TickSize.Den==0 means continue
// using the current block tick size; a non-zero value changes it and starts a
// new independent block. Session changes also start a new block.
type Record struct {
	Timestamp int64
	BidTicks  int64
	Spread    uint32
	Flags     uint32
	Session   uint32
	TickSize  Rational
}

// WriterConfig controls block granularity and final index emission.
type WriterConfig struct {
	MaxBlockTicks uint32
	WriteIndex    bool
}

// DefaultWriterConfig returns the canonical finite-file configuration.
func DefaultWriterConfig() WriterConfig {
	return WriterConfig{MaxBlockTicks: defaultMaxBlockTicks, WriteIndex: true}
}

func (c WriterConfig) normalized() (WriterConfig, error) {
	if c.MaxBlockTicks == 0 {
		c.MaxBlockTicks = defaultMaxBlockTicks
	}
	if uint64(c.MaxBlockTicks) > DefaultReaderLimits().MaxBlockTicks ||
		uint64(c.MaxBlockTicks) > uint64(maxInt()/PackedWordSize) {
		return WriterConfig{}, fmt.Errorf("%w: max block ticks %d exceeds the canonical limit", ErrInvalidMetadata, c.MaxBlockTicks)
	}
	return c, nil
}

// ReaderLimits bounds all allocations derived from untrusted container fields.
type ReaderLimits struct {
	MaxHeaderBytes  uint64
	MaxBlockTicks   uint64
	MaxBlockBytes   uint64
	MaxOverrides    uint64
	MaxIndexEntries uint64
}

// DefaultReaderLimits returns conservative limits suitable for ordinary files.
func DefaultReaderLimits() ReaderLimits {
	return ReaderLimits{
		MaxHeaderBytes:  1 << 20,
		MaxBlockTicks:   1 << 20,
		MaxBlockBytes:   64 << 20,
		MaxOverrides:    1 << 20,
		MaxIndexEntries: 1 << 20,
	}
}

func (l ReaderLimits) normalized() ReaderLimits {
	d := DefaultReaderLimits()
	if l.MaxHeaderBytes == 0 {
		l.MaxHeaderBytes = d.MaxHeaderBytes
	}
	if l.MaxBlockTicks == 0 {
		l.MaxBlockTicks = d.MaxBlockTicks
	}
	if l.MaxBlockBytes == 0 {
		l.MaxBlockBytes = d.MaxBlockBytes
	}
	if l.MaxOverrides == 0 {
		l.MaxOverrides = d.MaxOverrides
	}
	if l.MaxIndexEntries == 0 {
		l.MaxIndexEntries = d.MaxIndexEntries
	}
	return l
}

type fileHeader struct {
	metadata      Metadata
	flags         uint8
	headerBytes   uint32
	maxBlockTicks uint32
	declaredTicks uint64
}

type blockIndexEntry struct {
	offset        uint64
	baseTimestamp int64
	tickCount     uint32
	sequence      uint32
	session       uint32
}

type overrideEntry struct {
	tickIndex uint32
	mask      uint8
	spread    uint32
	flags     uint32
}

func crcWithZeroedField(data []byte, offset int) uint32 {
	if offset < 0 || offset+4 > len(data) {
		return 0
	}
	var zero [4]byte
	crc := crc32.Update(0, crcTable, data[:offset])
	crc = crc32.Update(crc, crcTable, zero[:])
	return crc32.Update(crc, crcTable, data[offset+4:])
}

func gcd(a, b uint64) uint64 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

func putMagic(dst []byte, magic [4]byte) {
	copy(dst[:4], magic[:])
}

func hasMagic(src []byte, magic [4]byte) bool {
	return len(src) >= 4 && src[0] == magic[0] && src[1] == magic[1] && src[2] == magic[2] && src[3] == magic[3]
}

func addInt64(value int64, delta int64) (int64, bool) {
	if delta > 0 && value > math.MaxInt64-delta {
		return 0, false
	}
	if delta < 0 && value < math.MinInt64-delta {
		return 0, false
	}
	return value + delta, true
}

func encodedBlockBytes(ticks uint32, overrides uint32) (uint64, bool) {
	words := uint64(ticks) * PackedWordSize
	extensions := uint64(overrides) * overrideEntrySize
	total := uint64(blockHeaderSize) + words + extensions
	if total < words || total < extensions {
		return 0, false
	}
	return total, true
}

func parseRational(num int64, den uint64) (Rational, error) {
	r := Rational{Num: num, Den: den}
	if err := r.validate(); err != nil {
		return Rational{}, err
	}
	if gcd(uint64(r.Num), r.Den) != 1 {
		return Rational{}, fmt.Errorf("%w: tick size is not in canonical reduced form", ErrInvalidFormat)
	}
	return r, nil
}

func readUint32(src []byte, offset int) uint32 {
	return binary.LittleEndian.Uint32(src[offset : offset+4])
}

func readUint64(src []byte, offset int) uint64 {
	return binary.LittleEndian.Uint64(src[offset : offset+8])
}

func readInt64(src []byte, offset int) int64 {
	return int64(readUint64(src, offset))
}
