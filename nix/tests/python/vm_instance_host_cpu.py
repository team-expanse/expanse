"""vm-instance-host-cpu (Phase 13 A3): cpuModel host gives the guest vmx and a working /dev/kvm.

Two guests run side by side on a one-node cluster: "nested" sets cpuModel host, "plain" leaves it
unset. Each writes "CPU-REPORT vmx=N kvm=N END" to its raw disk once booted. An invalid cpuModel is
refused at apply time.

Runs after cluster-common.py, block-common.py, vol_cluster.py and single_node_common.py.
Expects KERNEL, INITRD and CMDLINE (vm-instance-host-cpu.nix) spliced in ahead of this file.
"""


def manifest(name, cpu_model=None):
    extra = f"\n    cpuModel: {cpu_model}" if cpu_model else ""
    return f"""apiVersion: expanse.io/v1
kind: Block
metadata:
  name: {name}
  namespace: default
spec:
  type: vm/instance
  replicas: 1
  strategy:
    kind: SINGLETON
  resources:
    requests:
      cpu: 1000m
      memory: 2Gi
  storage:
    - name: disk
      size: 16Mi
      mountPath: /mnt/{name}-disk
      filesystem: none
  placement:
    requiredCapabilities: [kvm]
  config:
    bootKernel: "{KERNEL}"
    bootInitrd: "{INITRD}"
    bootCmdline: "{CMDLINE}"{extra}
"""


def cpu_report(name):
    """The guest's report line from the head of its raw disk, or "" before it has written one."""
    res = volume_row(n1, f"blk-default-{name}-disk")["id"]
    dev = n1.succeed(f"drbdadm sh-dev {res}").strip()
    found = re.search(r"CPU-REPORT vmx=\d kvm=\d END", n1.succeed(f"head -c 64 {dev} | tr -d '\\0'"))
    return found.group(0) if found else ""


form_single("hostcpu")

with subtest("an unknown cpuModel is refused at apply time"):
    enc = base64.b64encode(manifest("bad", "max").encode()).decode()
    n1.succeed(f"echo {enc} | base64 -d > /tmp/bad.yaml")
    code, out = n1.execute(f"expanse ctl block apply {SOCK} -f /tmp/bad.yaml 2>&1")
    print(out)
    assert code != 0 and "cpuModel" in out, f"cpuModel max was accepted ({code}): {out}"

with subtest("both guests boot to RUNNING"):
    deploy(n1, "nested", manifest("nested", "host"))
    deploy(n1, "plain", manifest("plain"))
    for name in ("nested", "plain"):
        wait_phase(n1, name, ["RUNNING"], 600)

with subtest("cpuModel host reaches qemu's argv; unset leaves -cpu out"):
    for name, want in (("nested", True), ("plain", False)):
        pid = n1.succeed(f"systemctl show -p MainPID --value expanse-block-root@default-{name}-0.service").strip()
        argv = n1.succeed(f"ps --ppid {pid} -o args=")
        print(f"{name}: {argv}")
        assert ("-cpu host" in argv) == want, f"{name}: -cpu host present={not want}: {argv}"

with subtest("the nested guest sees vmx and /dev/kvm; the plain guest sees neither"):
    for name, want in (("nested", "CPU-REPORT vmx=1 kvm=1 END"), ("plain", "CPU-REPORT vmx=0 kvm=0 END")):
        wait_for(lambda: cpu_report(name) != "", f"{name} to write its CPU report", timeout=300)
        got = cpu_report(name)
        print(f"{name}: {got}")
        assert got == want, f"{name} reported {got!r}, want {want!r}"

print("VM-INSTANCE-HOST-CPU DONE")
