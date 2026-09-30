# Poll each workload every half second; one line per attempt: epoch-ms name ok|fail body.
# usage: watcher-poll.sh NAME=IP...
set -u
while :; do
  for target in "$@"; do
    name=${target%%=*}
    body=$(curl -s --max-time 1 "http://${target#*=}/" 2>/dev/null | tr -d '\n')
    case $body in *seq=*) state=ok ;; *) state=fail ;; esac
    echo "$(date +%s%3N) $name $state $body"
  done
  sleep 0.5
done
