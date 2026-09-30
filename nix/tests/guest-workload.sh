# Guest workload for cluster-vm-workloads.nix: address from the kernel command line, a counter on the
# replicated disk made durable before it is served, and busybox httpd serving it on port 80.
exec 2>/dev/ttyS0  # the serial console reaches the host's log, so a failed start says where
set -eux
arg() { tr ' ' '\n' < /proc/cmdline | grep "^workload\.$1=" | cut -d= -f2; }
name=$(arg name)
until ip link show eth0 >/dev/null 2>&1; do sleep 1; done  # a starved guest may start this before udev adds the NIC
ip addr add "$(arg ip)/24" dev eth0
ip link set eth0 up

until [ -b /dev/vda ]; do sleep 1; done
blkid -t TYPE=ext4 /dev/vda >/dev/null || mkfs.ext4 -q -F /dev/vda
mkdir -p /srv
mount /dev/vda /srv
boot=$(( $(cat /srv/boots 2>/dev/null || echo 0) + 1 ))
echo "$boot" > /srv/boots
seq=$(cat /srv/seq 2>/dev/null || echo 0)
sync

busybox httpd -p 80 -h /srv
set +x  # the loop runs every second; keep the console quiet
while :; do
  seq=$((seq + 1))
  echo "$seq" > /srv/seq
  # Durable before visible: the watcher only ever sees a page that survives a crash.
  echo "name=$name boot=$boot seq=$seq" > /srv/index.tmp
  sync
  mv /srv/index.tmp /srv/index.html
  sync
  sleep 1
done
