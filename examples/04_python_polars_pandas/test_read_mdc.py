import csv
import struct
import tempfile
import unittest
from pathlib import Path

import numpy as np

from read_mdc import (
    crc32c,
    decode_packed_words,
    load_mdc_to_numpy,
    load_raw_words,
    mapped_raw_words,
)


class MDCInteropTests(unittest.TestCase):
    def test_crc32c_reference_vector(self):
        self.assertEqual(crc32c(b"123456789"), 0xE3069283)

    def test_shared_golden_vectors(self):
        fixture_path = Path(__file__).parents[2] / "testdata" / "golden-packed-words.tsv"
        with fixture_path.open(encoding="utf-8", newline="") as handle:
            vectors = list(csv.DictReader(handle, delimiter="\t"))
        self.assertGreater(len(vectors), 0)
        for vector in vectors:
            word = int(vector["word_hex"], 16)
            path_bytes = bytes.fromhex(vector["bytes_le_hex"])
            self.assertEqual(int.from_bytes(path_bytes, "little"), word)
            fields = decode_packed_words(np.array([word], dtype="<u4"))
            self.assertEqual(int(fields["delta_t"][0]), int(vector["delta_t"]))
            self.assertEqual(int(fields["delta_bid"][0]), int(vector["delta_bid"]))
            self.assertEqual(int(fields["spread"][0]), int(vector["spread"]))
            self.assertEqual(int(fields["flag"][0]), int(vector["flag"]))

    def test_exact_little_endian_layout(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "ticks.mdc"
            path.write_bytes(bytes.fromhex("19 00 05 21 ff ff 80 ff"))

            words = load_raw_words(path)
            np.testing.assert_array_equal(words, np.array([0x21050019, 0xFF80FFFF]))

            fields = decode_packed_words(words)
            np.testing.assert_array_equal(fields["delta_t"], [25, 65535])
            np.testing.assert_array_equal(fields["delta_bid"], [5, -128])
            np.testing.assert_array_equal(fields["spread"], [1, 15])
            np.testing.assert_array_equal(fields["flag"], [2, 15])

    def test_memory_map(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "ticks.mdc"
            path.write_bytes(bytes.fromhex("19 00 05 21"))
            with mapped_raw_words(path) as words:
                self.assertIsInstance(words, np.memmap)
                self.assertEqual(int(words[0]), 0x21050019)

    def test_rejects_truncated_tick(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "bad.mdc"
            path.write_bytes(b"\x01\x02\x03")
            with self.assertRaisesRegex(ValueError, "divisible by 4"):
                load_raw_words(path)

    def test_empty_file(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "empty.mdc"
            path.touch()
            fields = decode_packed_words(load_raw_words(path, memory_map=True))
            self.assertTrue(all(array.size == 0 for array in fields.values()))

    def test_canonical_container_golden_fixture(self):
        root = Path(__file__).parents[2]
        metadata, fields = load_mdc_to_numpy(root / "testdata" / "golden-container.bin")
        self.assertEqual(metadata.instrument, "WINFUT:B3")
        self.assertEqual(metadata.price_unit, "index-point")
        self.assertEqual(metadata.time_unit, 3)
        self.assertEqual(metadata.ordering, 0)
        self.assertEqual((metadata.tick_size_num, metadata.tick_size_den), (5, 1))
        np.testing.assert_array_equal(
            fields["timestamp"], [1000000, 1000010, 1066000, 1066010, 1065900]
        )
        np.testing.assert_array_equal(fields["bid_ticks"], [34910, 34911, 35050, 35049, 35048])
        np.testing.assert_array_equal(fields["spread"], [1, 31, 2, 4, 6])
        np.testing.assert_array_equal(fields["flags"], [2, 4660, 3, 5, 7])
        np.testing.assert_array_equal(
            fields["session"], [20260817, 20260817, 20260817, 20260818, 20260818]
        )
        np.testing.assert_array_equal(fields["tick_size_num"], [5, 5, 5, 5, 5])
        np.testing.assert_array_equal(fields["tick_size_den"], [1, 1, 1, 1, 2])

    def test_canonical_container_detects_corruption(self):
        root = Path(__file__).parents[2]
        raw = bytearray((root / "testdata" / "golden-container.bin").read_bytes())
        block = raw.index(b"MDBK")
        raw[block + 80] ^= 1
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "damaged.mdc"
            path.write_bytes(raw)
            with self.assertRaisesRegex(ValueError, "payload CRC32C"):
                load_mdc_to_numpy(path)

    def test_canonical_container_enforces_file_limit(self):
        root = Path(__file__).parents[2]
        with self.assertRaisesRegex(ValueError, "exceeds configured limit"):
            load_mdc_to_numpy(root / "testdata" / "golden-container.bin", max_file_bytes=100)

    def test_canonical_container_rejects_noncanonical_rational(self):
        root = Path(__file__).parents[2]
        raw = bytearray((root / "testdata" / "golden-container.bin").read_bytes())
        block = raw.index(b"MDBK")
        struct.pack_into("<qQ", raw, block + 48, 10, 2)
        header = bytearray(raw[block : block + 80])
        header[68:72] = b"\x00\x00\x00\x00"
        struct.pack_into("<I", raw, block + 68, crc32c(header))
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "noncanonical.mdc"
            path.write_bytes(raw)
            with self.assertRaisesRegex(ValueError, "block dimensions"):
                load_mdc_to_numpy(path)

    def test_canonical_container_checks_declared_tick_count(self):
        root = Path(__file__).parents[2]
        raw = bytearray((root / "testdata" / "golden-container.bin").read_bytes())
        struct.pack_into("<Q", raw, 56, 999)
        header_bytes = struct.unpack_from("<I", raw, 8)[0]
        header = bytearray(raw[:header_bytes])
        header[64:68] = b"\x00\x00\x00\x00"
        struct.pack_into("<I", raw, 64, crc32c(header))
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "wrong-count.mdc"
            path.write_bytes(raw)
            with self.assertRaisesRegex(ValueError, "declared tick count"):
                load_mdc_to_numpy(path)


if __name__ == "__main__":
    unittest.main()
