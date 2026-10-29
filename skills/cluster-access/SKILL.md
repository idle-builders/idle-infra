---
name: cluster-access
description: Resolve the selected deployment and securely load or troubleshoot its Kubernetes, Helm and OpenTofu access.
---

# Cluster access

The framework/plugin directory and the live deployment are separate. Resolve the
deployment with `${CLAUDE_PLUGIN_ROOT}/scripts/infra-root.sh`. Selection uses an
explicit `INFRA_REPO`, the current deployment, or the default recorded by
`scripts/select-instance.sh`. Never infer cluster ownership from the plugin path.

Read `.infra-instance.json` in the deployment. `credentials_script` names its
optional environment loader; an empty value uses ambient credentials (for CI or
non-macOS installations). `terraform_root` names its OpenTofu root.

Run `${CLAUDE_PLUGIN_ROOT}/scripts/with-infra.sh kubectl get nodes` from another
repository. The wrapper loads credentials in its shell and runs cleanup traps
when the command exits. For an interactive shell, source the deployment's loader
in that shell; exports do not survive independent tool invocations.

Never print credentials, kubeconfig contents, tokens, client certificates or
private keys. Never persist a cluster-admin kubeconfig under the framework or
`~/.kube/config`, or use it for tenant CI. Tenant CI uses its namespace-scoped
deployer credential. Read the deployment's cluster-access runbook for its secret
store, restore and rotation procedures. Restore and rotation remain interactive;
do not bypass prompts or place secrets on a command line. Rotate only when
requested or compromise/expiry is established.
