#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
tofu fmt -check -recursive terraform examples
python3 tests/instance-selection.py
python3 tests/validate-console-spec.py docs/api/console-v1.yaml
TF_VAR_state_encryption_passphrase=validation-only-passphrase tofu -chdir=examples/infomaniak init -backend=false -input=false -lockfile=readonly
TF_VAR_state_encryption_passphrase=validation-only-passphrase tofu -chdir=examples/infomaniak validate
python3 - <<'PYCODE'
import json,pathlib,yaml
root=pathlib.Path('.')
for p in root.glob('helm/values/*.yaml'): yaml.safe_load(p.read_text())
for p in root.glob('.*/plugin.json'):
    manifest=json.loads(p.read_text())
    assert (root/manifest['skills']).is_dir(),p
for p in root.glob('skills/*/SKILL.md'):
    assert 'name: '+p.parent.name in p.read_text(),p
for p in root.glob('.github/workflows/*.yml'):
    workflow=yaml.safe_load(p.read_text())
    assert 'jobs' in workflow,p
print('Framework values, skills, manifests and workflows validated.')
PYCODE
