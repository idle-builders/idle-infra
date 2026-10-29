#!/usr/bin/env bash
set -euo pipefail
[ "$#" -eq 1 ] || { echo 'usage: init-instance.sh /path/to/new/deployment' >&2; exit 2; }
framework="$(cd "$(dirname "$0")/.." && pwd)"
target="$1"
[ ! -e "$target" ] || { echo 'Target already exists; refusing to overwrite it.' >&2; exit 1; }
mkdir -p "$target"
git -C "$target" init -b main
git -C "$target" submodule add https://github.com/idle-builders/idle-infra.git framework
# Always initialize against the exact framework release executing this command.
revision="$(git -C "$framework" rev-parse HEAD)"
git -C "$target/framework" checkout --detach "$revision"
mkdir -p "$target/terraform/main"
cp "$framework"/examples/infomaniak/*.tf "$target/terraform/main/"
cp "$framework/examples/infomaniak/.terraform.lock.hcl" "$target/terraform/main/"
python3 - "$target" <<'PYCODE'
import json,pathlib,sys
root=pathlib.Path(sys.argv[1])
for p in (root/'terraform/main').glob('*.tf'):
    p.write_text(p.read_text().replace('../../terraform/modules/','../../framework/terraform/modules/').replace('/../../docker/','/../../framework/docker/'))
(root/'.infra-instance.json').write_text(json.dumps({'name':root.name,'repository':'','terraform_root':'terraform/main','framework':'framework','credentials_script':''},indent=2)+'\n')
PYCODE
cp "$framework/.gitignore" "$target/.gitignore"
echo "Created deployment at $target. Supply your variables and credentials; follow framework/README.md before applying."
