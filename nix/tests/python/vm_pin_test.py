"""Unit tests for vm_pin's topology parsing, run at build time before any VM boots."""

import unittest

import vm_pin


def cpuinfo(rows):
    return "\n\n".join(f"processor\t: {cpu}\nphysical id\t: {pkg}\ncore id\t\t: {core}\nflags\t\t: fpu" for cpu, pkg, core in rows)


# Two sockets, two cores each, hyperthreads numbered after all cores (the dev host's layout).
TWO_SOCKETS = cpuinfo([(0, 0, 0), (1, 1, 0), (2, 0, 1), (3, 1, 1), (4, 0, 0), (5, 1, 0), (6, 0, 1), (7, 1, 1)])


class CoresTest(unittest.TestCase):
    def test_groups_hyperthread_siblings_by_socket_then_core(self):
        self.assertEqual(vm_pin.cores_from_cpuinfo(TWO_SOCKETS, set(range(8))), [[0, 4], [2, 6], [1, 5], [3, 7]])

    def test_drops_a_core_when_any_sibling_is_not_allowed(self):
        self.assertEqual(vm_pin.cores_from_cpuinfo(TWO_SOCKETS, {0, 4, 2}), [[0, 4]])

    def test_without_topology_each_cpu_is_its_own_core(self):
        text = "processor\t: 0\n\nprocessor\t: 1"
        self.assertEqual(vm_pin.cores_from_cpuinfo(text, {0, 1}), [[0], [1]])


if __name__ == "__main__":
    unittest.main()
