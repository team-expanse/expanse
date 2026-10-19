# Dumps Docker's and Pando's state to the serial console four times, two minutes apart (pando-guest.nix).
for i in 1 2 3 4; do
  echo "pando-debug $i: docker=$(systemctl is-active docker) pando=$(systemctl is-active pando)"
  docker ps -a --format '{{.Names}} {{.Status}}' 2>&1 | sed 's/^/pando-debug ps: /'
  journalctl -u pando -b --no-pager -o cat 2>&1 | tail -15 | sed 's/^/pando-debug unit: /'
  docker logs --tail 5 pando-pando-1 2>&1 | cut -c1-200 | sed 's/^/pando-debug log: /'
  sleep 120
done
