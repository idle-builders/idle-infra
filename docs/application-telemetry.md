# Select application execution telemetry

Application execution summaries are opt-in. A deployment supplies exact source
identities through the observability module's `application_telemetry_sources`.
The compactor receives the list as `APPLICATION_TELEMETRY_SOURCES_JSON`; the
default is empty. This configures classification, not permission to send OTLP.
Collector network admission remains separately controlled by
`otlp_ingress_namespaces`.

```hcl
application_telemetry_sources = [{
  k8s_namespace      = "atlas"
  service_namespace = "atlas"
  service_name      = "atlas-api"
  workload          = "atlas-api"
}]
```

Each entry must have exactly one `workload` or `cronjob` selector. Workloads match
exactly. A CronJob selector matches only its name followed by `-` and an 8–10
digit timestamp, preserving the Kubernetes scheduled-job identity rule. Manual
jobs and arbitrary prefix matches are excluded. The application marker
`infra.telemetry.type=application` is still required. Existing payload bounds,
sanitization and execution sampling limits remain in effect.

An invalid source list stops compaction before processing. With an empty list,
application summaries are disabled and rows use the ordinary signal path. The
same parsed list controls application retention and exclusion from that ordinary
path, so a row is never dropped merely because the two classifiers disagree.

When upgrading from a runtime with a built-in workload list, explicitly declare
your reviewed source identities **before** changing the telemetry image digest.
Pin the framework and runtime together in the deployment. No application or
cluster is selected by framework defaults.
