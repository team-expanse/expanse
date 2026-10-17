#!/usr/bin/env bash
# Write throughput of one target (block device or file), with vol-perf's profiles.
# Usage: disk-bench.sh <device-or-file> [file-size, default 4G]. Overwrites the target.
set -euo pipefail

target=${1:?usage: disk-bench.sh <device-or-file> [file-size]}
size=${2:-4G}
runtime=30

sizearg=()
[[ -b $target ]] || sizearg=(--size="$size")

# run <name> <fio args...>: prints the JSON result of one fio job.
run() {
  local name=$1
  shift
  fio --name="$name" --filename="$target" "${sizearg[@]}" --time_based --runtime="$runtime" \
    --ramp_time=3 --output-format=json "$@"
}

summarize() {
  python3 -c '
import json, sys
name, unit = sys.argv[1], sys.argv[2]
w = json.load(sys.stdin)["jobs"][0]["write"]
bw, iops = w["bw_bytes"] / 1e6, w["iops"]
if unit == "MBps":
    print(f"{name}: {bw:.0f} MB/s")
elif unit == "iops":
    print(f"{name}: {iops:.0f} IOPS")
else:
    print(f"{name}: {iops:.0f} fsyncs/s")
' "$@"
}

echo "target $target ($(date -Is))"
run seqwrite --direct=1 --ioengine=libaio --rw=write --bs=1M --iodepth=32 | summarize "seq write 1M qd32" MBps
run randwrite --direct=1 --ioengine=libaio --rw=randwrite --bs=4k --iodepth=64 | summarize "rand write 4k qd64" iops
run fsync --ioengine=psync --rw=write --bs=4k --fsync=1 | summarize "4k write+fsync" fsync
[[ -b $target ]] || rm -f "$target"
