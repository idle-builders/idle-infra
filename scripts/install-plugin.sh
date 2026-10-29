#!/usr/bin/env bash
# =============================================================================
# scripts/install-plugin.sh — Register the infra skills with Claude and Codex
# =============================================================================
#
# Makes the infra skills available from ANY repository on this device — acme,
# beacon, atlas — by registering this checkout as a local plugin
# marketplace in both agents' configuration.
#
# The registration also records where this repo lives on this device, which is
# how the skills resolve their file references when invoked from elsewhere
# (select it separately with scripts/select-instance.sh).
#
# Usage:
#   scripts/install-plugin.sh              # register
#   scripts/install-plugin.sh --status     # show what is registered
#   scripts/install-plugin.sh --uninstall  # remove the registration
#
# Idempotent: re-running updates the recorded path instead of duplicating it.
# Existing configuration for other plugins is preserved.
# =============================================================================

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
MARKETPLACE="idle-infra"
PLUGIN="infra"
QUALIFIED="${PLUGIN}@${MARKETPLACE}"

CLAUDE_SETTINGS="${HOME}/.claude/settings.json"
CODEX_CONFIG="${HOME}/.codex/config.toml"

MODE="install"
case "${1:-}" in
  --status)    MODE="status" ;;
  --uninstall) MODE="uninstall" ;;
  "")          ;;
  *) echo "Unknown argument: $1" >&2; exit 2 ;;
esac

# A worktree is a fine place to develop the skills from, but registering one
# points every repo on this device at a branch that may vanish.
if [ -f "${REPO_ROOT}/.git" ] && grep -q '^gitdir:.*worktrees' "${REPO_ROOT}/.git" 2>/dev/null; then
  echo "WARNING: ${REPO_ROOT} is a git worktree, not the main checkout." >&2
  echo "         Registering it pins the plugin to this branch. Prefer running" >&2
  echo "         this from the main checkout unless you are testing." >&2
  echo "" >&2
fi

# --- Claude: ~/.claude/settings.json -----------------------------------------
claude_apply() {
  python3 - "$CLAUDE_SETTINGS" "$MARKETPLACE" "$QUALIFIED" "$REPO_ROOT" "$MODE" <<'PY'
import json, os, sys

path, marketplace, qualified, repo_root, mode = sys.argv[1:6]
path = os.path.expanduser(path)
os.makedirs(os.path.dirname(path), exist_ok=True)

try:
    with open(path) as fh:
        data = json.load(fh)
except FileNotFoundError:
    data = {}
except json.JSONDecodeError:
    print(f"  Claude: {path} is not valid JSON — refusing to touch it")
    raise SystemExit(1)

markets = data.setdefault("extraKnownMarketplaces", {})
plugins = data.setdefault("enabledPlugins", {})

if mode == "status":
    entry = markets.get(marketplace)
    where = entry.get("source", {}).get("path") if entry else None
    print(f"  Claude: marketplace {'-> ' + where if where else 'not registered'}")
    print(f"  Claude: plugin {qualified} enabled={plugins.get(qualified, False)}")
    raise SystemExit(0)

if mode == "uninstall":
    markets.pop(marketplace, None)
    plugins.pop(qualified, None)
    if not markets:
        data.pop("extraKnownMarketplaces", None)
    if not plugins:
        data.pop("enabledPlugins", None)
    action = "removed"
else:
    markets[marketplace] = {"source": {"source": "directory", "path": repo_root}}
    plugins[qualified] = True
    action = "registered"

with open(path, "w") as fh:
    json.dump(data, fh, indent=2)
    fh.write("\n")
print(f"  Claude: {action} ({path})")
PY
}

# --- Codex: ~/.codex/config.toml ---------------------------------------------
codex_apply() {
  if [ ! -f "$CODEX_CONFIG" ] && [ "$MODE" != "install" ]; then
    echo "  Codex:  no config at ${CODEX_CONFIG} — nothing to do"
    return 0
  fi
  mkdir -p "$(dirname "$CODEX_CONFIG")"
  touch "$CODEX_CONFIG"

  python3 - "$CODEX_CONFIG" "$MARKETPLACE" "$QUALIFIED" "$REPO_ROOT" "$MODE" <<'PY'
import os, re, sys

path, marketplace, qualified, repo_root, mode = sys.argv[1:6]
text = open(path).read() if os.path.exists(path) else ""

mk_header = f"[marketplaces.{marketplace}]"
pl_header = f'[plugins."{qualified}"]'

def drop_section(body, header):
    # Remove the header and every line up to the next top-level table.
    pattern = re.compile(
        r"(?ms)^" + re.escape(header) + r"\s*$.*?(?=^\[|\Z)"
    )
    return pattern.sub("", body)

if mode == "status":
    m = re.search(
        r"(?ms)^" + re.escape(mk_header) + r"\s*$.*?^\s*source\s*=\s*\"([^\"]+)\"",
        text,
    )
    print(f"  Codex:  marketplace {'-> ' + m.group(1) if m else 'not registered'}")
    enabled = re.search(
        r"(?ms)^" + re.escape(pl_header) + r"\s*$.*?^\s*enabled\s*=\s*(\w+)", text
    )
    print(f"  Codex:  plugin {qualified} enabled={enabled.group(1) if enabled else False}")
    raise SystemExit(0)

text = drop_section(text, mk_header)
text = drop_section(text, pl_header)
text = text.rstrip("\n")

if mode == "install":
    block = (
        f"\n\n{mk_header}\n"
        f'source_type = "local"\n'
        f'source = "{repo_root}"\n'
        f"\n{pl_header}\n"
        f"enabled = true\n"
    )
    text = (text + block) if text else block.lstrip("\n")
    action = "registered"
else:
    text = text + "\n" if text else ""
    action = "removed"

open(path, "w").write(text)
print(f"  Codex:  {action} ({path})")
PY
}

case "$MODE" in
  status)
    echo "Infra skills plugin — status"
    echo "  Repo:   ${REPO_ROOT}"
    claude_apply || true
    codex_apply || true
    ;;
  uninstall)
    echo "Removing the infra skills plugin registration"
    claude_apply
    codex_apply
    echo ""
    echo "Done. Restart your agent session for the change to take effect."
    ;;
  install)
    echo "Registering the infra skills plugin"
    echo "  Repo:   ${REPO_ROOT}"
    claude_apply
    codex_apply
    echo ""
    echo "Done. Restart your agent session, then the skills are available from"
    echo "any repository on this device."
    echo ""
    echo "Verify with:"
    echo "  scripts/install-plugin.sh --status"
    echo "  scripts/infra-root.sh          # run from another repo"
    ;;
esac
