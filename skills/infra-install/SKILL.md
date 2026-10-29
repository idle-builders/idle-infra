---
name: infra-install
description: Install the reusable infrastructure skills and explicitly select a deployment to operate.
---

# Install infrastructure skills

Run `scripts/install-plugin.sh` from the idle-infra framework checkout to register
its plugin with Claude and Codex. `--status` inspects registration; `--uninstall`
removes it. Framework installation does not select or authenticate a cluster.

Run `scripts/select-instance.sh /absolute/path/to/deployment` to select a default.
A deployment contains `.infra-instance.json` and its own OpenTofu root. From inside
a deployment, that deployment takes priority over the default. `INFRA_REPO` is an
explicit override and fails closed when invalid.

Use `scripts/with-infra.sh kubectl get nodes` to verify access. It enters the
selected deployment and sources its optional credential loader for that command.
Never print credentials or kubeconfig contents. Test a different selection with
`INFRA_REPO=/path/to/other/deployment scripts/with-infra.sh kubectl get nodes`.
