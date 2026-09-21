"""vol-lvm testScript body (Phase 1 A3).

Runs internal/storage/lvm's real-LVM integration test as root on two scratch
disks, so the wrapper's flags and output parsing are checked against real lvm2.
"""

start_all()
n1.wait_for_unit("multi-user.target")

n1.succeed("test -b /dev/vdb && test -b /dev/vdc")
out = n1.succeed(
    "LVM_TEST_PV=/dev/vdb LVM_TEST_PV2=/dev/vdc @lvmtest@/bin/lvm.test -test.v 2>&1"
)
print(out)
assert "--- PASS: TestRealLVM " in out, "integration test did not pass"
assert "FAIL" not in out, "integration test reported a failure"
