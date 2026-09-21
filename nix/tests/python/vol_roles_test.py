"""Unit tests for vol_roles, run at build time before any VM boots."""

import unittest

import vol_roles as roles

RES = "vol-0123456789abcdef"


def line(ts, verb, role, res=RES):
    return f"{ts} {verb} resource name:{res} role:{role} suspended:no force-io-failures:no may-promote:no promotion_score:0"


class ParseTest(unittest.TestCase):
    def test_reads_the_role_changes_of_one_resource_with_their_times(self):
        text = "\n".join(
            [
                line("2026-01-01T00:00:01.500000+0000", "exists", "Secondary"),
                line("2026-01-01T00:00:03.250000+0000", "change", "Primary"),
                line("2026-01-01T00:00:09.000000+0000", "change", "Secondary"),
            ]
        )
        got = roles.parse(text, RES)
        self.assertEqual([r for _, r in got], ["Secondary", "Primary", "Secondary"])
        self.assertAlmostEqual(got[1][0] - got[0][0], 1.75)

    def test_ignores_other_resources_and_other_objects(self):
        text = "\n".join(
            [
                line("2026-01-01T00:00:01.000000+0000", "change", "Primary", res="vol-other"),
                f"2026-01-01T00:00:02.000000+0000 change connection name:{RES} peer-node-id:1 conn-name:n2 connection:Connecting role:Unknown",
                f"2026-01-01T00:00:03.000000+0000 change device name:{RES} volume:0 minor:1 disk:UpToDate",
                "exists -",
                "",
            ]
        )
        self.assertEqual(roles.parse(text, RES), [])

    def test_a_timezone_offset_moves_the_instant(self):
        a = roles.parse(line("2026-01-01T02:00:00.000000+0200", "change", "Primary"), RES)[0][0]
        b = roles.parse(line("2026-01-01T00:00:00.000000+0000", "change", "Primary"), RES)[0][0]
        self.assertEqual(a, b)


class IntervalsTest(unittest.TestCase):
    def test_a_primary_stretch_runs_from_promotion_to_demotion(self):
        events = [(1.0, "Secondary"), (3.0, "Primary"), (9.0, "Secondary")]
        self.assertEqual(roles.primary_intervals(events, end=20.0), [(3.0, 9.0)])

    def test_a_role_still_held_at_the_end_is_closed_at_the_end(self):
        self.assertEqual(roles.primary_intervals([(3.0, "Primary")], end=20.0), [(3.0, 20.0)])

    def test_repeated_primary_events_do_not_split_a_stretch(self):
        events = [(3.0, "Primary"), (4.0, "Primary"), (9.0, "Secondary")]
        self.assertEqual(roles.primary_intervals(events, end=20.0), [(3.0, 9.0)])

    def test_two_stretches_stay_apart(self):
        events = [(1.0, "Primary"), (2.0, "Secondary"), (5.0, "Primary"), (6.0, "Secondary")]
        self.assertEqual(roles.primary_intervals(events, end=20.0), [(1.0, 2.0), (5.0, 6.0)])

    def test_no_events_means_never_primary(self):
        self.assertEqual(roles.primary_intervals([], end=20.0), [])


class OverlapTest(unittest.TestCase):
    def test_disjoint_stretches_on_two_nodes_do_not_overlap(self):
        held = {"n1": [(1.0, 5.0)], "n2": [(6.0, 9.0)]}
        self.assertEqual(roles.overlaps(held, tolerance=0.0), [])

    def test_a_handoff_with_a_gap_is_safe_and_reports_its_margin(self):
        held = {"n1": [(1.0, 5.0)], "n2": [(5.5, 9.0)]}
        self.assertAlmostEqual(roles.smallest_gap(held), 0.5)

    def test_overlapping_stretches_are_reported_with_their_length(self):
        held = {"n1": [(1.0, 6.0)], "n2": [(5.0, 9.0)], "n3": []}
        found = roles.overlaps(held, tolerance=0.0)
        self.assertEqual(len(found), 1)
        self.assertEqual({found[0].first, found[0].second}, {"n1", "n2"})
        self.assertAlmostEqual(found[0].seconds, 1.0)

    def test_an_overlap_inside_the_tolerance_is_forgiven(self):
        held = {"n1": [(1.0, 5.02)], "n2": [(5.0, 9.0)]}
        self.assertEqual(roles.overlaps(held, tolerance=0.05), [])
        self.assertEqual(len(roles.overlaps(held, tolerance=0.01)), 1)

    def test_stretches_that_only_touch_are_not_an_overlap(self):
        held = {"n1": [(1.0, 5.0)], "n2": [(5.0, 9.0)]}
        self.assertEqual(roles.overlaps(held, tolerance=0.0), [])

    def test_a_node_never_overlaps_itself(self):
        self.assertEqual(roles.overlaps({"n1": [(1.0, 5.0), (5.0, 6.0)]}, tolerance=0.0), [])

    def test_three_way_overlap_reports_every_pair(self):
        held = {"n1": [(0.0, 10.0)], "n2": [(2.0, 8.0)], "n3": [(4.0, 6.0)]}
        self.assertEqual(len(roles.overlaps(held, tolerance=0.0)), 3)

    def test_smallest_gap_is_negative_when_stretches_overlap(self):
        held = {"n1": [(1.0, 6.0)], "n2": [(5.0, 9.0)]}
        self.assertAlmostEqual(roles.smallest_gap(held), -1.0)

    def test_smallest_gap_is_the_tightest_of_several_handoffs(self):
        held = {"n1": [(1.0, 5.0), (30.0, 31.0)], "n2": [(9.0, 12.0)], "n3": [(12.2, 20.0)]}
        self.assertAlmostEqual(roles.smallest_gap(held), 0.2)

    def test_smallest_gap_is_none_without_a_handoff(self):
        self.assertIsNone(roles.smallest_gap({"n1": [(1.0, 5.0)], "n2": []}))


class HandoffTest(unittest.TestCase):
    def test_counts_changes_of_node_in_time_order(self):
        held = {"n1": [(1.0, 5.0), (20.0, 25.0)], "n2": [(6.0, 9.0)], "n3": [(10.0, 15.0)]}
        self.assertEqual(roles.handoffs(held), 3)

    def test_the_same_node_promoted_twice_is_not_a_handoff(self):
        self.assertEqual(roles.handoffs({"n1": [(1.0, 2.0), (3.0, 4.0)]}), 0)


class OffsetTest(unittest.TestCase):
    def test_the_probe_with_the_smallest_round_trip_wins(self):
        # (driver time before, node time, driver time after)
        probes = [(100.0, 105.0, 100.5), (200.0, 205.01, 200.02), (300.0, 306.0, 301.0)]
        self.assertAlmostEqual(roles.clock_offset(probes), 5.0, places=6)

    def test_a_node_behind_the_driver_has_a_negative_offset(self):
        self.assertAlmostEqual(roles.clock_offset([(10.0, 7.0, 10.0)]), -3.0)


if __name__ == "__main__":
    unittest.main()
