# Idle Infra

This repository is the reusable framework. A deployment is a different repository
containing `.infra-instance.json`; never use the plugin checkout as a cluster.

- Public documentation, examples, screenshots and fixtures must be self-contained.
  Never identify a private deployment or refer readers to its repositories or runbooks.
  Use fictional application names and reserved example domains.
- Keep state backends, production IDs, tenant inventory and credentials out of
  framework defaults. Examples use operator inputs and reserved example domains.
- Preserve namespace, schema, network-policy and RBAC boundaries when extracting
  or upgrading modules. Record address migrations in the consuming deployment.
- Run `tests/validate.sh` and the affected runtime tests. Report cloud/bootstrap,
  local integration and live deployment results separately.
- Production changes go through deployment code, reviewed plans and serialized
  applies. Never print credentials, raw state, secret data or kubeconfig contents.
- Pin deployment framework commits and runtime image digests. Publishing a build
  never authorizes an apply to a consuming cluster.
