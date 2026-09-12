#!/usr/bin/env bash
# expanse install — shell wrapper for environments without the Go binary.
# The canonical installer is `expanse install`; this script exists so the
# ISO works even if invoked from a rescue shell.
set -euo pipefail

if command -v expanse >/dev/null 2>&1; then
  exec expanse install "$@"
fi

echo "expanse binary not found on PATH" >&2
exit 1
