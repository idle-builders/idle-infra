#!/usr/bin/env bash
# Resolve a deployment, never the installed framework/plugin checkout.
set -euo pipefail
result="$(python3 - <<'PYCODE'
import json, os, pathlib, sys

def valid(path):
    p = pathlib.Path(path).expanduser().resolve()
    try:
        cfg = json.loads((p / '.infra-instance.json').read_text())
        root = (p / cfg['terraform_root']).resolve()
        return p if root.is_relative_to(p) and root.is_dir() else None
    except (OSError, KeyError, ValueError, TypeError):
        return None

explicit = os.environ.get('INFRA_REPO')
if explicit:
    result = valid(explicit)
    if not result:
        sys.exit('ERROR: INFRA_REPO is not a valid deployment; refusing fallback.')
    print(result)
    sys.exit(0)
for directory in [pathlib.Path.cwd(), *pathlib.Path.cwd().parents]:
    if (directory / '.infra-instance.json').exists():
        result = valid(directory)
        if not result:
            sys.exit('ERROR: invalid .infra-instance.json in current deployment.')
        print(result)
        sys.exit(0)
config = pathlib.Path(os.environ.get('XDG_CONFIG_HOME', str(pathlib.Path.home() / '.config')))
selection = config / 'idle-infra/default-instance'
try:
    result = valid(selection.read_text().strip())
except OSError:
    result = None
if not result:
    sys.exit('ERROR: no deployment selected. Run from a deployment, set INFRA_REPO, or run scripts/select-instance.sh /path/to/deployment. The plugin path is never a deployment.')
print(result)
PYCODE
)"
warn_if_not_main() {
  local branch
  branch="$(git -C "$result" symbolic-ref --short HEAD 2>/dev/null || true)"
  [ -z "$branch" ] || [ "$branch" = main ] || echo "NOTE: selected deployment is on $branch; source describes intent until applied." >&2
}
warn_if_not_main
printf '%s\n' "$result"
