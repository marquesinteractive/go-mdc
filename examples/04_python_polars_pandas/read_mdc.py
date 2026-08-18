"""Independent NumPy interoperability for MDC containers and packed words."""

from collections.abc import Iterator
from contextlib import contextmanager
from dataclasses import dataclass
from pathlib import Path
from math import gcd
import struct
import unicodedata
from typing import TypeAlias

import numpy as np


PathLike: TypeAlias = str | Path
WORD_SIZE = 4
RAW_DTYPE = np.dtype("<u4")
_CRC32C_POLY = 0x82F63B78


def _make_crc32c_table() -> tuple[int, ...]:
    table = []
    for value in range(256):
        crc = value
        for _ in range(8):
            crc = (crc >> 1) ^ (_CRC32C_POLY if crc & 1 else 0)
        table.append(crc & 0xFFFFFFFF)
    return tuple(table)


_CRC32C_TABLE = _make_crc32c_table()


def crc32c(data: bytes | bytearray | memoryview) -> int:
    """Return CRC-32C/Castagnoli without an optional native dependency."""
    crc = 0xFFFFFFFF
    for value in data:
        crc = _CRC32C_TABLE[(crc ^ value) & 0xFF] ^ (crc >> 8)
    return (~crc) & 0xFFFFFFFF


def _crc_with_zeroed_field(data: memoryview, offset: int) -> int:
    materialized = bytearray(data)
    materialized[offset : offset + 4] = b"\x00\x00\x00\x00"
    return crc32c(materialized)


@dataclass(frozen=True)
class MDCMetadata:
    instrument: str
    price_unit: str
    time_unit: int
    ordering: int
    tick_size_num: int
    tick_size_den: int
    spread_unit: int


def load_raw_words(filepath: PathLike, *, memory_map: bool = False) -> np.ndarray:
    """Load a headerless sequence of little-endian 16/8/4/4 words."""
    path = Path(filepath)
    size = path.stat().st_size
    if size % WORD_SIZE:
        raise ValueError(f"raw word byte length must be divisible by {WORD_SIZE}; got {size}")
    if size == 0:
        return np.empty(0, dtype=RAW_DTYPE)
    if memory_map:
        return np.memmap(path, mode="r", dtype=RAW_DTYPE)
    return np.fromfile(path, dtype=RAW_DTYPE)


@contextmanager
def mapped_raw_words(filepath: PathLike) -> Iterator[np.ndarray]:
    """Yield memory-mapped raw words and close the mapping deterministically."""
    words = load_raw_words(filepath, memory_map=True)
    try:
        yield words
    finally:
        mapping = getattr(words, "_mmap", None)
        if mapping is not None:
            mapping.close()


def decode_packed_words(raw: np.ndarray) -> dict[str, np.ndarray]:
    """Vectorize fields from an array of packed 16/8/4/4 words."""
    words = np.asarray(raw, dtype=RAW_DTYPE)
    delta_t = (words & np.uint32(0xFFFF)).astype(np.uint16)
    delta_bid_u8 = ((words >> np.uint32(16)) & np.uint32(0xFF)).astype(np.uint8)
    delta_bid = delta_bid_u8.view(np.int8)
    spread = ((words >> np.uint32(24)) & np.uint32(0x0F)).astype(np.uint8)
    flags = ((words >> np.uint32(28)) & np.uint32(0x0F)).astype(np.uint8)
    return {"delta_t": delta_t, "delta_bid": delta_bid, "spread": spread, "flag": flags}


def load_mdc_to_numpy(
    filepath: PathLike,
    *,
    max_header_bytes: int = 1 << 20,
    max_block_ticks: int = 1 << 20,
    max_block_bytes: int = 64 << 20,
    max_overrides: int = 1 << 20,
    max_index_entries: int = 1 << 20,
    max_file_bytes: int = 1 << 30,
    max_total_ticks: int = 100_000_000,
) -> tuple[MDCMetadata, dict[str, np.ndarray]]:
    """Load and fully verify one finite canonical MDC container."""
    path = Path(filepath)
    size = path.stat().st_size
    if size > max_file_bytes:
        raise ValueError(f"MDC file exceeds configured limit: {size} > {max_file_bytes}")
    raw = path.read_bytes()
    view = memoryview(raw)
    if len(view) < 72 or view[:4] != b"MDCF":
        raise ValueError("invalid MDC file header")
    if view[4] != 1 or view[5] != 0 or view[6] != 1:
        raise ValueError("unsupported MDC format version or endianness")
    file_flags = view[7]
    if file_flags not in (1, 2):
        raise ValueError("unsupported MDC file flags")
    header_bytes, block_header_bytes = struct.unpack_from("<II", view, 8)
    if (
        header_bytes < 72
        or header_bytes % 8
        or header_bytes > max_header_bytes
        or header_bytes > len(view)
        or block_header_bytes != 80
    ):
        raise ValueError("invalid MDC header length")
    header = view[:header_bytes]
    expected_header_crc = struct.unpack_from("<I", header, 64)[0]
    if _crc_with_zeroed_field(header, 64) != expected_header_crc:
        raise ValueError("MDC file-header CRC32C mismatch")
    if (
        view[18] != 1
        or view[19] != 1
        or view[21] != 1
        or any(view[22:24])
        or any(view[68:72])
        or struct.unpack_from("<I", view, 48)[0] != 1
    ):
        raise ValueError("unsupported MDC file feature")
    tick_num = struct.unpack_from("<q", view, 24)[0]
    tick_den = struct.unpack_from("<Q", view, 32)[0]
    declared_ticks = struct.unpack_from("<Q", view, 56)[0]
    instrument_bytes = struct.unpack_from("<I", view, 40)[0]
    price_unit_bytes = struct.unpack_from("<I", view, 52)[0]
    declared_max_ticks = struct.unpack_from("<I", view, 44)[0]
    if (
        tick_num <= 0
        or tick_den == 0
        or gcd(tick_num, tick_den) != 1
        or declared_max_ticks == 0
        or declared_max_ticks > max_block_ticks
    ):
        raise ValueError("invalid MDC metadata or block limit")
    instrument_end = 72 + instrument_bytes
    price_unit_end = instrument_end + price_unit_bytes
    if price_unit_end > header_bytes or any(view[price_unit_end:header_bytes]):
        raise ValueError("invalid MDC metadata string lengths or padding")
    instrument = bytes(view[72:instrument_end]).decode("utf-8")
    price_unit = bytes(view[instrument_end:price_unit_end]).decode("utf-8")
    if (
        not instrument
        or not price_unit
        or instrument.strip() != instrument
        or price_unit.strip() != price_unit
        or any(unicodedata.category(character) == "Cc" for character in instrument)
        or any(unicodedata.category(character) == "Cc" for character in price_unit)
    ):
        raise ValueError("non-canonical MDC instrument or price unit")
    metadata = MDCMetadata(
        instrument=instrument,
        price_unit=price_unit,
        time_unit=int(view[16]),
        ordering=int(view[17]),
        tick_size_num=tick_num,
        tick_size_den=tick_den,
        spread_unit=int(view[20]),
    )
    if metadata.time_unit not in (1, 2, 3, 4) or metadata.ordering not in (0, 1, 2) or metadata.spread_unit != 1:
        raise ValueError("invalid MDC metadata enum")

    timestamps: list[int] = []
    bids: list[int] = []
    spreads: list[int] = []
    flags: list[int] = []
    sessions: list[int] = []
    tick_nums: list[int] = []
    tick_dens: list[int] = []
    seen_index: list[tuple[int, int, int, int, int]] = []
    position = header_bytes
    expected_sequence = 0
    last_timestamp: int | None = None

    while True:
        if position == len(view):
            if file_flags == 2:
                break
            raise ValueError("finite MDC container is missing its index")
        if position + 4 > len(view):
            raise ValueError("truncated MDC section magic")
        magic = bytes(view[position : position + 4])
        if magic == b"MDCI":
            if file_flags != 1:
                raise ValueError("index in streaming MDC container")
            position = _validate_index(view, position, seen_index, max_index_entries)
            if position != len(view):
                raise ValueError("trailing bytes after MDC index")
            break
        if magic != b"MDBK" or position + 80 > len(view):
            raise ValueError(f"invalid MDC block at offset {position}")
        block_offset = position
        block_header = view[position : position + 80]
        if (
            block_header[4] != 1
            or block_header[5] & ~1
            or struct.unpack_from("<H", block_header, 6)[0] != 80
            or any(block_header[72:80])
        ):
            raise ValueError("invalid MDC block header")
        if _crc_with_zeroed_field(block_header, 68) != struct.unpack_from("<I", block_header, 68)[0]:
            raise ValueError("MDC block-header CRC32C mismatch")
        block_bytes = struct.unpack_from("<Q", block_header, 8)[0]
        tick_count, override_count, sequence, session = struct.unpack_from("<IIII", block_header, 16)
        base_timestamp, base_bid, block_tick_num = struct.unpack_from("<qqq", block_header, 32)
        block_tick_den = struct.unpack_from("<Q", block_header, 56)[0]
        if (
            tick_count == 0
            or tick_count > max_block_ticks
            or len(timestamps) + tick_count > max_total_ticks
            or override_count > tick_count
            or override_count > max_overrides
            or block_bytes != 80 + 4 * tick_count + 16 * override_count
            or block_bytes > max_block_bytes
            or position + block_bytes > len(view)
            or sequence != expected_sequence
            or block_tick_num <= 0
            or block_tick_den == 0
            or gcd(block_tick_num, block_tick_den) != 1
            or bool(override_count) != bool(block_header[5] & 1)
        ):
            raise ValueError("invalid MDC block dimensions or sequence")
        payload = view[position + 80 : position + block_bytes]
        if crc32c(payload) != struct.unpack_from("<I", block_header, 64)[0]:
            raise ValueError("MDC block-payload CRC32C mismatch")

        block_start = len(timestamps)
        timestamp, bid = base_timestamp, base_bid
        for index in range(tick_count):
            word = struct.unpack_from("<I", payload, index * 4)[0]
            delta_t = word & 0xFFFF
            delta_bid_raw = (word >> 16) & 0xFF
            delta_bid = delta_bid_raw - 256 if delta_bid_raw >= 128 else delta_bid_raw
            if index == 0:
                if delta_t != 0 or delta_bid != 0:
                    raise ValueError("first MDC block word has non-zero deltas")
            else:
                if delta_t == 0xFFFF or delta_bid == -128:
                    raise ValueError("reserved marker in normal MDC block word")
                timestamp += delta_t
                bid += delta_bid
                if not -(1 << 63) <= timestamp < (1 << 63) or not -(1 << 63) <= bid < (1 << 63):
                    raise ValueError("MDC absolute reconstruction overflow")
            if last_timestamp is not None:
                if metadata.ordering != 0 and timestamp < last_timestamp:
                    raise ValueError("MDC ordering violation")
                if metadata.ordering == 2 and timestamp == last_timestamp:
                    raise ValueError("MDC strict-ordering violation")
            last_timestamp = timestamp
            timestamps.append(timestamp)
            bids.append(bid)
            spreads.append((word >> 24) & 0x0F)
            flags.append((word >> 28) & 0x0F)
            sessions.append(session)
            tick_nums.append(block_tick_num)
            tick_dens.append(block_tick_den)

        previous_override = -1
        override_base = 4 * tick_count
        for override_index in range(override_count):
            entry = payload[override_base + override_index * 16 :]
            tick_index = struct.unpack_from("<I", entry, 0)[0]
            mask = int(entry[4])
            spread, flag_value = struct.unpack_from("<II", entry, 8)
            if (
                tick_index >= tick_count
                or tick_index <= previous_override
                or mask not in (1, 2, 3)
                or any(entry[5:8])
            ):
                raise ValueError("invalid MDC override entry")
            absolute_index = block_start + tick_index
            if mask & 1:
                if spread <= 15 or spread & 15 != spreads[absolute_index]:
                    raise ValueError("non-canonical MDC spread override")
                spreads[absolute_index] = spread
            elif spread:
                raise ValueError("unused MDC spread override data")
            if mask & 2:
                if flag_value <= 15 or flag_value & 15 != flags[absolute_index]:
                    raise ValueError("non-canonical MDC flag override")
                flags[absolute_index] = flag_value
            elif flag_value:
                raise ValueError("unused MDC flag override data")
            previous_override = tick_index

        seen_index.append((block_offset, base_timestamp, tick_count, sequence, session))
        expected_sequence += 1
        position += block_bytes

    if declared_ticks != (1 << 64) - 1 and declared_ticks != len(timestamps):
        raise ValueError("MDC declared tick count mismatch")

    columns = {
        "timestamp": np.asarray(timestamps, dtype=np.int64),
        "bid_ticks": np.asarray(bids, dtype=np.int64),
        "spread": np.asarray(spreads, dtype=np.uint32),
        "flags": np.asarray(flags, dtype=np.uint32),
        "session": np.asarray(sessions, dtype=np.uint32),
        "tick_size_num": np.asarray(tick_nums, dtype=np.int64),
        "tick_size_den": np.asarray(tick_dens, dtype=np.uint64),
    }
    return metadata, columns


def _validate_index(
    view: memoryview,
    position: int,
    seen: list[tuple[int, int, int, int, int]],
    max_entries: int,
) -> int:
    if position + 24 > len(view):
        raise ValueError("truncated MDC index header")
    header = view[position : position + 24]
    if header[4] != 1 or any(header[5:8]) or struct.unpack_from("<I", header, 16)[0] != 32:
        raise ValueError("unsupported MDC index header")
    if crc32c(header[:20]) != struct.unpack_from("<I", header, 20)[0]:
        raise ValueError("MDC index-header CRC32C mismatch")
    count = struct.unpack_from("<Q", header, 8)[0]
    if count > max_entries or count != len(seen):
        raise ValueError("MDC index entry count mismatch")
    entries_start = position + 24
    entries_end = entries_start + 32 * count
    trailer_end = entries_end + 24
    if trailer_end > len(view):
        raise ValueError("truncated MDC index")
    entries = view[entries_start:entries_end]
    trailer = view[entries_end:trailer_end]
    if trailer[:4] != b"MDCE" or trailer[4] != 1 or any(trailer[5:8]):
        raise ValueError("invalid MDC index trailer")
    if crc32c(trailer[:20]) != struct.unpack_from("<I", trailer, 20)[0]:
        raise ValueError("MDC index-trailer CRC32C mismatch")
    if struct.unpack_from("<Q", trailer, 8)[0] != 24 + len(entries):
        raise ValueError("MDC index section length mismatch")
    if crc32c(entries) != struct.unpack_from("<I", trailer, 16)[0]:
        raise ValueError("MDC index-entry CRC32C mismatch")
    for index, expected in enumerate(seen):
        entry = entries[index * 32 :]
        actual = (
            struct.unpack_from("<Q", entry, 0)[0],
            struct.unpack_from("<q", entry, 8)[0],
            struct.unpack_from("<I", entry, 16)[0],
            struct.unpack_from("<I", entry, 20)[0],
            struct.unpack_from("<I", entry, 24)[0],
        )
        if any(entry[28:32]) or actual != expected:
            raise ValueError(f"MDC index entry {index} mismatch")
    return trailer_end


def load_mdc_to_polars(filepath: PathLike):
    """Load a verified MDC container into a Polars DataFrame."""
    try:
        import polars as pl
    except ImportError as exc:
        raise RuntimeError("install Polars with: pip install polars") from exc
    metadata, columns = load_mdc_to_numpy(filepath)
    return metadata, pl.DataFrame(columns)


def load_mdc_to_pandas(filepath: PathLike):
    """Load a verified MDC container into a Pandas DataFrame."""
    try:
        import pandas as pd
    except ImportError as exc:
        raise RuntimeError("install Pandas with: pip install pandas") from exc
    metadata, columns = load_mdc_to_numpy(filepath)
    return metadata, pd.DataFrame(columns)


if __name__ == "__main__":
    print("Use load_mdc_to_numpy('ticks.mdc') for a verified MDC container.")
