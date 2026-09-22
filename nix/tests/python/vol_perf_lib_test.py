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


class PassMetric(unittest.TestCase):
    RESULT = lib.FioResult(iops=1000, bw_bytes=4096000, sync_p99_us=None)

    def test_sequential_pass_is_judged_by_bandwidth(self):
        self.assertEqual(lib.pass_metric("seq", self.RESULT), 4096000)

    def test_random_pass_is_judged_by_iops(self):
        self.assertEqual(lib.pass_metric("rand", self.RESULT), 1000)

    def test_unknown_kind_is_an_error(self):
        with self.assertRaises(KeyError):
            lib.pass_metric("zigzag", self.RESULT)


class ThinAxis(unittest.TestCase):
    CELLS = {"local-thin-cold": 40.0, "local-thin-warm": 80.0, "local-thick": 100.0, "vol-cold": 30.0, "vol-warm": 60.0}

    def test_each_ratio_names_what_it_compares(self):
        self.assertEqual(
            lib.thin_axis(self.CELLS),
            {
                "local_first_touch": 0.5,
                "local_thin_vs_thick": 0.8,
                "vol_first_touch": 0.5,
                "vol_cold_vs_local_thick": 0.3,
            },
        )

    def test_a_missing_cell_is_an_error(self):
        with self.assertRaises(KeyError):
            lib.thin_axis({"local-thin-cold": 1.0})

    def test_a_zero_baseline_is_an_error(self):
        with self.assertRaises(ValueError):
            lib.thin_axis(dict(self.CELLS, **{"local-thick": 0.0}))


class VmstatMean(unittest.TestCase):
    TEXT = (
        "procs -----------memory---------- ---swap-- -----io---- -system-- ------cpu-----\n"
        " r  b   swpd   free   buff  cache   si   so    bi    bo   in   cs us sy id wa st\n"
        " 9  0      0 100000  1000  50000    0    0     1     2  100  200  1  1 98  0  0\n"
        " 1  0      0 100000  1000  50000    0    0     0    10  100  200 20 30 40 10  0\n"
        " 1  0      0 100000  1000  50000    0    0     0    10  100  200 40 50 10  0  0\n"
    )

    def test_the_first_sample_since_boot_is_dropped_and_the_rest_averaged(self):
        self.assertEqual(lib.vmstat_mean(self.TEXT), {"us": 30.0, "sy": 40.0, "id": 25.0, "wa": 5.0, "st": 0.0})


    def test_columns_are_found_by_name_so_a_trailing_guest_column_does_not_shift_them(self):
        text = (
            "procs -----------memory---------- ---swap-- -----io---- -system-- -------cpu-------\n"
            " r  b   swpd   free   buff  cache   si   so    bi    bo   in   cs us sy id wa st gu\n"
            " 9  0      0 100000  1000  50000    0    0     1     2  100  200  1  1 98  0  0  0\n"
            " 1  0      0 100000  1000  50000    0    0     0    10  100  200 20 30 40 10  0  0\n"
        )
        self.assertEqual(lib.vmstat_mean(text), {"us": 20.0, "sy": 30.0, "id": 40.0, "wa": 10.0, "st": 0.0})

    def test_output_without_samples_is_an_error(self):
        with self.assertRaises(ValueError):
            lib.vmstat_mean("procs -----memory-----\n r  b us sy id wa st\n")


class Fanout(unittest.TestCase):
    IPERF = json.dumps({"end": {"sum_received": {"bits_per_second": 419430400.0}}})

    def test_received_rate_in_mib_per_second(self):
        self.assertAlmostEqual(lib.iperf_mib_s(self.IPERF), 50.0)

    def test_a_failed_run_is_an_error(self):
        with self.assertRaises(ValueError):
            lib.iperf_mib_s(json.dumps({"error": "unable to connect to server"}))


class Enforce(unittest.TestCase):
    WAIVING = [
        {"name": "vol_seqwrite_ratio", "min": 0.75, "vm_waiver": "the harness is CPU-bound"},
        {"name": "vol_seqread_ratio", "min": 0.95},
    ]

    def test_a_violated_budget_without_a_waiver_is_a_problem(self):
        problems, waived = lib.enforce({"vol_seqread_ratio": 0.5}, self.WAIVING)
        self.assertEqual(len(problems), 1)
        self.assertEqual(waived, [])

    def test_a_violated_budget_with_a_waiver_is_reported_with_its_reason_and_is_not_a_problem(self):
        problems, waived = lib.enforce({"vol_seqwrite_ratio": 0.04}, self.WAIVING)
        self.assertEqual(problems, [])
        self.assertEqual(len(waived), 1)
        self.assertIn("vol_seqwrite_ratio", waived[0])
        self.assertIn("the harness is CPU-bound", waived[0])

    def test_a_met_waived_budget_is_neither(self):
        self.assertEqual(lib.enforce({"vol_seqwrite_ratio": 0.9}, self.WAIVING), ([], []))


if __name__ == "__main__":
    unittest.main()
