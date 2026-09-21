"""Unit tests for vol_durability_rec, run at build time before any VM boots."""

import os
import socket
import tempfile
import threading
import unittest

import vol_durability_rec as rec


def write_records(path, seqs):
    with open(path, "wb") as f:
        for seq in seqs:
            f.seek(seq * rec.BLOCK)
            f.write(rec.record(seq))


class RecordTest(unittest.TestCase):
    def test_a_record_is_one_block_and_determined_by_its_sequence(self):
        self.assertEqual(len(rec.record(7)), rec.BLOCK)
        self.assertEqual(rec.record(7), rec.record(7))
        self.assertNotEqual(rec.record(7), rec.record(8))

    def test_records_a_full_cycle_apart_still_differ(self):
        self.assertNotEqual(rec.record(1), rec.record(1 + 251))

    def test_the_body_differs_between_neighbours_so_a_stale_tail_is_caught(self):
        self.assertNotEqual(rec.record(1)[64:], rec.record(2)[64:])


class VerifyTest(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        self.addCleanup(self.dir.cleanup)
        self.path = os.path.join(self.dir.name, "dev")

    def test_intact_records_verify_clean(self):
        write_records(self.path, range(50))
        self.assertEqual(rec.mismatched(self.path, 50, direct=False), [])

    def test_a_missing_record_is_reported(self):
        write_records(self.path, [0, 1, 3, 4])
        self.assertEqual(rec.mismatched(self.path, 5, direct=False), [2])

    def test_a_corrupted_record_is_reported(self):
        write_records(self.path, range(10))
        with open(self.path, "r+b") as f:
            f.seek(4 * rec.BLOCK + 100)
            f.write(b"\xff")
        self.assertEqual(rec.mismatched(self.path, 10, direct=False), [4])

    def test_a_record_written_at_the_wrong_offset_is_reported(self):
        write_records(self.path, [0, 1, 2])
        with open(self.path, "r+b") as f:
            f.seek(1 * rec.BLOCK)
            f.write(rec.record(2))
        self.assertEqual(rec.mismatched(self.path, 3, direct=False), [1])

    def test_a_short_device_reports_the_records_it_cannot_hold(self):
        write_records(self.path, range(3))
        self.assertEqual(rec.mismatched(self.path, 5, direct=False), [3, 4])

    def test_the_report_is_bounded(self):
        open(self.path, "wb").close()
        self.assertEqual(len(rec.mismatched(self.path, 100, direct=False, limit=10)), 10)

    def test_records_spanning_several_read_chunks_verify(self):
        count = 3 * rec.CHUNK // rec.BLOCK + 5
        write_records(self.path, range(count))
        self.assertEqual(rec.mismatched(self.path, count, direct=False), [])


class DigestTest(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        self.addCleanup(self.dir.cleanup)

    def path(self, name, seqs):
        p = os.path.join(self.dir.name, name)
        write_records(p, seqs)
        return p

    def digest(self, path, blocks):
        return rec.digest(path, blocks * rec.BLOCK, direct=False)

    def test_equal_content_has_equal_digest(self):
        self.assertEqual(self.digest(self.path("a", range(8)), 8), self.digest(self.path("b", range(8)), 8))

    def test_one_differing_block_changes_the_digest(self):
        a, b = self.path("a", range(8)), self.path("b", [0, 1, 2, 3, 4, 5, 6, 9])
        self.assertNotEqual(self.digest(a, 8), self.digest(b, 8))

    def test_only_the_requested_prefix_is_hashed(self):
        a, b = self.path("a", range(8)), self.path("b", [*range(4), 20, 21, 22, 23])
        self.assertEqual(self.digest(a, 4), self.digest(b, 4))


class LedgerTest(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        self.addCleanup(self.dir.cleanup)
        self.path = os.path.join(self.dir.name, "ledger")

    def serve(self):
        server = rec.make_ledger("127.0.0.1", 0, self.path)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        return server.server_address[1]

    def test_an_empty_or_missing_ledger_has_no_acks(self):
        self.assertEqual(rec.highest(self.path), -1)
        open(self.path, "w").close()
        self.assertEqual(rec.highest(self.path), -1)

    def test_the_ledger_stores_each_sequence_before_replying_ok(self):
        port = self.serve()
        with socket.create_connection(("127.0.0.1", port)) as s:
            f = s.makefile("rw")
            for seq in (0, 1, 2):
                f.write(f"{seq}\n")
                f.flush()
                self.assertEqual(f.readline().strip(), "ok")
                self.assertEqual(rec.highest(self.path), seq)

    def test_a_new_ledger_can_take_the_port_while_a_dead_writer_connection_lingers(self):
        first = rec.make_ledger("127.0.0.1", 0, self.path)
        port = first.server_address[1]
        threading.Thread(target=first.serve_forever, daemon=True).start()
        client = socket.create_connection(("127.0.0.1", port))
        self.addCleanup(client.close)
        client.sendall(b"0\n")
        self.assertEqual(client.makefile().readline().strip(), "ok")
        first.shutdown()
        first.server_close()
        second = rec.make_ledger("127.0.0.1", port, self.path)
        second.server_close()

    def test_highest_is_the_largest_sequence_not_the_last_line(self):
        with open(self.path, "w") as f:
            f.write("3\n9\n4\n")
        self.assertEqual(rec.highest(self.path), 9)

    def test_a_torn_final_line_is_ignored(self):
        with open(self.path, "w") as f:
            f.write("3\n4\n1")
        self.assertEqual(rec.highest(self.path), 4)


class WriterTest(unittest.TestCase):
    def test_the_writer_acks_only_records_that_are_on_the_device(self):
        with tempfile.TemporaryDirectory() as d:
            dev, ledger = os.path.join(d, "dev"), os.path.join(d, "ledger")
            open(dev, "wb").close()
            server = rec.make_ledger("127.0.0.1", 0, ledger)
            threading.Thread(target=server.serve_forever, daemon=True).start()
            try:
                rec.write_loop(dev, ("127.0.0.1", server.server_address[1]), start=5, limit=25, pause=0)
            finally:
                server.shutdown()
                server.server_close()
            self.assertEqual(rec.highest(ledger), 24)
            self.assertEqual(rec.mismatched(dev, 25, direct=False), list(range(5)))


if __name__ == "__main__":
    unittest.main()
