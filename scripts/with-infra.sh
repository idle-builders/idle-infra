#!/usr/bin/env bash
# Keep the parent shell alive so credential-loader EXIT traps remove derived files.
set -euo pipefail
[ "$#" -gt 0 ] || { echo 'usage: with-infra.sh command [args...]' >&2; exit 2; }
here="$(cd "$(dirname "$0")" && pwd)"
instance="$("$here/infra-root.sh")"
cd "$instance"
loader="$(python3 -c 'import json; print(json.load(open(".infra-instance.json")).get("credentials_script", ""))')"
if [ -n "$loader" ]; then
  # The deployment owns its credential source; CI may use ambient environment instead.
  source "$instance/$loader" >/dev/null
fi
"$@"
