package mdc

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
)

const ioBlockSize = 64 * 1024

var (
	// ErrPackedWordAlignment indicates a byte length that is not a whole number of words.
	ErrPackedWordAlignment = errors.New("mdc: byte length is not aligned to a 4-byte packed-word boundary")

	// ErrPackedWordFileTooLarge indicates that the input cannot fit in a Go slice.
	ErrPackedWordFileTooLarge = errors.New("mdc: file contains too many packed words for this platform")

	// ErrPackedWordBufferTooSmall indicates that scratch space is shorter than one word.
	ErrPackedWordBufferTooSmall = errors.New("mdc: I/O buffer must hold at least one 4-byte packed word")

	// ErrPackedWordLimitExceeded indicates that ReadPackedWordsFileLimit rejected the
	// declared file size.
	ErrPackedWordLimitExceeded = errors.New("mdc: file exceeds configured packed-word limit")

	// ErrInvalidWrite indicates that an io.Writer violated the Writer contract.
	ErrInvalidWrite = errors.New("mdc: writer returned an invalid byte count")
)

// WritePackedWords serializes low-level words to w in little-endian order. It uses one bounded work
// buffer and propagates short writes and writer errors.
func WritePackedWords(w io.Writer, words []uint32) error {
	if len(words) == 0 {
		return nil
	}
	buf := make([]byte, ioBlockSize)
	return WritePackedWordsBuffer(w, words, buf)
}

// WritePackedWordsBuffer serializes words using caller-owned scratch space. A buffer of at
// least four bytes is required; extra bytes that do not form a complete word are
// ignored. Reusing the same buffer makes repeated bulk writes allocation-free.
func WritePackedWordsBuffer(w io.Writer, words []uint32, buf []byte) error {
	if len(words) == 0 {
		return nil
	}
	wordsPerBlock := len(buf) / PackedWordSize
	if wordsPerBlock == 0 {
		return ErrPackedWordBufferTooSmall
	}
	buf = buf[:wordsPerBlock*PackedWordSize]

	for len(words) > 0 {
		n := min(len(words), wordsPerBlock)
		block := buf[:n*PackedWordSize]
		for i, word := range words[:n] {
			binary.LittleEndian.PutUint32(block[i*PackedWordSize:], word)
		}
		if err := writeFull(w, block); err != nil {
			return err
		}
		words = words[n:]
	}
	return nil
}

// WritePackedWordsFile writes a headerless packed-word stream. This is not a
// canonical MDC container; use Writer for self-contained .mdc files.
func WritePackedWordsFile(filename string, words []uint32) error {
	file, err := os.Create(filename)
	if err != nil {
		return err
	}

	writeErr := WritePackedWords(file, words)
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}

// ReadPackedWordsFile reads a complete headerless packed-word stream.
func ReadPackedWordsFile(filename string) ([]uint32, error) {
	return readFile(filename, nil)
}

// ReadPackedWordsFileLimit is ReadPackedWordsFile with an allocation guard.
// maxTicks=0 accepts only an empty stream.
func ReadPackedWordsFileLimit(filename string, maxTicks uint64) ([]uint32, error) {
	return readFile(filename, &maxTicks)
}

func readFile(filename string, maxTicks *uint64) ([]uint32, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}

	info, statErr := file.Stat()
	if statErr != nil {
		_ = file.Close()
		return nil, statErr
	}
	if info.Size()%PackedWordSize != 0 {
		_ = file.Close()
		return nil, ErrPackedWordAlignment
	}

	wordCount64 := info.Size() / PackedWordSize
	if maxTicks != nil && uint64(wordCount64) > *maxTicks {
		_ = file.Close()
		return nil, ErrPackedWordLimitExceeded
	}
	maxInt := int64(^uint(0) >> 1)
	if wordCount64 > maxInt {
		_ = file.Close()
		return nil, ErrPackedWordFileTooLarge
	}

	words := make([]uint32, int(wordCount64))
	readErr := readWords(file, words)
	closeErr := file.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return words, nil
}

func readWords(r io.Reader, words []uint32) error {
	buf := make([]byte, ioBlockSize)
	wordOffset := 0
	for wordOffset < len(words) {
		n := min(len(words)-wordOffset, len(buf)/PackedWordSize)
		block := buf[:n*PackedWordSize]
		if _, err := io.ReadFull(r, block); err != nil {
			return fmt.Errorf("mdc: read tick %d: %w", wordOffset, err)
		}
		for i := 0; i < n; i++ {
			words[wordOffset+i] = binary.LittleEndian.Uint32(block[i*PackedWordSize:])
		}
		wordOffset += n
	}
	return nil
}

func writeFull(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		if n < 0 || n > len(data) {
			return ErrInvalidWrite
		}
		if n > 0 {
			data = data[n:]
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

// PackedWordEncoder serializes individual low-level words. It is
// not safe for concurrent use; synchronize access or use one encoder per stream.
type PackedWordEncoder struct {
	writer io.Writer
	buf    [PackedWordSize]byte
}

// NewPackedWordEncoder creates a low-level word encoder that writes to w.
func NewPackedWordEncoder(w io.Writer) *PackedWordEncoder {
	return &PackedWordEncoder{writer: w}
}

// Encode writes one packed word in little-endian order. It tolerates partial
// writes and returns io.ErrShortWrite if a writer makes no progress.
func (e *PackedWordEncoder) Encode(word uint32) error {
	binary.LittleEndian.PutUint32(e.buf[:], word)
	return writeFull(e.writer, e.buf[:])
}

// EncodeDelta validates and writes one decoded Delta.
func (e *PackedWordEncoder) EncodeDelta(delta Delta) error {
	if err := delta.Validate(); err != nil {
		return err
	}
	return e.Encode(delta.Encode())
}

// PackedWordDecoder deserializes individual low-level words. It
// is not safe for concurrent use; synchronize access or use one per stream.
type PackedWordDecoder struct {
	reader io.Reader
	buf    [PackedWordSize]byte
}

// NewPackedWordDecoder creates a low-level word decoder that reads from r.
func NewPackedWordDecoder(r io.Reader) *PackedWordDecoder {
	return &PackedWordDecoder{reader: r}
}

// Decode reads one packed word in little-endian order. It returns io.EOF when
// no bytes remain and io.ErrUnexpectedEOF for a truncated final word.
func (d *PackedWordDecoder) Decode() (uint32, error) {
	if _, err := io.ReadFull(d.reader, d.buf[:]); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(d.buf[:]), nil
}

// DecodeDelta reads and decodes one Delta.
func (d *PackedWordDecoder) DecodeDelta() (Delta, error) {
	packed, err := d.Decode()
	if err != nil {
		return Delta{}, err
	}
	return DecodeWord(packed), nil
}
