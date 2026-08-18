// Package mdc implements the canonical MDC container and its low-level packed
// word primitive for compact market data.
package mdc

import (
	"errors"
	"fmt"
)

const (
	// PackedWordSize is the serialized size of one low-level packed word, in bytes.
	PackedWordSize = 4

	// EscapeTime is a reserved marker that an application-level protocol may use
	// when a time delta cannot be represented inline. MDC does not serialize the
	// corresponding absolute value; callers must define that side channel.
	EscapeTime uint16 = 0xFFFF

	// EscapePrice is a reserved marker that an application-level protocol may use
	// when a price delta cannot be represented inline. MDC does not serialize the
	// corresponding absolute value; callers must define that side channel.
	EscapePrice int8 = -128

	// MaxDeltaT is the largest inline delta when EscapeTime is reserved.
	MaxDeltaT uint16 = 0xFFFE

	// MinDeltaBid is the smallest inline price delta when EscapePrice is reserved.
	MinDeltaBid int8 = -127

	// MaxDeltaBid is the largest representable inline price delta.
	MaxDeltaBid int8 = 127

	// MaxSpread is the maximum representable spread value (4 bits).
	MaxSpread uint8 = 0x0F

	// MaxFlag is the maximum representable flag value (4 bits).
	MaxFlag uint8 = 0x0F
)

var (
	// ErrDeltaTOutOfRange is returned when a normal semantic delta is outside
	// 0..MaxDeltaT. The raw word domain still round-trips EscapeTime.
	ErrDeltaTOutOfRange = errors.New("mdc: time delta outside normal semantic range")

	// ErrDeltaBidOutOfRange is returned when a normal semantic delta is outside
	// MinDeltaBid..MaxDeltaBid. The raw word domain still round-trips EscapePrice.
	ErrDeltaBidOutOfRange = errors.New("mdc: bid delta outside normal semantic range")

	// ErrSpreadOutOfRange is returned by checked APIs for spreads above 15.
	ErrSpreadOutOfRange = errors.New("mdc: spread exceeds 4-bit range")

	// ErrFlagOutOfRange is returned by checked APIs for flags above 15.
	ErrFlagOutOfRange = errors.New("mdc: flag exceeds 4-bit range")
)

// Delta represents the four fields embedded in one low-level packed word.
// Units and meanings come from the enclosing MDC container contract.
type Delta struct {
	DeltaT   uint16
	DeltaBid int8
	Spread   uint8
	Flag     uint8
}

// PackedWord is the type-safe in-memory form of one 16/8/4/4 word.
type PackedWord uint32

// Pack encodes the low four bits of spread and flag into one uint32.
//
// Bit layout:
//   - deltaT:   bits 0..15
//   - deltaBid: bits 16..23, in two's-complement form
//   - spread:   bits 24..27
//   - flag:     bits 28..31
//
// Pack intentionally masks spread and flag for compatibility. Use PackChecked
// when silent truncation is not acceptable.
func Pack(deltaT uint16, deltaBid int8, spread uint8, flag uint8) uint32 {
	return uint32(deltaT) |
		uint32(uint8(deltaBid))<<16 |
		uint32(spread&MaxSpread)<<24 |
		uint32(flag&MaxFlag)<<28
}

// PackChecked validates four-bit fields before encoding a tick.
func PackChecked(deltaT uint16, deltaBid int8, spread uint8, flag uint8) (uint32, error) {
	if spread > MaxSpread {
		return 0, ErrSpreadOutOfRange
	}
	if flag > MaxFlag {
		return 0, ErrFlagOutOfRange
	}
	return Pack(deltaT, deltaBid, spread, flag), nil
}

// PackNormalChecked validates the low-level normal semantic domain before
// narrowing deltaT and deltaBid to their wire types. EscapeTime and EscapePrice
// remain round-trippable through Pack, PackChecked, and Unpack, but are rejected
// here because they are reserved by the normal semantic-domain convention.
func PackNormalChecked(deltaT int64, deltaBid int64, spread uint8, flag uint8) (uint32, error) {
	if deltaT < 0 || deltaT > int64(MaxDeltaT) {
		return 0, fmt.Errorf("%w: got %d, want 0..%d", ErrDeltaTOutOfRange, deltaT, MaxDeltaT)
	}
	if deltaBid < int64(MinDeltaBid) || deltaBid > int64(MaxDeltaBid) {
		return 0, fmt.Errorf("%w: got %d, want %d..%d", ErrDeltaBidOutOfRange, deltaBid, MinDeltaBid, MaxDeltaBid)
	}
	return PackChecked(uint16(deltaT), int8(deltaBid), spread, flag)
}

// PackAbsoluteChecked computes deltas from absolute int64 values and validates
// them before narrowing. It is the safe entry point when timestamps and bid
// prices have not already been converted to low-level delta types.
func PackAbsoluteChecked(
	previousTimestamp, currentTimestamp int64,
	previousBidTicks, currentBidTicks int64,
	spread, flag uint8,
) (uint32, error) {
	if currentTimestamp < previousTimestamp {
		return 0, fmt.Errorf("%w: current timestamp %d precedes previous timestamp %d",
			ErrDeltaTOutOfRange, currentTimestamp, previousTimestamp)
	}
	deltaT := uint64(currentTimestamp) - uint64(previousTimestamp)
	if deltaT > uint64(MaxDeltaT) {
		return 0, fmt.Errorf("%w: absolute timestamps %d -> %d exceed maximum delta %d",
			ErrDeltaTOutOfRange, previousTimestamp, currentTimestamp, MaxDeltaT)
	}

	var deltaBid int64
	if currentBidTicks >= previousBidTicks {
		magnitude := uint64(currentBidTicks) - uint64(previousBidTicks)
		if magnitude > uint64(MaxDeltaBid) {
			return 0, fmt.Errorf("%w: absolute bid ticks %d -> %d exceed positive maximum %d",
				ErrDeltaBidOutOfRange, previousBidTicks, currentBidTicks, MaxDeltaBid)
		}
		deltaBid = int64(magnitude)
	} else {
		magnitude := uint64(previousBidTicks) - uint64(currentBidTicks)
		if magnitude > uint64(-int64(MinDeltaBid)) {
			return 0, fmt.Errorf("%w: absolute bid ticks %d -> %d exceed negative minimum %d",
				ErrDeltaBidOutOfRange, previousBidTicks, currentBidTicks, MinDeltaBid)
		}
		deltaBid = -int64(magnitude)
	}

	return PackNormalChecked(int64(deltaT), deltaBid, spread, flag)
}

// Unpack decodes one uint32 into its constituent fields. The explicit uint8
// conversion reconstructs the signed two's-complement byte without sign
// extension during the shift.
func Unpack(packed uint32) (deltaT uint16, deltaBid int8, spread uint8, flag uint8) {
	deltaT = uint16(packed & 0xFFFF)
	deltaBid = int8(uint8((packed >> 16) & 0xFF))
	spread = uint8((packed >> 24) & 0x0F)
	flag = uint8((packed >> 28) & 0x0F)
	return deltaT, deltaBid, spread, flag
}

// Validate checks fields that can exceed their on-wire width.
func (t Delta) Validate() error {
	if t.Spread > MaxSpread {
		return ErrSpreadOutOfRange
	}
	if t.Flag > MaxFlag {
		return ErrFlagOutOfRange
	}
	return nil
}

// ValidateNormal validates t against the normal semantic domain, where
// EscapeTime and EscapePrice are reserved and require an outer protocol.
func (t Delta) ValidateNormal() error {
	if err := t.Validate(); err != nil {
		return err
	}
	if t.DeltaT == EscapeTime {
		return ErrDeltaTOutOfRange
	}
	if t.DeltaBid == EscapePrice {
		return ErrDeltaBidOutOfRange
	}
	return nil
}

// UsesEscapeMarker reports whether the tick contains a reserved escape marker.
// It does not validate any application-level overflow payload.
func (t Delta) UsesEscapeMarker() bool {
	return t.DeltaT == EscapeTime || t.DeltaBid == EscapePrice
}

// Encode converts a Delta to its uint32 representation. Like Pack, it masks the
// low-width fields; call Validate first when inputs are not trusted.
func (t Delta) Encode() uint32 {
	return Pack(t.DeltaT, t.DeltaBid, t.Spread, t.Flag)
}

// Pack returns the type-safe encoded representation of t.
func (t Delta) Pack() PackedWord {
	return PackedWord(t.Encode())
}

// DecodeWord converts one packed uint32 to a Delta.
func DecodeWord(packed uint32) Delta {
	dt, db, sp, fl := Unpack(packed)
	return Delta{DeltaT: dt, DeltaBid: db, Spread: sp, Flag: fl}
}

// Uint32 exposes the primitive representation of p.
func (p PackedWord) Uint32() uint32 {
	return uint32(p)
}

// Decode converts p to a Delta.
func (p PackedWord) Decode() Delta {
	return DecodeWord(uint32(p))
}

// PackDeltas packs as many deltas as fit in dst and returns the number written.
// It performs no allocations when dst is supplied by the caller.
func PackDeltas(dst []uint32, src []Delta) int {
	n := min(len(dst), len(src))
	for i := 0; i < n; i++ {
		dst[i] = src[i].Encode()
	}
	return n
}

// UnpackWords decodes as many words as fit in dst and returns the number written.
// It performs no allocations when dst is supplied by the caller.
func UnpackWords(dst []Delta, src []uint32) int {
	n := min(len(dst), len(src))
	for i := 0; i < n; i++ {
		dst[i] = DecodeWord(src[i])
	}
	return n
}
