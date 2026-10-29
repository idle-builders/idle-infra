#!/usr/bin/env bash
set -euo pipefail
[ "$#" -eq 1 ] || { echo 'usage: select-instance.sh /path/to/deployment' >&2; exit 2; }
here="$(cd "$(dirname "$0")" && pwd)"
instance="$(INFRA_REPO="$1" "$here/infra-root.sh")"
config="${XDG_CONFIG_HOME:-$HOME/.config}/idle-infra"
mkdir -p "$config"
printf '%s\n' "$instance" > "$config/default-instance"
echo "Selected deployment: $instance"
