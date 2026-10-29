# Infomaniak deployment root

Use `scripts/init-instance.sh` from the framework root to create a separate
repository pinned to this framework revision. The scaffolder rewrites local
module paths and keeps credentials and state in the new deployment.

The example validates without cloud credentials. Its bootstrap stages are in the
framework README. A domain, actual API endpoint, first administrator, credentials
and selected runtime image digests are operator inputs, never Ambrovia defaults.

KEDA is disabled by default. When enabling it, provide values restricted to the
intended tenant and declare the scaler destination network policies in the
instance. Identity organizations and client registrations are likewise instance
resources; the framework deploys the shared Zitadel service.
