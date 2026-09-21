"""Unit tests for vol_perf_lib (run by the vol-perf lint derivation)."""

import json
import unittest

import vol_perf_lib as lib

FIO_WRITE = json.dumps(
    {
        "jobs": [
            {
                "write": {"iops": 2500.5, "bw_bytes": 2621440000},
                "read": {"iops": 0, "bw_bytes": 0},
                "sync": {"lat_ns": {"percentile": {"99.000000": 850000}}},
            }
        ]
    }
)

FIO_READ = json.dumps(
    {"jobs": [{"read": {"iops": 9000, "bw_bytes": 5000}, "write": {"iops": 0, "bw_bytes": 0}}]}
)

BUDGETS = [
    {"name": "vol_seqwrite_ratio", "min": 0.75},
    {"name": "vol_fsync_p99_us", "max": 20000},
]


class ParseFio(unittest.TestCase):
    def test_write_job_reports_write_side(self):
        r = lib.parse_fio(FIO_WRITE, "write")
        self.assertEqual(r.iops, 2500.5)
        self.assertEqual(r.bw_bytes, 2621440000)

    def test_read_job_reports_read_side(self):
        self.assertEqual(lib.parse_fio(FIO_READ, "read").iops, 9000)

    def test_sync_p99_is_converted_to_microseconds(self):
        self.assertEqual(lib.parse_fio(FIO_WRITE, "write").sync_p99_us, 850)

    def test_missing_sync_section_is_none(self):
        self.assertIsNone(lib.parse_fio(FIO_READ, "read").sync_p99_us)

    def test_fio_notes_before_the_json_are_skipped(self):
        noisy = "fio: set debug option\n" + FIO_READ
        self.assertEqual(lib.parse_fio(noisy, "read").iops, 9000)

    def test_output_without_json_is_an_error(self):
        with self.assertRaises(ValueError):
            lib.parse_fio("fio: file not found", "read")


class Ratio(unittest.TestCase):
    def test_ratio_is_vol_over_local(self):
        self.assertAlmostEqual(lib.ratio(75.0, 100.0), 0.75)

    def test_zero_local_is_an_error_not_infinity(self):
        with self.assertRaises(ValueError):
            lib.ratio(1.0, 0.0)


class ProfileRatios(unittest.TestCase):
    @staticmethod
    def results(bw, iops):
        return {
            p: lib.FioResult(iops=iops[p], bw_bytes=bw[p], sync_p99_us=None)
            for p in ("seqwrite", "seqread", "randwrite", "randread")
        }

    def test_sequential_profiles_compare_bandwidth_random_compare_iops(self):
        local = self.results(
            bw={"seqwrite": 100, "seqread": 200, "randwrite": 1, "randread": 1},
            iops={"seqwrite": 1, "seqread": 1, "randwrite": 1000, "randread": 4000},
        )
        remote = self.results(
            bw={"seqwrite": 50, "seqread": 190, "randwrite": 999, "randread": 999},
            iops={"seqwrite": 999, "seqread": 999, "randwrite": 500, "randread": 2000},
        )
        got = lib.profile_ratios(local, remote)
        self.assertEqual(
            got,
            {
                "vol_seqwrite_ratio": 0.5,
                "vol_seqread_ratio": 0.95,
                "vol_randwrite_ratio": 0.5,
                "vol_randread_ratio": 0.5,
            },
        )


class FsyncAllowance(unittest.TestCase):
    def test_allowance_is_twice_local_plus_one_rtt(self):
        self.assertEqual(lib.fsync_allowance_us(400, 1500), 2300)


class CheckBudgets(unittest.TestCase):
    def test_floor_met_and_ceiling_met_is_clean(self):
        got = {"vol_seqwrite_ratio": 0.8, "vol_fsync_p99_us": 900}
        self.assertEqual(lib.check_budgets(got, BUDGETS), [])

    def test_below_floor_is_reported_with_name_and_values(self):
        got = {"vol_seqwrite_ratio": 0.5}
        msgs = lib.check_budgets(got, BUDGETS)
        self.assertEqual(len(msgs), 1)
        self.assertIn("vol_seqwrite_ratio", msgs[0])
        self.assertIn("0.5", msgs[0])
        self.assertIn("0.75", msgs[0])

    def test_above_ceiling_is_reported(self):
        msgs = lib.check_budgets({"vol_fsync_p99_us": 30000}, BUDGETS)
        self.assertEqual(len(msgs), 1)
        self.assertIn("vol_fsync_p99_us", msgs[0])

    def test_budget_without_a_measurement_is_ignored(self):
        self.assertEqual(lib.check_budgets({}, BUDGETS), [])

    def test_measurement_without_a_budget_is_an_error(self):
        with self.assertRaises(KeyError):
            lib.check_budgets({"vol_typo_ratio": 1.0}, BUDGETS)


class BudgetLookup(unittest.TestCase):
    def test_returns_the_named_budget(self):
        self.assertEqual(lib.budget(BUDGETS, "vol_fsync_p99_us")["max"], 20000)

    def test_unknown_budget_is_an_error(self):
        with self.assertRaises(KeyError):
            lib.budget(BUDGETS, "nope")


class Rate(unittest.TestCase):
    def test_mib_per_second(self):
        self.assertAlmostEqual(lib.mib_per_s(256 * 1024 * 1024, 8.0), 32.0)

    def test_zero_elapsed_is_an_error(self):
        with self.assertRaises(ValueError):
            lib.mib_per_s(1, 0)


class RttParse(unittest.TestCase):
    PING = (
        "20 packets transmitted, 20 received, 0% packet loss, time 19027ms\n"
        "rtt min/avg/max/mdev = 0.310/0.542/1.870/0.310 ms\n"
    )

    def test_max_rtt_in_microseconds(self):
        self.assertEqual(lib.ping_max_rtt_us(self.PING), 1870)

    def test_output_without_summary_is_an_error(self):
        with self.assertRaises(ValueError):
            lib.ping_max_rtt_us("ping: connect: Network is unreachable")


if __name__ == "__main__":
    unittest.main()
