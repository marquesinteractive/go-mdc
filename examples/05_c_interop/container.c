#include <inttypes.h>
#include <limits.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

enum {
    FILE_HEADER_FIXED = 72,
    BLOCK_HEADER = 80,
    INDEX_HEADER = 24,
    INDEX_ENTRY = 32,
    INDEX_TRAILER = 24,
    OVERRIDE_ENTRY = 16,
    MAX_BLOCK_TICKS = 1 << 20,
};

typedef struct {
    int64_t timestamp;
    int64_t bid;
    uint32_t spread;
    uint32_t flags;
    uint32_t session;
    int64_t tick_num;
    uint64_t tick_den;
} Record;

typedef struct {
    uint64_t offset;
    int64_t base_timestamp;
    uint32_t tick_count;
    uint32_t sequence;
    uint32_t session;
} IndexEntry;

static const Record expected_records[] = {
    {1000000, 34910, 1, 2, 20260817, 5, 1},
    {1000010, 34911, 31, 4660, 20260817, 5, 1},
    {1066000, 35050, 2, 3, 20260817, 5, 1},
    {1066010, 35049, 4, 5, 20260818, 5, 1},
	{1065900, 35048, 6, 7, 20260818, 5, 2},
};

static uint16_t le16(const uint8_t *p) {
    return (uint16_t)p[0] | (uint16_t)((uint16_t)p[1] << 8);
}

static uint32_t le32(const uint8_t *p) {
    return (uint32_t)p[0] | ((uint32_t)p[1] << 8) |
           ((uint32_t)p[2] << 16) | ((uint32_t)p[3] << 24);
}

static uint64_t le64(const uint8_t *p) {
    return (uint64_t)le32(p) | ((uint64_t)le32(p + 4) << 32);
}

static int64_t lei64(const uint8_t *p) {
    uint64_t value = le64(p);
    int64_t result;
    memcpy(&result, &value, sizeof result);
    return result;
}

static uint32_t crc32c(const uint8_t *data, size_t length) {
    uint32_t crc = UINT32_MAX;
    for (size_t i = 0; i < length; ++i) {
        crc ^= data[i];
        for (unsigned bit = 0; bit < 8; ++bit) {
            crc = (crc >> 1) ^ (0x82F63B78u & (uint32_t)-(int32_t)(crc & 1u));
        }
    }
    return ~crc;
}

static uint32_t crc_zero_field(const uint8_t *data, size_t length, size_t offset) {
    uint8_t *copy = malloc(length);
    if (copy == NULL) {
        return 0;
    }
    memcpy(copy, data, length);
    memset(copy + offset, 0, 4);
    uint32_t result = crc32c(copy, length);
    free(copy);
    return result;
}

static int all_zero(const uint8_t *data, size_t length) {
    for (size_t i = 0; i < length; ++i) {
        if (data[i] != 0) {
            return 0;
        }
    }
    return 1;
}

static int add_i64(int64_t value, int64_t delta, int64_t *out) {
    if ((delta > 0 && value > INT64_MAX - delta) ||
        (delta < 0 && value < INT64_MIN - delta)) {
        return 0;
    }
    *out = value + delta;
    return 1;
}

static int record_equal(const Record *left, const Record *right) {
    return left->timestamp == right->timestamp && left->bid == right->bid &&
           left->spread == right->spread && left->flags == right->flags &&
           left->session == right->session && left->tick_num == right->tick_num &&
           left->tick_den == right->tick_den;
}

static int fail(const char *message) {
    fprintf(stderr, "container interop: %s\n", message);
    return 1;
}

int main(int argc, char **argv) {
    const char *path = argc > 1 ? argv[1] : "testdata/golden-container.bin";
    FILE *file = fopen(path, "rb");
    if (file == NULL) {
        perror(path);
        return 1;
    }
    if (fseek(file, 0, SEEK_END) != 0) {
        fclose(file);
        return fail("seek end failed");
    }
    long file_length = ftell(file);
    if (file_length < FILE_HEADER_FIXED || fseek(file, 0, SEEK_SET) != 0) {
        fclose(file);
        return fail("invalid file length");
    }
    size_t length = (size_t)file_length;
    uint8_t *data = malloc(length);
    if (data == NULL || fread(data, 1, length, file) != length || fclose(file) != 0) {
        free(data);
        return fail("read failed");
    }

    if (memcmp(data, "MDCF", 4) != 0 || data[4] != 1 || data[5] != 0 ||
        data[6] != 1 || data[7] != 1 || le32(data + 12) != BLOCK_HEADER) {
        free(data);
        return fail("invalid file header");
    }
    uint32_t header_bytes = le32(data + 8);
    uint32_t instrument_bytes = le32(data + 40);
	uint32_t price_unit_bytes = le32(data + 52);
	uint64_t metadata_bytes = (uint64_t)instrument_bytes + price_unit_bytes;
    if (header_bytes < FILE_HEADER_FIXED || header_bytes % 8 != 0 ||
		header_bytes > length || metadata_bytes > header_bytes - FILE_HEADER_FIXED ||
        crc_zero_field(data, header_bytes, 64) != le32(data + 64) ||
		!all_zero(data + FILE_HEADER_FIXED + metadata_bytes,
				  header_bytes - FILE_HEADER_FIXED - (size_t)metadata_bytes) ||
        instrument_bytes != strlen("WINFUT:B3") ||
        memcmp(data + FILE_HEADER_FIXED, "WINFUT:B3", instrument_bytes) != 0 ||
		price_unit_bytes != strlen("index-point") ||
		memcmp(data + FILE_HEADER_FIXED + instrument_bytes, "index-point", price_unit_bytes) != 0 ||
        data[16] != 3 || data[17] != 0 || data[18] != 1 || data[19] != 1 ||
        data[20] != 1 || data[21] != 1 || lei64(data + 24) != 5 || le64(data + 32) != 1) {
        free(data);
        return fail("metadata or file-header checksum mismatch");
    }

    IndexEntry seen[64];
    size_t seen_count = 0;
    size_t expected_index = 0;
    size_t position = header_bytes;
    uint32_t expected_sequence = 0;
    while (position + 4 <= length && memcmp(data + position, "MDBK", 4) == 0) {
        if (position + BLOCK_HEADER > length || seen_count == 64) {
            free(data);
            return fail("truncated block header or too many fixture blocks");
        }
        const uint8_t *header = data + position;
        uint64_t block_bytes = le64(header + 8);
        uint32_t tick_count = le32(header + 16);
        uint32_t override_count = le32(header + 20);
        uint32_t sequence = le32(header + 24);
        uint32_t session = le32(header + 28);
        int64_t base_timestamp = lei64(header + 32);
        int64_t base_bid = lei64(header + 40);
        int64_t tick_num = lei64(header + 48);
        uint64_t tick_den = le64(header + 56);
        if (header[4] != 1 || (header[5] & ~1u) != 0 || le16(header + 6) != BLOCK_HEADER ||
            !all_zero(header + 72, 8) || crc_zero_field(header, BLOCK_HEADER, 68) != le32(header + 68) ||
            tick_count == 0 || tick_count > MAX_BLOCK_TICKS || override_count > tick_count ||
            block_bytes != BLOCK_HEADER + 4ull * tick_count + 16ull * override_count ||
            block_bytes > length - position || sequence != expected_sequence ||
            tick_num <= 0 || tick_den == 0 || (!!override_count != !!(header[5] & 1u))) {
            free(data);
            return fail("invalid block header, dimensions, or checksum");
        }
        const uint8_t *payload = header + BLOCK_HEADER;
        size_t payload_bytes = (size_t)block_bytes - BLOCK_HEADER;
        if (crc32c(payload, payload_bytes) != le32(header + 64)) {
            free(data);
            return fail("block payload checksum mismatch");
        }
        Record *block = calloc(tick_count, sizeof *block);
        if (block == NULL) {
            free(data);
            return fail("allocation failed");
        }
        int64_t timestamp = base_timestamp;
        int64_t bid = base_bid;
        for (uint32_t i = 0; i < tick_count; ++i) {
            uint32_t word = le32(payload + 4u * i);
            uint16_t delta_t = (uint16_t)(word & 0xffffu);
			uint8_t delta_bid_raw = (uint8_t)((word >> 16) & 0xffu);
			int16_t delta_bid = delta_bid_raw >= 128u ? (int16_t)delta_bid_raw - 256 : delta_bid_raw;
            if (i == 0) {
                if (delta_t != 0 || delta_bid != 0) {
                    free(block);
                    free(data);
                    return fail("non-zero first block delta");
                }
            } else if (delta_t == UINT16_MAX || delta_bid == INT8_MIN ||
                       !add_i64(timestamp, delta_t, &timestamp) ||
                       !add_i64(bid, delta_bid, &bid)) {
                free(block);
                free(data);
                return fail("reserved marker or absolute overflow");
            }
            block[i] = (Record){timestamp, bid, (word >> 24) & 0x0fu,
                                (word >> 28) & 0x0fu, session, tick_num, tick_den};
        }
        uint32_t previous_override = 0;
        for (uint32_t i = 0; i < override_count; ++i) {
            const uint8_t *entry = payload + 4u * tick_count + OVERRIDE_ENTRY * i;
            uint32_t tick_index = le32(entry);
            uint8_t mask = entry[4];
            uint32_t spread = le32(entry + 8);
            uint32_t flags = le32(entry + 12);
            if (tick_index >= tick_count || (i != 0 && tick_index <= previous_override) ||
                mask == 0 || (mask & ~3u) != 0 || !all_zero(entry + 5, 3)) {
                free(block);
                free(data);
                return fail("invalid override entry");
            }
            if (mask & 1u) {
                if (spread <= 15 || (spread & 15u) != block[tick_index].spread) {
                    free(block);
                    free(data);
                    return fail("non-canonical spread override");
                }
                block[tick_index].spread = spread;
            } else if (spread != 0) {
                free(block);
                free(data);
                return fail("unused spread override data");
            }
            if (mask & 2u) {
                if (flags <= 15 || (flags & 15u) != block[tick_index].flags) {
                    free(block);
                    free(data);
                    return fail("non-canonical flag override");
                }
                block[tick_index].flags = flags;
            } else if (flags != 0) {
                free(block);
                free(data);
                return fail("unused flag override data");
            }
            previous_override = tick_index;
        }
        for (uint32_t i = 0; i < tick_count; ++i) {
            if (expected_index >= sizeof expected_records / sizeof expected_records[0] ||
                !record_equal(&block[i], &expected_records[expected_index])) {
                free(block);
                free(data);
                return fail("decoded record differs from independent oracle");
            }
            ++expected_index;
        }
        free(block);
        seen[seen_count++] = (IndexEntry){position, base_timestamp, tick_count, sequence, session};
        ++expected_sequence;
        position += (size_t)block_bytes;
    }
    if (expected_index != sizeof expected_records / sizeof expected_records[0] ||
        position + INDEX_HEADER + INDEX_TRAILER > length ||
        memcmp(data + position, "MDCI", 4) != 0) {
        free(data);
        return fail("missing records or index");
    }

    const uint8_t *index_header = data + position;
    uint64_t entry_count = le64(index_header + 8);
    if (index_header[4] != 1 || !all_zero(index_header + 5, 3) ||
        le32(index_header + 16) != INDEX_ENTRY ||
        crc32c(index_header, 20) != le32(index_header + 20) || entry_count != seen_count ||
        entry_count > (SIZE_MAX - INDEX_HEADER - INDEX_TRAILER) / INDEX_ENTRY) {
        free(data);
        return fail("invalid index header");
    }
    size_t entries_bytes = (size_t)entry_count * INDEX_ENTRY;
    const uint8_t *entries = index_header + INDEX_HEADER;
    const uint8_t *trailer = entries + entries_bytes;
    if ((size_t)(trailer - data) + INDEX_TRAILER != length ||
        memcmp(trailer, "MDCE", 4) != 0 || trailer[4] != 1 ||
        !all_zero(trailer + 5, 3) || le64(trailer + 8) != INDEX_HEADER + entries_bytes ||
        crc32c(entries, entries_bytes) != le32(trailer + 16) ||
        crc32c(trailer, 20) != le32(trailer + 20)) {
        free(data);
        return fail("invalid index entries or trailer");
    }
    for (size_t i = 0; i < seen_count; ++i) {
        const uint8_t *entry = entries + i * INDEX_ENTRY;
        if (le64(entry) != seen[i].offset || lei64(entry + 8) != seen[i].base_timestamp ||
            le32(entry + 16) != seen[i].tick_count || le32(entry + 20) != seen[i].sequence ||
            le32(entry + 24) != seen[i].session || !all_zero(entry + 28, 4)) {
            free(data);
            return fail("index entry differs from decoded block");
        }
    }
    free(data);
    printf("C interop validated the canonical MDC container and %zu records.\n", expected_index);
    return 0;
}
