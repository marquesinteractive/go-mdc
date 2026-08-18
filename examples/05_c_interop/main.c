#include <errno.h>
#include <inttypes.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

static uint32_t pack_tick(uint16_t delta_t, int8_t delta_bid, uint8_t spread, uint8_t flag) {
    return (uint32_t)delta_t |
           ((uint32_t)(uint8_t)delta_bid << 16) |
           ((uint32_t)(spread & 0x0fu) << 24) |
           ((uint32_t)(flag & 0x0fu) << 28);
}

static uint32_t read_le32(const uint8_t bytes[4]) {
    return (uint32_t)bytes[0] |
           ((uint32_t)bytes[1] << 8) |
           ((uint32_t)bytes[2] << 16) |
           ((uint32_t)bytes[3] << 24);
}

static int parse_byte_pair(const char *hex, uint8_t *out) {
    char pair[3] = {hex[0], hex[1], '\0'};
    char *end = NULL;
    errno = 0;
    unsigned long value = strtoul(pair, &end, 16);
    if (errno != 0 || end == pair || *end != '\0' || value > 0xffu) {
        return 0;
    }
    *out = (uint8_t)value;
    return 1;
}

int main(int argc, char **argv) {
    const char *path = argc > 1 ? argv[1] : "testdata/golden-packed-words.tsv";
    FILE *file = fopen(path, "rb");
    if (file == NULL) {
        perror(path);
        return 1;
    }

    char line[256];
    static const char header[] = "name\tdelta_t\tdelta_bid\tspread\tflag\tword_hex\tbytes_le_hex";
    if (fgets(line, sizeof line, file) == NULL || strncmp(line, header, strlen(header)) != 0) {
        fprintf(stderr, "invalid golden-vector header\n");
        fclose(file);
        return 1;
    }

    unsigned count = 0;
    while (fgets(line, sizeof line, file) != NULL) {
        char name[64], word_hex[16], bytes_hex[16];
        unsigned delta_t, spread, flag;
        int delta_bid;
        int fields = sscanf(line, "%63[^\t]\t%u\t%d\t%u\t%u\t%15[^\t]\t%15s",
                            name, &delta_t, &delta_bid, &spread, &flag, word_hex, bytes_hex);
        if (fields != 7 || delta_t > UINT16_MAX || delta_bid < INT8_MIN ||
            delta_bid > INT8_MAX || spread > UINT8_MAX || flag > UINT8_MAX ||
            strlen(bytes_hex) != 8) {
            fprintf(stderr, "invalid vector line: %s", line);
            fclose(file);
            return 1;
        }

        char *end = NULL;
        errno = 0;
        unsigned long expected_long = strtoul(word_hex, &end, 16);
        if (errno != 0 || end == word_hex || *end != '\0' || expected_long > UINT32_MAX) {
            fprintf(stderr, "invalid word for %s\n", name);
            fclose(file);
            return 1;
        }
        uint32_t expected = (uint32_t)expected_long;
        uint32_t packed = pack_tick((uint16_t)delta_t, (int8_t)delta_bid,
                                    (uint8_t)spread, (uint8_t)flag);
        if (packed != expected) {
            fprintf(stderr, "%s: packed %08" PRIX32 ", expected %08" PRIX32 "\n",
                    name, packed, expected);
            fclose(file);
            return 1;
        }

        uint8_t bytes[4];
        for (size_t i = 0; i < 4; ++i) {
            if (!parse_byte_pair(bytes_hex + i * 2, &bytes[i])) {
                fprintf(stderr, "invalid bytes for %s\n", name);
                fclose(file);
                return 1;
            }
        }
        if (read_le32(bytes) != expected) {
            fprintf(stderr, "%s: little-endian bytes do not match word\n", name);
            fclose(file);
            return 1;
        }
		uint8_t decoded_bid_raw = (uint8_t)((packed >> 16) & 0xffu);
		int decoded_bid = decoded_bid_raw >= 128u ? (int)decoded_bid_raw - 256 : decoded_bid_raw;
		if ((uint16_t)(packed & 0xffffu) != (uint16_t)delta_t ||
			decoded_bid != delta_bid ||
            (uint8_t)((packed >> 24) & 0x0fu) != (uint8_t)spread ||
            (uint8_t)((packed >> 28) & 0x0fu) != (uint8_t)flag) {
            fprintf(stderr, "%s: decoded fields do not match\n", name);
            fclose(file);
            return 1;
        }
        ++count;
    }

    if (ferror(file) || fclose(file) != 0 || count == 0) {
        fprintf(stderr, "failed while reading golden vectors\n");
        return 1;
    }
	printf("C interop validated %u MDC packed-word golden vectors.\n", count);
    return 0;
}
