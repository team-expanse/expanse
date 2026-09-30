# Guest workload for cluster-vm-workloads.nix: address from the kernel command line, a counter on the
# replicated disk made durable before it is served, and busybox httpd serving it on port 80.
set -eu
arg() { tr ' ' '\n' < /proc/cmdline | grep "^workload\.$1=" | cut -d= -f2; }
name=$(arg name)
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
while :; do
  seq=$((seq + 1))
  echo "$seq" > /srv/seq
  sync
  echo "name=$name boot=$boot seq=$seq" > /srv/index.html
  sleep 1
done
