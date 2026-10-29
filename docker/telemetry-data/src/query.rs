use std::collections::{BTreeMap, HashMap, HashSet};
use std::env;
use std::sync::Arc;

use anyhow::{Context, Result, bail, ensure};
use chrono::{DateTime, Duration, SecondsFormat, Timelike, Utc};
use futures::TryStreamExt;
use object_store::path::Path;
use object_store::{ObjectStore, ObjectStoreExt};
use serde::Serialize;
use serde_json::Value;

use crate::model::{Manifest, POLICY_VERSION, SCHEMA_VERSION};
use crate::parquet_output;

const SIGNALS: [&str; 3] = ["logs", "metrics", "traces"];
const MANIFEST_SIGNALS: [&str; 4] = ["logs", "metrics", "traces", "applications"];
const MAX_QUERY_WINDOW_HOURS: i64 = 48;
const MAX_QUERY_BYTES: usize = 64 * 1024 * 1024;
const MAX_PARQUET_BYTES: usize = 16 * 1024 * 1024;
const MAX_MANIFEST_BYTES: usize = 1024 * 1024;
const MAX_APPLICATION_SAMPLES_PER_QUERY: usize = 20;
// kubeletstats is sampled every 30 seconds. Three intervals tolerate a missed
// scrape without merging pods from an earlier rollout into the newest sample.
const WORKLOAD_SAMPLE_FRESHNESS_NS: i64 = 90 * 1_000_000_000;
// PostgreSQL is scraped every 60 seconds. Permit one missed scrape, but never
// attach an older gauge to an endpoint timestamp from a newer scrape.
const DATABASE_SAMPLE_FRESHNESS_NS: i64 = 90 * 1_000_000_000;
// The broker is scraped every 60 seconds, matching PostgreSQL's freshness
// allowance for one missed scrape.
const STORAGE_BROKER_SAMPLE_FRESHNESS_NS: i64 = 90 * 1_000_000_000;
const IDENTITY_SAMPLE_FRESHNESS_NS: i64 = 90 * 1_000_000_000;

#[derive(Clone, Debug)]
pub struct QueryRow {
    pub signal: String,
    pub timestamp_ns: i64,
    pub start_timestamp_ns: Option<i64>,
    pub end_timestamp_ns: Option<i64>,
    pub name: Option<String>,
    pub severity_number: Option<i32>,
    pub severity_text: Option<String>,
    pub trace_id: Option<String>,
    pub span_id: Option<String>,
    pub parent_span_id: Option<String>,
    pub status_code: Option<String>,
    pub service_name: Option<String>,
    pub service_namespace: Option<String>,
    pub k8s_namespace: Option<String>,
    pub workload: Option<String>,
    pub node: Option<String>,
    pub body_json: Option<String>,
    pub attributes_json: String,
    pub resource_json: String,
}

#[derive(Clone, Debug)]
pub struct QueryParams {
    pub start_ns: i64,
    pub end_ns: i64,
    pub namespace: Option<String>,
    pub workload: Option<String>,
    pub service: Option<String>,
    pub limit: usize,
}

impl QueryParams {
    pub fn from_env() -> Result<Self> {
        let end = match env::var("QUERY_TO") {
            Ok(value) if !value.is_empty() => parse_time("QUERY_TO", &value)?,
            _ => Utc::now(),
        };
        let start = match env::var("QUERY_FROM") {
            Ok(value) if !value.is_empty() => parse_time("QUERY_FROM", &value)?,
            _ => end - Duration::hours(1),
        };
        ensure!(start < end, "QUERY_FROM must be before QUERY_TO");
        ensure!(
            end - start <= Duration::hours(MAX_QUERY_WINDOW_HOURS),
            "query window exceeds {MAX_QUERY_WINDOW_HOURS} hours"
        );
        let limit = env::var("QUERY_LIMIT")
            .unwrap_or_else(|_| "100".to_owned())
            .parse::<usize>()
            .context("QUERY_LIMIT must be an integer")?;
        ensure!(
            (1..=100).contains(&limit),
            "QUERY_LIMIT must be between 1 and 100"
        );
        let namespace = optional_filter("QUERY_NAMESPACE")?;
        let workload = optional_filter("QUERY_WORKLOAD")?;
        let service = optional_filter("QUERY_SERVICE")?;
        Ok(Self {
            start_ns: nanos(start)?,
            end_ns: nanos(end)?,
            namespace,
            workload,
            service,
            limit,
        })
    }

    #[cfg(test)]
    fn for_test(start_ns: i64, end_ns: i64) -> Self {
        Self {
            start_ns,
            end_ns,
            namespace: None,
            workload: None,
            service: None,
            limit: 100,
        }
    }

    fn matches(&self, row: &QueryRow) -> bool {
        if !self.matches_without_service(row) {
            return false;
        }
        if let Some(service) = &self.service
            && row.service_name.as_deref() != Some(service.as_str())
        {
            return false;
        }
        true
    }

    fn matches_without_service(&self, row: &QueryRow) -> bool {
        if row.timestamp_ns < self.start_ns || row.timestamp_ns > self.end_ns {
            return false;
        }
        if let Some(namespace) = &self.namespace
            && row.k8s_namespace.as_deref() != Some(namespace.as_str())
        {
            return false;
        }
        if let Some(workload) = &self.workload
            && row.workload.as_deref() != Some(workload.as_str())
            && !row
                .body_json
                .as_deref()
                .is_some_and(|body| body.contains(workload))
        {
            return false;
        }
        true
    }
}

pub async fn run(store: Arc<dyn ObjectStore>, params: QueryParams) -> Result<Overview> {
    let mut manifest_paths = Vec::new();
    let mut seen_manifests = HashSet::new();
    let start = from_nanos(params.start_ns)?;
    let end = from_nanos(params.end_ns)?;
    let mut hour = start
        .with_minute(0)
        .and_then(|value| value.with_second(0))
        .and_then(|value| value.with_nanosecond(0))
        .context("round query start to hour")?;
    while hour <= end {
        for signal in MANIFEST_SIGNALS {
            let prefix = Path::from(format!(
                "manifests/v1/{signal}/date={}/hour={:02}",
                hour.format("%Y-%m-%d"),
                hour.hour()
            ));
            let objects = store
                .list(Some(&prefix))
                .try_collect::<Vec<_>>()
                .await
                .with_context(|| format!("list telemetry manifests under {prefix}"))?;
            for object in objects {
                if seen_manifests.insert(object.location.to_string()) {
                    manifest_paths.push(object.location);
                }
            }
        }
        hour += Duration::hours(1);
    }

    let mut outputs = Vec::new();
    let mut seen_outputs = HashSet::new();
    for path in manifest_paths {
        let bytes = store
            .get(&path)
            .await
            .with_context(|| format!("get telemetry manifest {path}"))?
            .bytes()
            .await
            .with_context(|| format!("read telemetry manifest {path}"))?;
        ensure!(
            bytes.len() <= MAX_MANIFEST_BYTES,
            "telemetry manifest exceeds 1 MiB: {path}"
        );
        let manifest: Manifest = serde_json::from_slice(&bytes)
            .with_context(|| format!("parse telemetry manifest {path}"))?;
        for output in manifest.outputs {
            let expected_prefix = format!("curated/v1/{}/", manifest.signal);
            ensure!(
                output.key.starts_with(&expected_prefix) && !output.key.contains(".."),
                "manifest contains invalid output key"
            );
            ensure!(
                output.bytes <= MAX_PARQUET_BYTES,
                "Parquet object exceeds 16 MiB"
            );
            if seen_outputs.insert(output.key.clone()) {
                outputs.push(output);
            }
        }
    }
    let scanned_bytes: usize = outputs.iter().map(|output| output.bytes).sum();
    ensure!(
        scanned_bytes <= MAX_QUERY_BYTES,
        "query would scan more than 64 MiB"
    );

    let mut builder = OverviewBuilder::new(params);
    for output in outputs {
        let path = Path::from(output.key.clone());
        let bytes = store
            .get(&path)
            .await
            .with_context(|| format!("get telemetry data {path}"))?
            .bytes()
            .await
            .with_context(|| format!("read telemetry data {path}"))?;
        ensure!(
            bytes.len() <= MAX_PARQUET_BYTES,
            "Parquet object exceeds 16 MiB"
        );
        parquet_output::visit_query_rows(&bytes, |row| builder.push(row))
            .with_context(|| format!("query telemetry data {path}"))?;
    }
    Ok(builder.finish())
}

fn parse_time(name: &str, value: &str) -> Result<DateTime<Utc>> {
    DateTime::parse_from_rfc3339(value)
        .map(|value| value.with_timezone(&Utc))
        .with_context(|| format!("{name} must be RFC3339"))
}

fn optional_filter(name: &str) -> Result<Option<String>> {
    let Ok(value) = env::var(name) else {
        return Ok(None);
    };
    if value.is_empty() {
        return Ok(None);
    }
    ensure!(value.len() <= 253, "{name} is too long");
    if !value
        .bytes()
        .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'.' | b'_' | b'-'))
    {
        bail!("{name} contains unsupported characters");
    }
    Ok(Some(value))
}

fn nanos(value: DateTime<Utc>) -> Result<i64> {
    value
        .timestamp_nanos_opt()
        .context("query timestamp is out of range")
}

fn from_nanos(value: i64) -> Result<DateTime<Utc>> {
    DateTime::<Utc>::from_timestamp(
        value.div_euclid(1_000_000_000),
        value.rem_euclid(1_000_000_000) as u32,
    )
    .context("query timestamp is out of range")
}

#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct Overview {
    pub schema_version: u32,
    pub requested_window: TimeWindow,
    pub data_through: Option<String>,
    pub fidelity: String,
    pub policy_version: String,
    pub coverage: Vec<SignalCoverage>,
    pub warnings: Vec<WarningEvent>,
    pub workloads: Vec<WorkloadSummary>,
    pub nodes: Vec<NodeSummary>,
    pub databases: Vec<DatabaseEndpointSummary>,
    pub storage_broker: Option<StorageBrokerSummary>,
    pub identity: Option<IdentitySummary>,
    pub applications: Vec<ApplicationServiceSummary>,
}

#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct ApplicationServiceSummary {
    pub namespace: String,
    pub service: String,
    pub last_seen: String,
    pub executions: u64,
    pub successful: u64,
    pub rejected: u64,
    pub failed: u64,
    pub p95_duration_seconds: Option<f64>,
    pub complete: bool,
    pub details_truncated: bool,
    pub operations: Vec<ApplicationOperationSummary>,
    pub samples: Vec<ApplicationExecutionSample>,
    pub standalone_logs: Vec<ApplicationLogSample>,
}

#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct ApplicationOperationSummary {
    pub operation: String,
    pub kind: String,
    pub executions: u64,
    pub successful: u64,
    pub rejected: u64,
    pub failed: u64,
    pub p95_duration_seconds: Option<f64>,
    pub sampled_executions: u64,
}

#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct ApplicationExecutionSample {
    pub trace_id: String,
    pub operation: String,
    pub outcome: String,
    pub reason: String,
    pub complete: bool,
    pub started_at: String,
    pub duration_seconds: Option<f64>,
    pub spans: Vec<ApplicationSpanSample>,
    pub logs: Vec<ApplicationLogSample>,
}

#[derive(Clone, Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct ApplicationSpanSample {
    pub service: String,
    pub span_id: String,
    pub parent_span_id: Option<String>,
    pub name: String,
    pub started_at: String,
    pub duration_seconds: Option<f64>,
    pub status: String,
    pub error_type: Option<String>,
    pub server_address: Option<String>,
}

#[derive(Clone, Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct ApplicationLogSample {
    pub timestamp: String,
    pub severity: String,
    pub message: String,
}

#[derive(Debug, Serialize)]
pub struct TimeWindow {
    pub from: String,
    pub to: String,
}

#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct SignalCoverage {
    pub signal: String,
    pub status: String,
    pub rows: usize,
    pub newest: Option<String>,
    pub note: Option<String>,
}

#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct WarningEvent {
    pub last_seen: String,
    pub namespace: String,
    pub reason: String,
    pub kind: String,
    pub resource: String,
    pub message: String,
    pub occurrences: usize,
}

#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct WorkloadSummary {
    pub namespace: String,
    pub workload: String,
    pub sampled_at: String,
    pub cpu_cores: Option<f64>,
    pub memory_working_set_bytes: Option<i64>,
    pub restarts: Option<i64>,
    pub ready_containers: Option<usize>,
    pub total_containers: Option<usize>,
    pub ready: Option<bool>,
}

#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct NodeSummary {
    pub node: String,
    pub sampled_at: String,
    pub ready: Option<bool>,
    pub cpu_cores: Option<f64>,
    pub memory_working_set_bytes: Option<i64>,
}

#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct DatabaseEndpointSummary {
    pub workload: String,
    pub role: String,
    pub observed_from: String,
    pub sampled_at: String,
    pub available: Option<bool>,
    pub connections: Option<i64>,
    pub max_connections: Option<i64>,
    pub size_bytes: Option<i64>,
    pub growth_bytes: Option<i64>,
    pub transactions_in_window: Option<i64>,
    pub rollbacks_in_window: Option<i64>,
    pub conflicts_in_window: Option<i64>,
    pub deadlocks_in_window: Option<i64>,
    pub temp_bytes_in_window: Option<i64>,
    pub replication_lag_seconds: Option<f64>,
}

#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct StorageBrokerSummary {
    pub observed_from: String,
    pub sampled_at: String,
    pub targets_observed_in_window: usize,
    pub targets_current: usize,
    pub targets_scrapeable: usize,
    pub requests_in_window: Option<i64>,
    pub successful_requests_in_window: Option<i64>,
    pub rejected_requests_in_window: Option<i64>,
    pub error_requests_in_window: Option<i64>,
    pub success_p95_latency_seconds: Option<f64>,
    pub success_p95_latency_overflow: Option<bool>,
}

#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct IdentitySummary {
    pub observed_from: String,
    pub sampled_at: String,
    pub targets_observed_in_window: usize,
    pub targets_current: usize,
    pub targets_scrapeable: usize,
    pub http_requests_in_window: Option<i64>,
    pub routes: Vec<IdentityRouteSummary>,
    pub request_p95_latency_seconds: Option<f64>,
    pub request_p95_latency_overflow: Option<bool>,
}

#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct IdentityRouteSummary {
    pub route: String,
    pub requests_in_window: Option<i64>,
    pub successful_requests_in_window: Option<i64>,
    pub rejected_requests_in_window: Option<i64>,
    pub error_requests_in_window: Option<i64>,
}

#[derive(Clone)]
struct MetricPoint {
    timestamp_ns: i64,
    identity: String,
    name: String,
    namespace: Option<String>,
    workload: Option<String>,
    node: Option<String>,
    value: f64,
    stable_workload: bool,
}

#[derive(Default)]
struct WorkloadAcc {
    sampled_ns: i64,
    cpu: Option<f64>,
    memory: Option<i64>,
    restarts: Option<i64>,
    ready: usize,
    ready_total: usize,
}

#[derive(Default)]
struct NodeAcc {
    sampled_ns: i64,
    ready: Option<bool>,
    cpu: Option<f64>,
    memory: Option<i64>,
}

#[derive(Clone)]
struct WarningAcc {
    last_seen_ns: i64,
    namespace: String,
    reason: String,
    kind: String,
    resource: String,
    message: String,
    occurrences: usize,
}

#[derive(Default)]
struct MetricSeries {
    points: BTreeMap<i64, f64>,
}

#[derive(Default)]
struct DatabaseAcc {
    observed_from_ns: Option<i64>,
    sampled_ns: i64,
    metrics: HashMap<String, MetricSeries>,
}

#[derive(Clone)]
struct HistogramPoint {
    bounds: Vec<f64>,
    bucket_counts: Vec<u64>,
}

#[derive(Default)]
struct HistogramSeries {
    points: BTreeMap<i64, HistogramPoint>,
}

#[derive(Default)]
struct StorageBrokerAcc {
    observed_from_ns: Option<i64>,
    sampled_ns: i64,
    replicas: HashMap<String, MetricSeries>,
    requests: HashMap<String, MetricSeries>,
    success_duration: HashMap<String, HistogramSeries>,
}

#[derive(Default)]
struct IdentityAcc {
    observed_from_ns: Option<i64>,
    sampled_ns: i64,
    replicas: HashMap<String, MetricSeries>,
    total_requests: HashMap<String, MetricSeries>,
    route_responses: HashMap<String, MetricSeries>,
    request_duration: HashMap<String, HistogramSeries>,
}

#[derive(Default)]
struct ApplicationOperationAcc {
    kind: String,
    executions: u64,
    successful: u64,
    rejected: u64,
    failed: u64,
    duration_bounds: Vec<f64>,
    duration_buckets: Vec<u64>,
    sampled_executions: u64,
}

#[derive(Default)]
struct ApplicationServiceAcc {
    last_seen_ns: i64,
    telemetry_loss_observed: bool,
    operations: BTreeMap<String, ApplicationOperationAcc>,
    sample_roots: BTreeMap<String, ApplicationRootAcc>,
    standalone_logs: Vec<ApplicationLogSample>,
}

#[derive(Default)]
struct ApplicationRootAcc {
    operation: String,
    outcome: String,
    started_ns: i64,
    duration_seconds: Option<f64>,
}

#[derive(Default)]
struct ApplicationBundleAcc {
    reason: Option<String>,
    complete: Option<bool>,
    spans: Vec<ApplicationSpanSample>,
    logs: Vec<ApplicationLogSample>,
    orphan_log_services: HashSet<(String, String)>,
}

#[cfg(test)]
fn build_overview(rows: impl IntoIterator<Item = QueryRow>, params: QueryParams) -> Overview {
    let mut builder = OverviewBuilder::new(params);
    for row in rows {
        builder.push(row);
    }
    builder.finish()
}

pub struct OverviewBuilder {
    params: QueryParams,
    counts: BTreeMap<String, usize>,
    newest: BTreeMap<String, Option<i64>>,
    data_through: Option<i64>,
    warnings: BTreeMap<String, WarningAcc>,
    latest_metrics: HashMap<String, MetricPoint>,
    databases: BTreeMap<String, DatabaseAcc>,
    storage_broker: StorageBrokerAcc,
    identity: IdentityAcc,
    applications: BTreeMap<(String, String), ApplicationServiceAcc>,
    application_bundles: BTreeMap<String, ApplicationBundleAcc>,
}

impl OverviewBuilder {
    pub fn new(params: QueryParams) -> Self {
        let mut counts = BTreeMap::new();
        let mut newest = BTreeMap::new();
        for signal in SIGNALS {
            counts.insert(signal.to_owned(), 0usize);
            newest.insert(signal.to_owned(), None);
        }
        Self {
            params,
            counts,
            newest,
            data_through: None,
            warnings: BTreeMap::new(),
            latest_metrics: HashMap::new(),
            databases: BTreeMap::new(),
            storage_broker: StorageBrokerAcc::default(),
            identity: IdentityAcc::default(),
            applications: BTreeMap::new(),
            application_bundles: BTreeMap::new(),
        }
    }

    pub fn push(&mut self, row: QueryRow) {
        let application_row = row.signal == "application_metrics" || is_application_sample(&row);
        if !self.params.matches(&row)
            && !(application_row && self.params.matches_without_service(&row))
        {
            return;
        }
        if let Some(count) = self.counts.get_mut(&row.signal) {
            *count += 1;
        }
        self.newest.entry(row.signal.clone()).and_modify(|value| {
            *value = Some(value.unwrap_or(row.timestamp_ns).max(row.timestamp_ns))
        });
        self.data_through = Some(
            self.data_through
                .unwrap_or(row.timestamp_ns)
                .max(row.timestamp_ns),
        );

        if application_row {
            collect_application(&row, &mut self.applications, &mut self.application_bundles);
        } else if row.signal == "logs" {
            collect_warning(&row, &mut self.warnings);
        } else if row.signal == "metrics" {
            collect_database_metric(&row, &mut self.databases);
            collect_storage_broker_metric(&row, &mut self.storage_broker);
            collect_identity_metric(&row, &mut self.identity);
            collect_metric(row, &mut self.latest_metrics);
        }
    }

    pub fn finish(self) -> Overview {
        let coverage = SIGNALS
            .iter()
            .map(|signal| {
                let rows = self.counts[*signal];
                let gap = rows == 0;
                SignalCoverage {
                    signal: (*signal).to_owned(),
                    status: if gap { "gap" } else { "ok" }.to_owned(),
                    rows,
                    newest: self.newest[*signal].map(timestamp),
                    note: gap.then(|| format!("no {signal} evidence in this window")),
                }
            })
            .collect();

        let mut warning_rows = self
            .warnings
            .into_values()
            .map(|event| WarningEvent {
                last_seen: timestamp(event.last_seen_ns),
                namespace: event.namespace,
                reason: event.reason,
                kind: event.kind,
                resource: event.resource,
                message: event.message,
                occurrences: event.occurrences,
            })
            .collect::<Vec<_>>();
        warning_rows.sort_by(|a, b| b.last_seen.cmp(&a.last_seen));
        warning_rows.truncate(self.params.limit);

        let (mut workloads, mut nodes) = summarize_metrics(self.latest_metrics);
        let databases = summarize_databases(self.databases);
        let storage_broker = summarize_storage_broker(self.storage_broker, self.newest["metrics"]);
        let identity = summarize_identity(self.identity, self.newest["metrics"]);
        let applications = summarize_applications(
            self.applications,
            self.application_bundles,
            self.params.service.as_deref(),
            self.params.limit,
        );
        workloads.sort_by(|a, b| {
            a.ready
                .unwrap_or(true)
                .cmp(&b.ready.unwrap_or(true))
                .then_with(|| b.restarts.unwrap_or(0).cmp(&a.restarts.unwrap_or(0)))
                .then_with(|| a.namespace.cmp(&b.namespace))
                .then_with(|| a.workload.cmp(&b.workload))
        });
        workloads.truncate(self.params.limit);
        nodes.sort_by(|a, b| a.node.cmp(&b.node));

        Overview {
            schema_version: SCHEMA_VERSION,
            requested_window: TimeWindow {
                from: timestamp(self.params.start_ns),
                to: timestamp(self.params.end_ns),
            },
            data_through: self.data_through.map(timestamp),
            fidelity: if applications.is_empty() {
                "complete"
            } else {
                "mixed"
            }
            .to_owned(),
            policy_version: POLICY_VERSION.to_owned(),
            coverage,
            warnings: warning_rows,
            workloads,
            nodes,
            databases,
            storage_broker,
            identity,
            applications,
        }
    }
}

fn is_application_sample(row: &QueryRow) -> bool {
    matches!(row.signal.as_str(), "logs" | "traces")
        && row.service_name.is_some()
        && row.service_namespace.is_some()
        && json_attributes(row)["telemetry.sample.reason"].is_string()
}

fn collect_application(
    row: &QueryRow,
    applications: &mut BTreeMap<(String, String), ApplicationServiceAcc>,
    bundles: &mut BTreeMap<String, ApplicationBundleAcc>,
) {
    let (Some(namespace), Some(service)) = (
        row.service_namespace.as_deref(),
        row.service_name.as_deref(),
    ) else {
        return;
    };
    let service_acc = applications
        .entry((namespace.to_owned(), service.to_owned()))
        .or_default();
    service_acc.last_seen_ns = service_acc.last_seen_ns.max(row.timestamp_ns);
    if row.signal == "application_metrics" {
        let Some(body) = row
            .body_json
            .as_deref()
            .and_then(|body| serde_json::from_str::<Value>(body).ok())
        else {
            return;
        };
        let Some(operation) = body["operation"].as_str() else {
            return;
        };
        let acc = service_acc
            .operations
            .entry(operation.to_owned())
            .or_default();
        acc.kind = body["operationKind"]
            .as_str()
            .unwrap_or("unknown")
            .to_owned();
        acc.executions += json_u64(&body["executions"]);
        acc.successful += json_u64(&body["successful"]);
        acc.rejected += json_u64(&body["rejected"]);
        acc.failed += json_u64(&body["failed"]);
        acc.sampled_executions += json_u64(&body["sampledExecutions"]);
        service_acc.telemetry_loss_observed |=
            body["telemetryLossObserved"].as_bool().unwrap_or(false);
        let bounds = json_f64_array(&body["durationBounds"]);
        let buckets = json_u64_array(&body["durationBucketCounts"]);
        if let (Some(bounds), Some(buckets)) = (bounds, buckets)
            && buckets.len() == bounds.len() + 1
        {
            if acc.duration_bounds.is_empty() {
                acc.duration_bounds = bounds.clone();
                acc.duration_buckets = vec![0; buckets.len()];
            }
            if acc.duration_bounds == bounds {
                for (total, value) in acc.duration_buckets.iter_mut().zip(buckets) {
                    *total += value;
                }
            }
        }
        return;
    }

    let attrs = json_attributes(row);
    let reason = attrs["telemetry.sample.reason"].as_str();
    service_acc.telemetry_loss_observed |=
        attrs["telemetry.sample.bundle_complete"].as_bool() == Some(false);
    if row.signal == "logs" && row.severity_number.unwrap_or(0) >= 17 && row.trace_id.is_none() {
        service_acc.standalone_logs.push(application_log(row));
        return;
    }
    let Some(trace_id) = row.trace_id.as_deref() else {
        return;
    };
    let bundle = bundles.entry(trace_id.to_owned()).or_default();
    if reason == Some("orphaned_error") {
        bundle
            .orphan_log_services
            .insert((namespace.to_owned(), service.to_owned()));
    }
    bundle.reason = attrs["telemetry.sample.reason"]
        .as_str()
        .map(ToOwned::to_owned)
        .or(bundle.reason.take());
    bundle.complete = Some(
        bundle.complete.unwrap_or(true)
            && attrs["telemetry.sample.bundle_complete"]
                .as_bool()
                .unwrap_or(false),
    );
    if row.signal == "traces" {
        let span_duration = match (row.start_timestamp_ns, row.end_timestamp_ns) {
            (Some(start), Some(end)) => Some((end - start).max(0) as f64 / 1_000_000_000.0),
            _ => None,
        };
        bundle.spans.push(ApplicationSpanSample {
            service: service.to_owned(),
            span_id: row.span_id.clone().unwrap_or_default(),
            parent_span_id: row.parent_span_id.clone(),
            name: row.name.clone().unwrap_or_else(|| "span".to_owned()),
            started_at: timestamp(row.start_timestamp_ns.unwrap_or(row.timestamp_ns)),
            duration_seconds: span_duration,
            status: if row.status_code.as_deref() == Some("2") {
                "error".to_owned()
            } else {
                "ok".to_owned()
            },
            error_type: attrs["error.type"].as_str().map(ToOwned::to_owned),
            server_address: attrs["server.address"].as_str().map(ToOwned::to_owned),
        });
        if attrs["infra.operation.kind"].is_string() {
            service_acc.sample_roots.insert(
                trace_id.to_owned(),
                ApplicationRootAcc {
                    operation: row.name.clone().unwrap_or_else(|| "operation".to_owned()),
                    outcome: attrs["infra.execution.outcome"]
                        .as_str()
                        .unwrap_or("unknown")
                        .to_owned(),
                    started_ns: row.start_timestamp_ns.unwrap_or(row.timestamp_ns),
                    duration_seconds: span_duration,
                },
            );
        }
    } else if row.signal == "logs" {
        bundle.logs.push(application_log(row));
    }
}

fn application_log(row: &QueryRow) -> ApplicationLogSample {
    ApplicationLogSample {
        timestamp: timestamp(row.timestamp_ns),
        severity: row
            .severity_text
            .clone()
            .unwrap_or_else(|| row.severity_number.unwrap_or(0).to_string()),
        message: row
            .body_json
            .as_deref()
            .and_then(|body| serde_json::from_str::<Value>(body).ok())
            .and_then(|body| body.as_str().map(ToOwned::to_owned))
            .unwrap_or_else(|| "log record".to_owned()),
    }
}

fn summarize_applications(
    mut applications: BTreeMap<(String, String), ApplicationServiceAcc>,
    mut bundles: BTreeMap<String, ApplicationBundleAcc>,
    service_filter: Option<&str>,
    limit: usize,
) -> Vec<ApplicationServiceSummary> {
    for (trace_id, bundle) in &bundles {
        let has_root = applications
            .values()
            .any(|application| application.sample_roots.contains_key(trace_id));
        if !has_root {
            for service in &bundle.orphan_log_services {
                if let Some(application) = applications.get_mut(service) {
                    application
                        .standalone_logs
                        .extend(bundle.logs.iter().cloned());
                    application.telemetry_loss_observed = true;
                }
            }
        }
    }
    let selected = applications
        .into_iter()
        .filter(|(_, acc)| {
            !acc.operations.is_empty()
                || !acc.sample_roots.is_empty()
                || !acc.standalone_logs.is_empty()
        })
        .filter(|((_, service), _)| service_filter.is_none_or(|filter| service == filter))
        .take(limit)
        .collect::<Vec<_>>();
    let service_count = selected.len();
    selected
        .into_iter()
        .enumerate()
        .map(|(index, ((namespace, service), acc))| {
            let operation_budget = fair_share(limit, index, service_count);
            let sample_budget = fair_share(
                limit.min(MAX_APPLICATION_SAMPLES_PER_QUERY),
                index,
                service_count,
            );
            let standalone_budget = fair_share(limit, index, service_count);
            let executions = acc.operations.values().map(|value| value.executions).sum();
            let successful = acc.operations.values().map(|value| value.successful).sum();
            let rejected = acc.operations.values().map(|value| value.rejected).sum();
            let failed = acc.operations.values().map(|value| value.failed).sum();
            let mut service_bounds = Vec::new();
            let mut service_buckets = Vec::new();
            for operation in acc.operations.values() {
                if service_bounds.is_empty() && !operation.duration_bounds.is_empty() {
                    service_bounds = operation.duration_bounds.clone();
                    service_buckets = vec![0; operation.duration_buckets.len()];
                }
                if service_bounds == operation.duration_bounds {
                    for (total, value) in service_buckets
                        .iter_mut()
                        .zip(operation.duration_buckets.iter())
                    {
                        *total += value;
                    }
                }
            }
            let p95_duration_seconds = histogram_quantile(&service_bounds, &service_buckets, 0.95);
            let mut operations = acc
                .operations
                .into_iter()
                .map(|(operation, value)| ApplicationOperationSummary {
                    operation,
                    kind: value.kind,
                    executions: value.executions,
                    successful: value.successful,
                    rejected: value.rejected,
                    failed: value.failed,
                    p95_duration_seconds: histogram_quantile(
                        &value.duration_bounds,
                        &value.duration_buckets,
                        0.95,
                    ),
                    sampled_executions: value.sampled_executions,
                })
                .collect::<Vec<_>>();
            operations.sort_by(|left, right| {
                right
                    .failed
                    .cmp(&left.failed)
                    .then_with(|| right.executions.cmp(&left.executions))
                    .then_with(|| left.operation.cmp(&right.operation))
            });
            let operations_truncated = operations.len() > operation_budget;
            operations.truncate(operation_budget);
            let mut samples = acc
                .sample_roots
                .into_iter()
                .map(|(trace_id, root)| {
                    let bundle = bundles.entry(trace_id.clone()).or_default();
                    let mut spans = bundle.spans.clone();
                    let mut logs = bundle.logs.clone();
                    spans.sort_by(|a, b| a.started_at.cmp(&b.started_at));
                    logs.sort_by(|a, b| a.timestamp.cmp(&b.timestamp));
                    ApplicationExecutionSample {
                        trace_id,
                        operation: root.operation,
                        outcome: root.outcome,
                        reason: bundle
                            .reason
                            .clone()
                            .unwrap_or_else(|| "sampled".to_owned()),
                        complete: bundle.complete.unwrap_or(false),
                        started_at: timestamp(root.started_ns),
                        duration_seconds: root.duration_seconds,
                        spans: { spans },
                        logs: { logs },
                    }
                })
                .collect::<Vec<_>>();
            samples.sort_by(|left, right| right.started_at.cmp(&left.started_at));
            let samples_truncated = samples.len() > sample_budget;
            samples.truncate(sample_budget);
            let mut standalone_logs = acc.standalone_logs;
            standalone_logs.sort_by(|left, right| right.timestamp.cmp(&left.timestamp));
            let standalone_truncated = standalone_logs.len() > standalone_budget;
            standalone_logs.truncate(standalone_budget);
            ApplicationServiceSummary {
                namespace,
                service,
                last_seen: timestamp(acc.last_seen_ns),
                executions,
                successful,
                rejected,
                failed,
                p95_duration_seconds,
                complete: !acc.telemetry_loss_observed,
                details_truncated: operations_truncated
                    || samples_truncated
                    || standalone_truncated,
                operations,
                samples,
                standalone_logs,
            }
        })
        .collect()
}

fn fair_share(total: usize, index: usize, recipients: usize) -> usize {
    if recipients == 0 {
        return 0;
    }
    total / recipients + usize::from(index < total % recipients)
}

fn histogram_quantile(bounds: &[f64], buckets: &[u64], quantile: f64) -> Option<f64> {
    let total: u64 = buckets.iter().sum();
    if total == 0 || buckets.len() != bounds.len() + 1 {
        return None;
    }
    let wanted = (total as f64 * quantile).ceil() as u64;
    let mut seen = 0;
    for (index, count) in buckets.iter().enumerate() {
        seen += count;
        if seen >= wanted {
            return bounds
                .get(index)
                .copied()
                .or_else(|| bounds.last().copied());
        }
    }
    None
}

fn json_attributes(row: &QueryRow) -> Value {
    serde_json::from_str(&row.attributes_json).unwrap_or(Value::Null)
}

fn json_u64(value: &Value) -> u64 {
    value
        .as_u64()
        .or_else(|| value.as_str().and_then(|value| value.parse().ok()))
        .unwrap_or(0)
}

fn collect_database_metric(row: &QueryRow, databases: &mut BTreeMap<String, DatabaseAcc>) {
    let Some(name) = row.name.as_deref() else {
        return;
    };
    if !matches!(
        name,
        "pg_up"
            | "pg_settings_max_connections"
            | "pg_stat_database_numbackends"
            | "pg_stat_database_xact_commit"
            | "pg_stat_database_xact_rollback"
            | "pg_stat_database_conflicts"
            | "pg_stat_database_deadlocks"
            | "pg_stat_database_temp_bytes"
            | "pg_database_size_bytes"
            | "pg_replication_is_replica"
            | "pg_replication_lag_seconds"
    ) {
        return;
    }
    let resource: Value = serde_json::from_str(&row.resource_json).unwrap_or(Value::Null);
    if resource["service.name"].as_str() != Some("postgresql-approved") {
        return;
    }
    let Some(workload) = row
        .workload
        .as_deref()
        .or_else(|| resource["k8s.statefulset.name"].as_str())
    else {
        return;
    };
    let Some(body) = row
        .body_json
        .as_deref()
        .and_then(|body| serde_json::from_str::<Value>(body).ok())
    else {
        return;
    };
    let Some(value) = number_value_json(&body) else {
        return;
    };
    let datname = metric_attribute(&body, "datname").unwrap_or_default();
    let key = format!("{name}\0{datname}");
    let database = databases.entry(workload.to_owned()).or_default();
    database.observed_from_ns = Some(
        database
            .observed_from_ns
            .unwrap_or(row.timestamp_ns)
            .min(row.timestamp_ns),
    );
    database.sampled_ns = database.sampled_ns.max(row.timestamp_ns);
    database
        .metrics
        .entry(key)
        .or_default()
        .points
        .insert(row.timestamp_ns, value);
}

fn summarize_databases(databases: BTreeMap<String, DatabaseAcc>) -> Vec<DatabaseEndpointSummary> {
    databases
        .into_iter()
        .map(|(workload, database)| {
            let fresh_after = database.sampled_ns - DATABASE_SAMPLE_FRESHNESS_NS;
            let latest = |name: &str| {
                database
                    .metrics
                    .get(&format!("{name}\0"))
                    .and_then(|series| latest_value(series, fresh_after))
            };
            let latest_sum = |name: &str| {
                sum_metric(&database.metrics, name, |series| {
                    latest_value(series, fresh_after)
                })
            };
            let counter_delta = |name: &str| {
                sum_metric(&database.metrics, name, |series| {
                    monotonic_delta(series, fresh_after)
                })
            };
            let commits = counter_delta("pg_stat_database_xact_commit");
            let rollbacks = counter_delta("pg_stat_database_xact_rollback");
            DatabaseEndpointSummary {
                role: match latest("pg_replication_is_replica") {
                    Some(value) if value != 0.0 => "replica",
                    Some(_) => "primary",
                    None => "unknown",
                }
                .to_owned(),
                workload,
                observed_from: timestamp(database.observed_from_ns.unwrap_or(database.sampled_ns)),
                sampled_at: timestamp(database.sampled_ns),
                available: latest("pg_up").map(|value| value != 0.0),
                connections: latest_sum("pg_stat_database_numbackends").map(round_i64),
                max_connections: latest("pg_settings_max_connections").map(round_i64),
                size_bytes: latest_sum("pg_database_size_bytes").map(round_i64),
                growth_bytes: sum_metric(&database.metrics, "pg_database_size_bytes", |series| {
                    signed_delta(series, fresh_after)
                })
                .map(round_i64),
                transactions_in_window: match (commits, rollbacks) {
                    (Some(commits), Some(rollbacks)) => Some(round_i64(commits + rollbacks)),
                    _ => None,
                },
                rollbacks_in_window: rollbacks.map(round_i64),
                conflicts_in_window: counter_delta("pg_stat_database_conflicts").map(round_i64),
                deadlocks_in_window: counter_delta("pg_stat_database_deadlocks").map(round_i64),
                temp_bytes_in_window: counter_delta("pg_stat_database_temp_bytes").map(round_i64),
                replication_lag_seconds: latest("pg_replication_lag_seconds"),
            }
        })
        .collect()
}

fn collect_storage_broker_metric(row: &QueryRow, broker: &mut StorageBrokerAcc) {
    let Some(name) = row.name.as_deref() else {
        return;
    };
    if !matches!(
        name,
        "up" | "storage_broker_requests_total" | "storage_broker_request_duration_seconds"
    ) {
        return;
    }
    let resource: Value = serde_json::from_str(&row.resource_json).unwrap_or(Value::Null);
    if resource["service.name"].as_str() != Some("storage-broker-approved") {
        return;
    }
    let Some(instance) = resource["service.instance.id"].as_str() else {
        return;
    };
    let Some(body) = row
        .body_json
        .as_deref()
        .and_then(|body| serde_json::from_str::<Value>(body).ok())
    else {
        return;
    };

    let accepted = match name {
        "up" => number_value_json(&body).map(|value| {
            broker
                .replicas
                .entry(instance.to_owned())
                .or_default()
                .points
                .insert(row.timestamp_ns, value);
        }),
        "storage_broker_requests_total" => {
            let operation = metric_attribute(&body, "operation");
            let outcome = metric_attribute(&body, "outcome");
            match (operation, outcome, number_value_json(&body)) {
                (Some(operation), Some(outcome), Some(value))
                    if broker_operation(&operation) && broker_outcome(&outcome) =>
                {
                    broker
                        .requests
                        .entry(format!("{instance}\0{operation}\0{outcome}"))
                        .or_default()
                        .points
                        .insert(row.timestamp_ns, value);
                    Some(())
                }
                _ => None,
            }
        }
        "storage_broker_request_duration_seconds" => {
            let operation = metric_attribute(&body, "operation");
            let outcome = metric_attribute(&body, "outcome");
            let bounds = json_f64_array(&body["explicitBounds"]);
            let bucket_counts = json_u64_array(&body["bucketCounts"]);
            match (operation, outcome, bounds, bucket_counts) {
                (Some(operation), Some(outcome), Some(bounds), Some(bucket_counts))
                    if broker_operation(&operation)
                        && outcome == "success"
                        && bucket_counts.len() == bounds.len() + 1
                        && body["count"]
                            .as_str()
                            .and_then(|value| value.parse::<u64>().ok())
                            .or_else(|| body["count"].as_u64())
                            == Some(bucket_counts.iter().sum()) =>
                {
                    broker
                        .success_duration
                        .entry(format!("{instance}\0{operation}"))
                        .or_default()
                        .points
                        .insert(
                            row.timestamp_ns,
                            HistogramPoint {
                                bounds,
                                bucket_counts,
                            },
                        );
                    Some(())
                }
                _ => None,
            }
        }
        _ => None,
    };

    if accepted.is_some() {
        broker.observed_from_ns = Some(
            broker
                .observed_from_ns
                .unwrap_or(row.timestamp_ns)
                .min(row.timestamp_ns),
        );
        broker.sampled_ns = broker.sampled_ns.max(row.timestamp_ns);
    }
}

fn summarize_storage_broker(
    broker: StorageBrokerAcc,
    newest_metric_ns: Option<i64>,
) -> Option<StorageBrokerSummary> {
    let observed_from_ns = broker.observed_from_ns?;
    // Compare broker samples with the newest metric evidence in the query, not
    // with the broker's own last sample. Otherwise a stopped scrape target
    // would remain current forever relative to itself.
    let fresh_after =
        newest_metric_ns.unwrap_or(broker.sampled_ns) - STORAGE_BROKER_SAMPLE_FRESHNESS_NS;
    let targets_observed_in_window = broker.replicas.len();
    let targets_current = broker
        .replicas
        .values()
        .filter(|series| latest_value(series, fresh_after).is_some())
        .count();
    let targets_scrapeable = broker
        .replicas
        .values()
        .filter(|series| latest_value(series, fresh_after).is_some_and(|value| value != 0.0))
        .count();
    let requests = |outcomes: &[&str]| {
        let matching = broker
            .requests
            .iter()
            .filter(|(key, _)| {
                outcomes.is_empty()
                    || key
                        .rsplit_once('\0')
                        .is_some_and(|(_, outcome)| outcomes.contains(&outcome))
            })
            .map(|(key, series)| target_counter_delta(key, series, &broker.replicas))
            .collect::<Option<Vec<_>>>()?;
        if matching.is_empty() && broker.requests.is_empty() {
            None
        } else {
            Some(matching.into_iter().sum::<f64>())
        }
    };

    let success_p95 = histogram_p95(&broker.success_duration, &broker.replicas);
    Some(StorageBrokerSummary {
        observed_from: timestamp(observed_from_ns),
        sampled_at: timestamp(broker.sampled_ns),
        targets_observed_in_window,
        targets_current,
        targets_scrapeable,
        requests_in_window: requests(&[]).map(round_i64),
        successful_requests_in_window: requests(&["success"]).map(round_i64),
        rejected_requests_in_window: requests(&[
            "invalid",
            "unauthenticated",
            "denied",
            "rejected",
        ])
        .map(round_i64),
        error_requests_in_window: requests(&["error"]).map(round_i64),
        success_p95_latency_seconds: success_p95.map(|(seconds, _)| seconds),
        success_p95_latency_overflow: success_p95.map(|(_, overflow)| overflow),
    })
}

fn target_counter_delta(
    key: &str,
    series: &MetricSeries,
    targets: &HashMap<String, MetricSeries>,
) -> Option<f64> {
    let (first_timestamp, first) = series.points.first_key_value()?;
    let (last_timestamp, last) = series.points.last_key_value()?;
    let instance = key
        .split_once('\0')
        .map(|(instance, _)| instance)
        .unwrap_or(key);
    // The broker omits zero-value label combinations. If this series appeared
    // after an earlier successful scrape of the same target, its true baseline
    // is zero and the first observed value belongs to this query window.
    let appeared_in_window = targets
        .get(instance)
        .and_then(|up| up.points.range(..*first_timestamp).next_back())
        .is_some_and(|(_, value)| *value != 0.0);
    if !appeared_in_window && last_timestamp == first_timestamp {
        return None;
    }
    let mut previous = None;
    for value in series.points.values() {
        if previous.is_some_and(|previous| *value < previous) {
            return None;
        }
        previous = Some(*value);
    }
    Some(last - if appeared_in_window { 0.0 } else { *first })
}

fn collect_identity_metric(row: &QueryRow, identity: &mut IdentityAcc) {
    let Some(name) = row.name.as_deref() else {
        return;
    };
    if !matches!(
        name,
        "up" | "http_server_request_count_total"
            | "http_server_return_code_counter_total"
            | "http_server_request_duration_seconds"
    ) {
        return;
    }
    let resource: Value = serde_json::from_str(&row.resource_json).unwrap_or(Value::Null);
    if resource["service.name"].as_str() != Some("zitadel-approved") {
        return;
    }
    let Some(instance) = resource["service.instance.id"].as_str() else {
        return;
    };
    let Some(body) = row
        .body_json
        .as_deref()
        .and_then(|body| serde_json::from_str::<Value>(body).ok())
    else {
        return;
    };

    let accepted = match name {
        "up" => number_value_json(&body).map(|value| {
            identity
                .replicas
                .entry(instance.to_owned())
                .or_default()
                .points
                .insert(row.timestamp_ns, value);
        }),
        "http_server_request_count_total" if metric_attribute(&body, "uri").is_none() => {
            number_value_json(&body).map(|value| {
                identity
                    .total_requests
                    .entry(instance.to_owned())
                    .or_default()
                    .points
                    .insert(row.timestamp_ns, value);
            })
        }
        "http_server_return_code_counter_total" => {
            let route = metric_attribute(&body, "uri");
            let method = metric_attribute(&body, "method").unwrap_or_default();
            let code = metric_attribute(&body, "return_code");
            match (route, code, number_value_json(&body)) {
                (Some(route), Some(code), Some(value))
                    if identity_route(&route) && code.parse::<u16>().is_ok() =>
                {
                    identity
                        .route_responses
                        .entry(format!("{instance}\0{route}\0{method}\0{code}"))
                        .or_default()
                        .points
                        .insert(row.timestamp_ns, value);
                    Some(())
                }
                _ => None,
            }
        }
        "http_server_request_duration_seconds" => {
            let bounds = json_f64_array(&body["explicitBounds"]);
            let bucket_counts = json_u64_array(&body["bucketCounts"]);
            match (bounds, bucket_counts) {
                (Some(bounds), Some(bucket_counts))
                    if bucket_counts.len() == bounds.len() + 1
                        && body["count"]
                            .as_str()
                            .and_then(|value| value.parse::<u64>().ok())
                            .or_else(|| body["count"].as_u64())
                            == Some(bucket_counts.iter().sum()) =>
                {
                    identity
                        .request_duration
                        .entry(instance.to_owned())
                        .or_default()
                        .points
                        .insert(
                            row.timestamp_ns,
                            HistogramPoint {
                                bounds,
                                bucket_counts,
                            },
                        );
                    Some(())
                }
                _ => None,
            }
        }
        _ => None,
    };

    if accepted.is_some() {
        identity.observed_from_ns = Some(
            identity
                .observed_from_ns
                .unwrap_or(row.timestamp_ns)
                .min(row.timestamp_ns),
        );
        identity.sampled_ns = identity.sampled_ns.max(row.timestamp_ns);
    }
}

fn summarize_identity(
    identity: IdentityAcc,
    newest_metric_ns: Option<i64>,
) -> Option<IdentitySummary> {
    let observed_from_ns = identity.observed_from_ns?;
    let fresh_after =
        newest_metric_ns.unwrap_or(identity.sampled_ns) - IDENTITY_SAMPLE_FRESHNESS_NS;
    let targets_observed_in_window = identity.replicas.len();
    let targets_current = identity
        .replicas
        .values()
        .filter(|series| latest_value(series, fresh_after).is_some())
        .count();
    let targets_scrapeable = identity
        .replicas
        .values()
        .filter(|series| latest_value(series, fresh_after).is_some_and(|value| value != 0.0))
        .count();
    let total_requests = identity
        .total_requests
        .iter()
        .map(|(key, series)| target_counter_delta(key, series, &identity.replicas))
        .collect::<Option<Vec<_>>>()
        .filter(|values| !values.is_empty())
        .map(|values| values.into_iter().sum::<f64>());

    let mut routes = Vec::new();
    for (route, label) in [
        ("/oauth/v2/authorize", "authorize"),
        ("/oauth/v2/token", "token"),
        ("/oidc/v1/userinfo", "userinfo"),
    ] {
        let matching = identity
            .route_responses
            .iter()
            .filter(|(key, _)| key.split('\0').nth(1) == Some(route))
            .collect::<Vec<_>>();
        if matching.is_empty() {
            continue;
        }
        let sum_codes = |wanted: fn(u16) -> bool| {
            matching
                .iter()
                .filter(|(key, _)| {
                    key.rsplit_once('\0')
                        .and_then(|(_, code)| code.parse::<u16>().ok())
                        .is_some_and(wanted)
                })
                .map(|(key, series)| target_counter_delta(key, series, &identity.replicas))
                .collect::<Option<Vec<_>>>()
                .map(|values| values.into_iter().sum::<f64>())
        };
        routes.push(IdentityRouteSummary {
            route: label.to_owned(),
            requests_in_window: sum_codes(|_| true).map(round_i64),
            successful_requests_in_window: sum_codes(|code| code < 400).map(round_i64),
            rejected_requests_in_window: sum_codes(|code| (400..500).contains(&code))
                .map(round_i64),
            error_requests_in_window: sum_codes(|code| code >= 500).map(round_i64),
        });
    }
    let p95 = histogram_p95(&identity.request_duration, &identity.replicas);
    Some(IdentitySummary {
        observed_from: timestamp(observed_from_ns),
        sampled_at: timestamp(identity.sampled_ns),
        targets_observed_in_window,
        targets_current,
        targets_scrapeable,
        http_requests_in_window: total_requests.map(round_i64),
        routes,
        request_p95_latency_seconds: p95.map(|(seconds, _)| seconds),
        request_p95_latency_overflow: p95.map(|(_, overflow)| overflow),
    })
}

fn identity_route(value: &str) -> bool {
    matches!(
        value,
        "/oauth/v2/authorize" | "/oauth/v2/token" | "/oidc/v1/userinfo"
    )
}

fn histogram_p95(
    histograms: &HashMap<String, HistogramSeries>,
    targets: &HashMap<String, MetricSeries>,
) -> Option<(f64, bool)> {
    let mut bounds: Option<Vec<f64>> = None;
    let mut aggregate = Vec::<u64>::new();
    for (key, series) in histograms {
        let (first_timestamp, first) = series.points.first_key_value()?;
        let (last_timestamp, last) = series.points.last_key_value()?;
        let instance = key
            .split_once('\0')
            .map(|(instance, _)| instance)
            .unwrap_or(key);
        let appeared_in_window = targets
            .get(instance)
            .and_then(|up| up.points.range(..*first_timestamp).next_back())
            .is_some_and(|(_, value)| *value != 0.0);
        if !appeared_in_window && last_timestamp == first_timestamp {
            return None;
        }
        let mut previous = None;
        for point in series.points.values() {
            if bounds
                .as_ref()
                .is_some_and(|bounds| bounds != &point.bounds)
                || first.bounds != point.bounds
                || first.bucket_counts.len() != point.bucket_counts.len()
                || previous.as_ref().is_some_and(|previous: &Vec<u64>| {
                    point
                        .bucket_counts
                        .iter()
                        .zip(previous)
                        .any(|(current, previous)| current < previous)
                })
            {
                return None;
            }
            previous = Some(point.bucket_counts.clone());
        }
        bounds.get_or_insert_with(|| last.bounds.clone());
        if aggregate.is_empty() {
            aggregate.resize(last.bucket_counts.len(), 0);
        }
        for (index, total) in aggregate.iter_mut().enumerate() {
            let baseline = if appeared_in_window {
                0
            } else {
                first.bucket_counts[index]
            };
            *total = total.checked_add(last.bucket_counts[index].checked_sub(baseline)?)?;
        }
    }
    let bounds = bounds?;
    let total: u64 = aggregate.iter().sum();
    if total == 0 {
        return None;
    }
    let wanted = total.saturating_mul(95).div_ceil(100);
    let mut seen = 0u64;
    for (index, count) in aggregate.into_iter().enumerate() {
        seen = seen.saturating_add(count);
        if seen >= wanted {
            return bounds
                .get(index)
                .copied()
                .map(|seconds| (seconds, false))
                .or_else(|| bounds.last().copied().map(|seconds| (seconds, true)));
        }
    }
    None
}

fn broker_operation(value: &str) -> bool {
    matches!(
        value,
        "presign"
            | "multipart_create"
            | "multipart_part"
            | "multipart_complete"
            | "multipart_abort"
    )
}

fn broker_outcome(value: &str) -> bool {
    matches!(
        value,
        "success" | "invalid" | "unauthenticated" | "denied" | "rejected" | "error"
    )
}

fn json_f64_array(value: &Value) -> Option<Vec<f64>> {
    value
        .as_array()?
        .iter()
        .map(Value::as_f64)
        .collect::<Option<Vec<_>>>()
}

fn json_u64_array(value: &Value) -> Option<Vec<u64>> {
    value
        .as_array()?
        .iter()
        .map(|value| {
            value
                .as_u64()
                .or_else(|| value.as_str().and_then(|value| value.parse().ok()))
        })
        .collect::<Option<Vec<_>>>()
}

fn sum_metric(
    metrics: &HashMap<String, MetricSeries>,
    name: &str,
    value: impl Fn(&MetricSeries) -> Option<f64>,
) -> Option<f64> {
    let prefix = format!("{name}\0");
    let values = metrics
        .iter()
        .filter(|(key, _)| key.starts_with(&prefix))
        .map(|(_, range)| value(range))
        .collect::<Option<Vec<_>>>()?;
    (!values.is_empty()).then(|| values.into_iter().sum())
}

fn latest_value(series: &MetricSeries, fresh_after: i64) -> Option<f64> {
    series
        .points
        .last_key_value()
        .filter(|(timestamp, _)| **timestamp >= fresh_after)
        .map(|(_, value)| *value)
}

fn signed_delta(series: &MetricSeries, fresh_after: i64) -> Option<f64> {
    let (first_timestamp, first) = series.points.first_key_value()?;
    let (last_timestamp, last) = series.points.last_key_value()?;
    (*last_timestamp >= fresh_after && last_timestamp > first_timestamp).then_some(last - first)
}

fn monotonic_delta(series: &MetricSeries, fresh_after: i64) -> Option<f64> {
    let delta = signed_delta(series, fresh_after)?;
    let mut previous = None;
    for value in series.points.values() {
        if previous.is_some_and(|previous| *value < previous) {
            return None;
        }
        previous = Some(*value);
    }
    Some(delta)
}

fn round_i64(value: f64) -> i64 {
    value.round() as i64
}

fn metric_attribute(body: &Value, key: &str) -> Option<String> {
    body["attributes"].as_array()?.iter().find_map(|attribute| {
        (attribute["key"].as_str() == Some(key))
            .then(|| {
                attribute["value"]["stringValue"]
                    .as_str()
                    .map(str::to_owned)
            })
            .flatten()
    })
}

fn collect_warning(row: &QueryRow, warnings: &mut BTreeMap<String, WarningAcc>) {
    let Some(body) = row.body_json.as_deref() else {
        return;
    };
    let Ok(body): Result<Value, _> = serde_json::from_str(body) else {
        return;
    };
    let object = &body["object"];
    if object["type"].as_str() != Some("Warning") {
        return;
    }
    let namespace = row.k8s_namespace.clone().unwrap_or_default();
    let reason = text(object, "reason");
    let kind = text(&object["regarding"], "kind");
    let resource = text(&object["regarding"], "name");
    let message = text(object, "note");
    let key = format!("{namespace}\0{reason}\0{kind}\0{resource}\0{message}");
    warnings
        .entry(key)
        .and_modify(|event| {
            event.last_seen_ns = event.last_seen_ns.max(row.timestamp_ns);
            event.occurrences += 1;
        })
        .or_insert(WarningAcc {
            last_seen_ns: row.timestamp_ns,
            namespace,
            reason,
            kind,
            resource,
            message,
            occurrences: 1,
        });
}

fn collect_metric(row: QueryRow, latest: &mut HashMap<String, MetricPoint>) {
    let Some(name) = row.name.as_deref() else {
        return;
    };
    if !matches!(
        name,
        "container.cpu.usage"
            | "container.memory.working_set"
            | "k8s.container.restarts"
            | "k8s.container.ready"
            | "k8s.node.condition_ready"
            | "k8s.node.cpu.usage"
            | "k8s.node.memory.working_set"
    ) {
        return;
    }
    let Some(value) = row.body_json.as_deref().and_then(number_value) else {
        return;
    };
    let resource: Value = serde_json::from_str(&row.resource_json).unwrap_or(Value::Null);
    // The current-service table is controller based. Completed Job pods remain
    // in kube-state metrics with ready=0 and would otherwise dominate it
    // forever. Jobs need their own work/duration view.
    let stable_workload = [
        "k8s.deployment.name",
        "k8s.statefulset.name",
        "k8s.daemonset.name",
    ]
    .iter()
    .find_map(|key| resource[*key].as_str())
    .map(str::to_owned);
    let has_stable_workload = stable_workload.is_some();
    let workload = stable_workload.or(row.workload);
    if !name.starts_with("k8s.node.")
        && (workload.is_none()
            || (matches!(name, "container.cpu.usage" | "container.memory.working_set")
                && !has_stable_workload))
    {
        return;
    }
    let identity = if name.starts_with("k8s.node.") {
        row.node.clone().unwrap_or_default()
    } else {
        format!(
            "{}:{}",
            text(&resource, "k8s.pod.uid"),
            text(&resource, "k8s.container.name")
        )
    };
    if identity.is_empty() || identity == ":" {
        return;
    }
    let key = format!("{name}\0{identity}");
    let point = MetricPoint {
        timestamp_ns: row.timestamp_ns,
        identity,
        name: name.to_owned(),
        namespace: row.k8s_namespace,
        workload,
        node: row.node,
        value,
        stable_workload: has_stable_workload,
    };
    if latest
        .get(&key)
        .is_none_or(|current| point.timestamp_ns > current.timestamp_ns)
    {
        latest.insert(key, point);
    }
}

fn summarize_metrics(
    points: HashMap<String, MetricPoint>,
) -> (Vec<WorkloadSummary>, Vec<NodeSummary>) {
    // Kube-state can retain terminated pods for most of the query window. Use
    // recent kubelet CPU/memory points to identify the containers that belong
    // to the workload's newest sample, then attach readiness/restarts only to
    // those identities. Otherwise old pods make a 1-replica controller appear
    // as 3/4 ready and inflate its resource totals.
    let mut newest_workload_sample: BTreeMap<(String, String), i64> = BTreeMap::new();
    for point in points.values().filter(|point| {
        point.stable_workload
            && matches!(
                point.name.as_str(),
                "container.cpu.usage" | "container.memory.working_set"
            )
    }) {
        let (Some(namespace), Some(workload)) = (&point.namespace, &point.workload) else {
            continue;
        };
        newest_workload_sample
            .entry((namespace.clone(), workload.clone()))
            .and_modify(|newest| *newest = (*newest).max(point.timestamp_ns))
            .or_insert(point.timestamp_ns);
    }
    let active_containers = points
        .values()
        .filter(|point| {
            point.stable_workload
                && matches!(
                    point.name.as_str(),
                    "container.cpu.usage" | "container.memory.working_set"
                )
        })
        .filter_map(|point| {
            let namespace = point.namespace.clone()?;
            let workload = point.workload.clone()?;
            let newest = newest_workload_sample.get(&(namespace.clone(), workload.clone()))?;
            (point.timestamp_ns >= *newest - WORKLOAD_SAMPLE_FRESHNESS_NS)
                .then(|| (namespace, workload, point.identity.clone()))
        })
        .collect::<HashSet<_>>();
    let mut workloads: BTreeMap<(String, String), WorkloadAcc> = BTreeMap::new();
    let mut nodes: BTreeMap<String, NodeAcc> = BTreeMap::new();
    for point in points.into_values() {
        if point.name.starts_with("k8s.node.") {
            let Some(node) = point.node else { continue };
            let out = nodes.entry(node).or_default();
            out.sampled_ns = out.sampled_ns.max(point.timestamp_ns);
            match point.name.as_str() {
                "k8s.node.condition_ready" => out.ready = Some(point.value != 0.0),
                "k8s.node.cpu.usage" => out.cpu = Some(point.value),
                "k8s.node.memory.working_set" => out.memory = Some(point.value as i64),
                _ => {}
            }
            continue;
        }
        let (Some(namespace), Some(workload)) = (point.namespace, point.workload) else {
            continue;
        };
        if !active_containers.contains(&(
            namespace.clone(),
            workload.clone(),
            point.identity.clone(),
        )) {
            continue;
        }
        let out = workloads.entry((namespace, workload)).or_default();
        out.sampled_ns = out.sampled_ns.max(point.timestamp_ns);
        match point.name.as_str() {
            "container.cpu.usage" => *out.cpu.get_or_insert(0.0) += point.value,
            "container.memory.working_set" => *out.memory.get_or_insert(0) += point.value as i64,
            "k8s.container.restarts" => *out.restarts.get_or_insert(0) += point.value as i64,
            "k8s.container.ready" => {
                out.ready_total += 1;
                if point.value != 0.0 {
                    out.ready += 1;
                }
            }
            _ => {}
        }
    }

    let workloads = workloads
        .into_iter()
        .map(|((namespace, workload), value)| WorkloadSummary {
            namespace,
            workload,
            sampled_at: timestamp(value.sampled_ns),
            cpu_cores: value.cpu,
            memory_working_set_bytes: value.memory,
            restarts: value.restarts,
            ready_containers: (value.ready_total > 0).then_some(value.ready),
            total_containers: (value.ready_total > 0).then_some(value.ready_total),
            ready: (value.ready_total > 0).then_some(value.ready == value.ready_total),
        })
        .collect();
    let nodes = nodes
        .into_iter()
        .map(|(node, value)| NodeSummary {
            node,
            sampled_at: timestamp(value.sampled_ns),
            ready: value.ready,
            cpu_cores: value.cpu,
            memory_working_set_bytes: value.memory,
        })
        .collect();
    (workloads, nodes)
}

fn number_value(body: &str) -> Option<f64> {
    let value: Value = serde_json::from_str(body).ok()?;
    number_value_json(&value)
}

fn number_value_json(value: &Value) -> Option<f64> {
    value["asDouble"]
        .as_f64()
        .or_else(|| value["asInt"].as_i64().map(|value| value as f64))
        .or_else(|| value["asInt"].as_str()?.parse().ok())
}

fn text(value: &Value, key: &str) -> String {
    value[key].as_str().unwrap_or_default().to_owned()
}

fn timestamp(ns: i64) -> String {
    let seconds = ns.div_euclid(1_000_000_000);
    let nanos = ns.rem_euclid(1_000_000_000) as u32;
    DateTime::<Utc>::from_timestamp(seconds, nanos)
        .unwrap_or(DateTime::<Utc>::UNIX_EPOCH)
        .to_rfc3339_opts(SecondsFormat::Secs, false)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn row(signal: &str, timestamp_ns: i64) -> QueryRow {
        QueryRow {
            signal: signal.to_owned(),
            timestamp_ns,
            start_timestamp_ns: None,
            end_timestamp_ns: None,
            name: None,
            severity_number: None,
            severity_text: None,
            trace_id: None,
            span_id: None,
            parent_span_id: None,
            status_code: None,
            service_name: None,
            service_namespace: None,
            k8s_namespace: None,
            workload: None,
            node: None,
            body_json: None,
            attributes_json: "{}".to_owned(),
            resource_json: "{}".to_owned(),
        }
    }

    #[test]
    fn overview_reports_signal_gaps_and_latest_evidence() {
        let mut metric = row("metrics", 20_000_000_000);
        metric.name = Some("k8s.node.condition_ready".into());
        metric.node = Some("node-a".into());
        metric.body_json = Some(r#"{"asInt":"1"}"#.into());

        let out = build_overview(
            vec![row("logs", 10_000_000_000), metric],
            QueryParams::for_test(0, 30_000_000_000),
        );

        assert_eq!(
            out.data_through.as_deref(),
            Some("1970-01-01T00:00:20+00:00")
        );
        assert_eq!(out.coverage[0].signal, "logs");
        assert_eq!(out.coverage[0].rows, 1);
        assert_eq!(out.coverage[1].signal, "metrics");
        assert_eq!(out.coverage[1].rows, 1);
        assert_eq!(out.coverage[2].signal, "traces");
        assert_eq!(out.coverage[2].status, "gap");
        assert_eq!(out.fidelity, "complete");
    }

    #[test]
    fn overview_deduplicates_warning_events() {
        let mut first = row("logs", 10_000_000_000);
        first.k8s_namespace = Some("nimmly".into());
        first.body_json = Some(r#"{"object":{"type":"Warning","reason":"Unhealthy","note":"readiness failed","regarding":{"kind":"Pod","name":"worker-abc"}},"type":"ADDED"}"#.into());
        let mut second = first.clone();
        second.timestamp_ns = 20_000_000_000;
        second.body_json = Some(r#"{"object":{"type":"Warning","reason":"Unhealthy","note":"readiness failed","regarding":{"kind":"Pod","name":"worker-abc"}},"type":"MODIFIED"}"#.into());

        let out = build_overview(
            vec![first, second],
            QueryParams::for_test(0, 30_000_000_000),
        );

        assert_eq!(out.warnings.len(), 1);
        assert_eq!(out.warnings[0].occurrences, 2);
        assert_eq!(out.warnings[0].resource, "worker-abc");
        assert_eq!(out.warnings[0].last_seen, "1970-01-01T00:00:20+00:00");
    }

    #[test]
    fn overview_uses_only_the_latest_point_for_each_container() {
        let resource =
            r#"{"k8s.deployment.name":"api","k8s.pod.uid":"pod-a","k8s.container.name":"api"}"#;
        let mut old = row("metrics", 10_000_000_000);
        old.name = Some("container.memory.working_set".into());
        old.k8s_namespace = Some("nimmly".into());
        old.workload = Some("api".into());
        old.body_json = Some(r#"{"asInt":"10"}"#.into());
        old.resource_json = resource.into();
        let mut latest = old.clone();
        latest.timestamp_ns = 20_000_000_000;
        latest.body_json = Some(r#"{"asInt":"25"}"#.into());

        let out = build_overview(vec![old, latest], QueryParams::for_test(0, 30_000_000_000));

        assert_eq!(out.workloads.len(), 1);
        assert_eq!(out.workloads[0].namespace, "nimmly");
        assert_eq!(out.workloads[0].workload, "api");
        assert_eq!(out.workloads[0].memory_working_set_bytes, Some(25));
    }

    #[test]
    fn namespace_and_workload_filters_bound_results() {
        let mut wanted = row("metrics", 10_000_000_000);
        wanted.name = Some("container.cpu.usage".into());
        wanted.k8s_namespace = Some("nimmly".into());
        wanted.workload = Some("worker".into());
        wanted.body_json = Some(r#"{"asDouble":0.25}"#.into());
        wanted.resource_json = r#"{"k8s.deployment.name":"worker","k8s.pod.uid":"pod-a","k8s.container.name":"worker"}"#.into();
        let mut other = wanted.clone();
        other.k8s_namespace = Some("infra".into());
        other.workload = Some("console".into());

        let mut params = QueryParams::for_test(0, 30_000_000_000);
        params.namespace = Some("nimmly".into());
        params.workload = Some("worker".into());
        let out = build_overview(vec![wanted, other], params);

        assert_eq!(out.workloads.len(), 1);
        assert_eq!(out.workloads[0].workload, "worker");
        assert_eq!(out.workloads[0].cpu_cores, Some(0.25));
    }

    #[test]
    fn completed_jobs_do_not_look_like_unhealthy_services() {
        let mut job = row("metrics", 10_000_000_000);
        job.name = Some("k8s.container.ready".into());
        job.k8s_namespace = Some("infra".into());
        job.workload = Some("backup-123".into());
        job.body_json = Some(r#"{"asInt":"0"}"#.into());
        job.resource_json =
            r#"{"k8s.job.name":"backup-123","k8s.pod.uid":"pod-a","k8s.container.name":"backup"}"#
                .into();

        let out = build_overview(vec![job], QueryParams::for_test(0, 30_000_000_000));

        assert!(out.workloads.is_empty());
    }

    #[test]
    fn controller_evidence_attaches_readiness_without_controller_attributes() {
        let mut cpu = row("metrics", 10_000_000_000);
        cpu.name = Some("container.cpu.usage".into());
        cpu.k8s_namespace = Some("infra".into());
        cpu.workload = Some("postgresql-primary".into());
        cpu.body_json = Some(r#"{"asDouble":0.05}"#.into());
        cpu.resource_json = r#"{"k8s.statefulset.name":"postgresql-primary","k8s.pod.uid":"pod-a","k8s.container.name":"postgresql"}"#.into();

        let mut ready = row("metrics", 20_000_000_000);
        ready.name = Some("k8s.container.ready".into());
        ready.k8s_namespace = Some("infra".into());
        ready.workload = Some("postgresql-primary".into());
        ready.body_json = Some(r#"{"asInt":"1"}"#.into());
        ready.resource_json = r#"{"k8s.pod.uid":"pod-a","k8s.container.name":"postgresql"}"#.into();

        let out = build_overview(vec![cpu, ready], QueryParams::for_test(0, 30_000_000_000));

        assert_eq!(out.workloads.len(), 1);
        assert_eq!(out.workloads[0].ready, Some(true));
        assert_eq!(out.workloads[0].ready_containers, Some(1));
    }

    #[test]
    fn stale_pods_do_not_inflate_the_latest_workload_sample() {
        fn metric(name: &str, pod: &str, timestamp_ns: i64, value: &str) -> QueryRow {
            let mut point = row("metrics", timestamp_ns);
            point.name = Some(name.into());
            point.k8s_namespace = Some("nimmly".into());
            point.workload = Some("worker".into());
            point.body_json = Some(value.into());
            point.resource_json = format!(
                r#"{{"k8s.deployment.name":"worker","k8s.pod.uid":"{pod}","k8s.container.name":"worker"}}"#
            );
            point
        }

        let stale_cpu = metric(
            "container.cpu.usage",
            "old",
            10_000_000_000,
            r#"{"asDouble":0.1}"#,
        );
        let stale_ready = metric(
            "k8s.container.ready",
            "old",
            10_000_000_000,
            r#"{"asInt":"1"}"#,
        );
        let current_cpu = metric(
            "container.cpu.usage",
            "new",
            600_000_000_000,
            r#"{"asDouble":0.2}"#,
        );
        let current_ready = metric(
            "k8s.container.ready",
            "new",
            600_000_000_000,
            r#"{"asInt":"1"}"#,
        );

        let out = build_overview(
            vec![stale_cpu, stale_ready, current_cpu, current_ready],
            QueryParams::for_test(0, 700_000_000_000),
        );

        assert_eq!(out.workloads[0].cpu_cores, Some(0.2));
        assert_eq!(out.workloads[0].ready_containers, Some(1));
        assert_eq!(out.workloads[0].total_containers, Some(1));
    }

    #[test]
    fn database_summary_uses_latest_gauges_and_window_counter_deltas() {
        fn pg_metric(
            workload: &str,
            name: &str,
            timestamp_ns: i64,
            value: f64,
            datname: Option<&str>,
        ) -> QueryRow {
            let mut point = row("metrics", timestamp_ns);
            point.name = Some(name.into());
            point.k8s_namespace = Some("infra".into());
            point.workload = Some(workload.into());
            point.body_json = Some(match datname {
                Some(datname) => format!(
                    r#"{{"asDouble":{value},"attributes":[{{"key":"datname","value":{{"stringValue":"{datname}"}}}}]}}"#
                ),
                None => format!(r#"{{"asDouble":{value}}}"#),
            });
            point.resource_json = format!(
                r#"{{"service.name":"postgresql-approved","k8s.statefulset.name":"{workload}"}}"#
            );
            point
        }

        let rows = [
            ("postgresql-primary", "pg_up", 20, 1.0, None),
            (
                "postgresql-primary",
                "pg_replication_is_replica",
                20,
                0.0,
                None,
            ),
            (
                "postgresql-primary",
                "pg_settings_max_connections",
                20,
                100.0,
                None,
            ),
            (
                "postgresql-primary",
                "pg_stat_database_numbackends",
                20,
                10.0,
                Some("postgres"),
            ),
            (
                "postgresql-primary",
                "pg_stat_database_numbackends",
                20,
                5.0,
                Some("app"),
            ),
            (
                "postgresql-primary",
                "pg_stat_database_xact_commit",
                10,
                100.0,
                Some("app"),
            ),
            (
                "postgresql-primary",
                "pg_stat_database_xact_commit",
                20,
                130.0,
                Some("app"),
            ),
            (
                "postgresql-primary",
                "pg_stat_database_xact_rollback",
                10,
                2.0,
                Some("app"),
            ),
            (
                "postgresql-primary",
                "pg_stat_database_xact_rollback",
                20,
                3.0,
                Some("app"),
            ),
            (
                "postgresql-primary",
                "pg_database_size_bytes",
                10,
                1_000.0,
                Some("app"),
            ),
            (
                "postgresql-primary",
                "pg_database_size_bytes",
                20,
                1_250.0,
                Some("app"),
            ),
            ("postgresql-read", "pg_up", 20, 1.0, None),
            (
                "postgresql-read",
                "pg_replication_is_replica",
                20,
                1.0,
                None,
            ),
            (
                "postgresql-read",
                "pg_replication_lag_seconds",
                20,
                0.25,
                None,
            ),
        ]
        .into_iter()
        .map(|(workload, name, timestamp, value, datname)| {
            pg_metric(workload, name, timestamp, value, datname)
        });

        let out = build_overview(rows, QueryParams::for_test(0, 30));

        assert_eq!(out.databases.len(), 2);
        let primary = out
            .databases
            .iter()
            .find(|database| database.role == "primary")
            .unwrap();
        assert_eq!(primary.available, Some(true));
        assert_eq!(primary.observed_from, "1970-01-01T00:00:00+00:00");
        assert_eq!(primary.connections, Some(15));
        assert_eq!(primary.max_connections, Some(100));
        assert_eq!(primary.transactions_in_window, Some(31));
        assert_eq!(primary.rollbacks_in_window, Some(1));
        assert_eq!(primary.size_bytes, Some(1_250));
        assert_eq!(primary.growth_bytes, Some(250));

        let replica = out
            .databases
            .iter()
            .find(|database| database.role == "replica")
            .unwrap();
        assert_eq!(replica.replication_lag_seconds, Some(0.25));
    }

    #[test]
    fn identity_summary_reports_bounded_routes_outcomes_and_latency() {
        fn identity_metric(instance: &str, name: &str, timestamp_ns: i64, body: &str) -> QueryRow {
            let mut point = row("metrics", timestamp_ns);
            point.name = Some(name.to_owned());
            point.body_json = Some(body.to_owned());
            point.resource_json = format!(
                r#"{{"service.name":"zitadel-approved","service.instance.id":"{instance}","k8s.deployment.name":"zitadel"}}"#
            );
            point
        }
        let counter = |instance: &str, name: &str, timestamp, value, attributes: &str| {
            identity_metric(
                instance,
                name,
                timestamp,
                &format!(r#"{{"asDouble":{value},"attributes":{attributes}}}"#),
            )
        };
        let response = |instance: &str, timestamp, value, method: &str, route: &str, code: &str| {
            counter(
                instance,
                "http_server_return_code_counter_total",
                timestamp,
                value,
                &format!(
                    r#"[{{"key":"method","value":{{"stringValue":"{method}"}}}},{{"key":"uri","value":{{"stringValue":"{route}"}}}},{{"key":"return_code","value":{{"stringValue":"{code}"}}}}]"#
                ),
            )
        };
        let histogram = |instance: &str, timestamp, count: u64, counts: &str| {
            identity_metric(
                instance,
                "http_server_request_duration_seconds",
                timestamp,
                &format!(
                    r#"{{"count":"{count}","sum":1.0,"bucketCounts":{counts},"explicitBounds":[0.1,1.0]}}"#
                ),
            )
        };

        let rows = vec![
            identity_metric("id-a", "up", 10, r#"{"asDouble":1}"#),
            identity_metric("id-a", "up", 20, r#"{"asDouble":1}"#),
            identity_metric("id-b", "up", 10, r#"{"asDouble":1}"#),
            identity_metric("id-b", "up", 20, r#"{"asDouble":1}"#),
            counter("id-a", "http_server_request_count_total", 10, 100, "[]"),
            counter("id-a", "http_server_request_count_total", 20, 140, "[]"),
            counter("id-b", "http_server_request_count_total", 10, 50, "[]"),
            counter("id-b", "http_server_request_count_total", 20, 80, "[]"),
            response("id-a", 10, 5, "GET", "/oauth/v2/authorize", "302"),
            response("id-a", 20, 8, "GET", "/oauth/v2/authorize", "302"),
            response("id-b", 10, 1, "GET", "/oauth/v2/authorize", "302"),
            response("id-b", 20, 2, "GET", "/oauth/v2/authorize", "302"),
            response("id-a", 10, 10, "POST", "/oauth/v2/token", "200"),
            response("id-a", 20, 12, "POST", "/oauth/v2/token", "200"),
            response("id-a", 20, 3, "POST", "/oauth/v2/token", "400"),
            response("id-b", 10, 4, "POST", "/oauth/v2/token", "200"),
            response("id-b", 20, 5, "POST", "/oauth/v2/token", "200"),
            response("id-a", 10, 1, "GET", "/oidc/v1/userinfo", "200"),
            response("id-a", 20, 2, "GET", "/oidc/v1/userinfo", "200"),
            response("id-a", 10, 10, "POST", "/oidc/v1/userinfo", "200"),
            response("id-a", 20, 13, "POST", "/oidc/v1/userinfo", "200"),
            histogram("id-a", 10, 10, "[8,2,0]"),
            histogram("id-a", 20, 14, "[10,4,0]"),
            histogram("id-b", 10, 10, "[8,2,0]"),
            histogram("id-b", 20, 14, "[10,4,0]"),
        ];

        let out = build_overview(rows, QueryParams::for_test(0, 30));
        let identity = out.identity.expect("identity summary");
        assert_eq!(identity.targets_observed_in_window, 2);
        assert_eq!(identity.targets_current, 2);
        assert_eq!(identity.targets_scrapeable, 2);
        assert_eq!(identity.http_requests_in_window, Some(70));
        assert_eq!(identity.routes.len(), 3);
        assert_eq!(identity.routes[0].route, "authorize");
        assert_eq!(identity.routes[0].requests_in_window, Some(4));
        assert_eq!(identity.routes[0].successful_requests_in_window, Some(4));
        assert_eq!(identity.routes[0].rejected_requests_in_window, Some(0));
        assert_eq!(identity.routes[1].route, "token");
        assert_eq!(identity.routes[1].requests_in_window, Some(6));
        assert_eq!(identity.routes[1].successful_requests_in_window, Some(3));
        assert_eq!(identity.routes[1].rejected_requests_in_window, Some(3));
        assert_eq!(identity.routes[2].route, "userinfo");
        assert_eq!(identity.routes[2].requests_in_window, Some(4));
        assert_eq!(identity.routes[2].successful_requests_in_window, Some(4));
        assert_eq!(identity.request_p95_latency_seconds, Some(1.0));
        assert_eq!(identity.request_p95_latency_overflow, Some(false));
    }

    #[test]
    fn storage_broker_summary_reports_availability_outcomes_and_success_latency() {
        fn broker_metric(instance: &str, name: &str, timestamp_ns: i64, body: &str) -> QueryRow {
            let mut point = row("metrics", timestamp_ns);
            point.name = Some(name.into());
            point.k8s_namespace = Some("infra".into());
            point.workload = Some("storage-broker".into());
            point.body_json = Some(body.into());
            point.resource_json = format!(
                r#"{{"service.name":"storage-broker-approved","service.instance.id":"{instance}","k8s.deployment.name":"storage-broker"}}"#
            );
            point
        }

        let counter = |instance: &str, timestamp, value, outcome: &str| {
            broker_metric(
                instance,
                "storage_broker_requests_total",
                timestamp,
                &format!(
                    r#"{{"asDouble":{value},"attributes":[{{"key":"operation","value":{{"stringValue":"presign"}}}},{{"key":"outcome","value":{{"stringValue":"{outcome}"}}}}]}}"#
                ),
            )
        };
        let histogram = |instance: &str, timestamp, count: u64, counts: &str| {
            broker_metric(
                instance,
                "storage_broker_request_duration_seconds",
                timestamp,
                &format!(
                    r#"{{"count":"{count}","sum":1.0,"bucketCounts":{counts},"explicitBounds":[0.01,0.1],"attributes":[{{"key":"operation","value":{{"stringValue":"presign"}}}},{{"key":"outcome","value":{{"stringValue":"success"}}}}]}}"#
                ),
            )
        };

        let rows = vec![
            broker_metric("broker-a", "up", 10, r#"{"asDouble":1}"#),
            broker_metric("broker-a", "up", 20, r#"{"asDouble":1}"#),
            broker_metric("broker-b", "up", 10, r#"{"asDouble":1}"#),
            broker_metric("broker-b", "up", 20, r#"{"asDouble":0}"#),
            counter("broker-a", 10, 100, "success"),
            counter("broker-a", 20, 130, "success"),
            counter("broker-b", 10, 50, "success"),
            counter("broker-b", 20, 70, "success"),
            // Sparse outcomes are absent at zero. A first rejection after an
            // earlier successful scrape must count from a zero baseline.
            counter("broker-a", 20, 1, "unauthenticated"),
            counter("broker-b", 10, 1, "error"),
            counter("broker-b", 20, 3, "error"),
            histogram("broker-a", 10, 100, "[90,10,0]"),
            histogram("broker-a", 20, 130, "[115,14,1]"),
            histogram("broker-b", 10, 50, "[40,10,0]"),
            histogram("broker-b", 20, 70, "[55,14,1]"),
        ];

        let out = build_overview(rows, QueryParams::for_test(0, 30));
        let broker = out.storage_broker.expect("storage broker summary");
        assert_eq!(broker.targets_observed_in_window, 2);
        assert_eq!(broker.targets_current, 2);
        assert_eq!(broker.targets_scrapeable, 1);
        assert_eq!(broker.requests_in_window, Some(53));
        assert_eq!(broker.successful_requests_in_window, Some(50));
        assert_eq!(broker.rejected_requests_in_window, Some(1));
        assert_eq!(broker.error_requests_in_window, Some(2));
        assert_eq!(broker.success_p95_latency_seconds, Some(0.1));
        assert_eq!(broker.success_p95_latency_overflow, Some(false));
    }

    #[test]
    fn storage_broker_target_freshness_uses_newest_metric_evidence() {
        let broker = StorageBrokerAcc {
            observed_from_ns: Some(10),
            sampled_ns: 20,
            replicas: HashMap::from([(
                "broker-a".into(),
                MetricSeries {
                    points: [(10, 1.0), (20, 1.0)].into(),
                },
            )]),
            ..Default::default()
        };

        let summary = summarize_storage_broker(broker, Some(200_000_000_000)).unwrap();
        assert_eq!(summary.targets_observed_in_window, 1);
        assert_eq!(summary.targets_current, 0);
        assert_eq!(summary.targets_scrapeable, 0);
    }

    #[test]
    fn storage_broker_histogram_reset_invalidates_p95() {
        let points = [
            (10, vec![90, 10, 0]),
            (15, vec![1, 0, 0]),
            (20, vec![115, 14, 1]),
        ]
        .into_iter()
        .map(|(timestamp, bucket_counts)| {
            (
                timestamp,
                HistogramPoint {
                    bounds: vec![0.01, 0.1],
                    bucket_counts,
                },
            )
        })
        .collect();
        let histograms = HashMap::from([("broker-a\0presign".into(), HistogramSeries { points })]);
        let targets = HashMap::from([(
            "broker-a".into(),
            MetricSeries {
                points: [(10, 1.0), (20, 1.0)].into(),
            },
        )]);

        assert_eq!(histogram_p95(&histograms, &targets), None);
    }

    #[test]
    fn storage_broker_histogram_marks_p95_above_largest_bucket() {
        let histograms = HashMap::from([(
            "broker-a\0presign".into(),
            HistogramSeries {
                points: [
                    (
                        10,
                        HistogramPoint {
                            bounds: vec![0.01, 0.1],
                            bucket_counts: vec![0, 0, 0],
                        },
                    ),
                    (
                        20,
                        HistogramPoint {
                            bounds: vec![0.01, 0.1],
                            bucket_counts: vec![0, 0, 10],
                        },
                    ),
                ]
                .into(),
            },
        )]);
        let targets = HashMap::from([(
            "broker-a".into(),
            MetricSeries {
                points: [(10, 1.0), (20, 1.0)].into(),
            },
        )]);

        assert_eq!(histogram_p95(&histograms, &targets), Some((0.1, true)));
    }

    #[test]
    fn database_counter_reset_does_not_report_a_partial_delta() {
        fn series(points: &[(i64, f64)]) -> MetricSeries {
            MetricSeries {
                points: points.iter().copied().collect(),
            }
        }
        let mut metrics = HashMap::from([
            (
                "counter\0app".to_owned(),
                series(&[(10, 10.0), (15, 1.0), (20, 20.0)]),
            ),
            (
                "counter\0postgres".to_owned(),
                series(&[(10, 5.0), (20, 8.0)]),
            ),
        ]);

        let delta = sum_metric(&metrics, "counter", |series| monotonic_delta(series, 0));
        assert_eq!(delta, None);

        metrics.insert("counter\0app".to_owned(), series(&[(20, 20.0)]));
        let partial = sum_metric(&metrics, "counter", |series| monotonic_delta(series, 0));
        assert_eq!(partial, None);
    }

    #[test]
    fn database_gauges_must_match_the_latest_scrape() {
        let resource =
            r#"{"service.name":"postgresql-approved","k8s.statefulset.name":"postgresql-primary"}"#;
        let mut stale_connections = row("metrics", 20);
        stale_connections.name = Some("pg_stat_database_numbackends".into());
        stale_connections.workload = Some("postgresql-primary".into());
        stale_connections.body_json = Some(r#"{"asInt":"12"}"#.into());
        stale_connections.resource_json = resource.into();
        let mut current_up = row("metrics", 200_000_000_000);
        current_up.name = Some("pg_up".into());
        current_up.workload = Some("postgresql-primary".into());
        current_up.body_json = Some(r#"{"asInt":"1"}"#.into());
        current_up.resource_json = resource.into();

        let out = build_overview(
            [stale_connections, current_up],
            QueryParams::for_test(0, 300_000_000_000),
        );

        assert_eq!(out.databases[0].available, Some(true));
        assert_eq!(out.databases[0].connections, None);
    }

    #[test]
    fn duplicate_counter_timestamp_is_counted_once() {
        let mut series = MetricSeries::default();
        series.points.insert(10, 1.0);
        series.points.insert(10, 1.0);
        series.points.insert(20, 3.0);

        assert_eq!(monotonic_delta(&series, 0), Some(2.0));
    }

    #[test]
    fn application_summary_is_complete_while_trace_and_logs_are_one_sample() {
        let mut summary = row("application_metrics", 20_000_000_000);
        summary.service_namespace = Some("nimmly".into());
        summary.service_name = Some("nimmly-api".into());
        summary.body_json = Some(
            r#"{"operation":"GET /health","operationKind":"http","executions":100,"successful":97,"rejected":2,"failed":1,"durationCount":100,"durationSumSeconds":8.0,"durationBounds":[0.05,0.1],"durationBucketCounts":[40,55,5],"sampledExecutions":1}"#.into(),
        );

        let mut trace = row("traces", 10_000_000_000);
        trace.start_timestamp_ns = Some(10_000_000_000);
        trace.end_timestamp_ns = Some(10_080_000_000);
        trace.name = Some("GET /health".into());
        trace.trace_id = Some("abc".into());
        trace.span_id = Some("root".into());
        trace.service_namespace = Some("nimmly".into());
        trace.service_name = Some("nimmly-api".into());
        trace.attributes_json =
            r#"{"infra.operation.kind":"http","infra.execution.outcome":"error","telemetry.sample.reason":"failure","telemetry.sample.bundle_complete":true}"#.into();

        let mut child = row("traces", 10_010_000_000);
        child.start_timestamp_ns = Some(10_010_000_000);
        child.end_timestamp_ns = Some(10_070_000_000);
        child.name = Some("SELECT".into());
        child.trace_id = Some("abc".into());
        child.span_id = Some("child".into());
        child.parent_span_id = Some("root".into());
        child.status_code = Some("2".into());
        child.service_namespace = Some("nimmly".into());
        child.service_name = Some("storage-broker".into());
        child.attributes_json =
            r#"{"server.address":"database","error.type":"timeout","telemetry.sample.reason":"failure","telemetry.sample.bundle_complete":true}"#.into();

        let mut log = row("logs", 10_080_000_000);
        log.trace_id = Some("abc".into());
        log.service_namespace = Some("nimmly".into());
        log.service_name = Some("nimmly-api".into());
        log.severity_number = Some(17);
        log.severity_text = Some("ERROR".into());
        log.body_json = Some(r#""request failed""#.into());
        log.attributes_json =
            r#"{"telemetry.sample.reason":"failure","telemetry.sample.bundle_complete":true}"#
                .into();

        let out = build_overview(
            [summary, trace, child, log],
            QueryParams::for_test(0, 30_000_000_000),
        );

        assert_eq!(out.fidelity, "mixed");
        assert_eq!(out.applications.len(), 1);
        assert_eq!(out.applications[0].executions, 100);
        assert_eq!(out.applications[0].failed, 1);
        assert_eq!(
            out.applications[0].operations[0].p95_duration_seconds,
            Some(0.1)
        );
        assert_eq!(out.applications[0].samples[0].trace_id, "abc");
        assert_eq!(out.applications[0].samples[0].operation, "GET /health");
        assert_eq!(out.applications[0].samples[0].spans.len(), 2);
        assert!(out.applications[0].complete);
        assert_eq!(out.applications[0].samples[0].spans[1].span_id, "child");
        assert_eq!(
            out.applications[0].samples[0].spans[1].service,
            "storage-broker"
        );
        assert_eq!(
            out.applications[0].samples[0].spans[1]
                .parent_span_id
                .as_deref(),
            Some("root")
        );
        assert_eq!(out.applications[0].samples[0].spans[1].status, "error");
        assert_eq!(
            out.applications[0].samples[0].spans[1]
                .server_address
                .as_deref(),
            Some("database")
        );
        assert!(out.applications[0].samples[0].complete);
        assert_eq!(
            out.applications[0].samples[0].logs[0].message,
            "request failed"
        );
    }

    #[test]
    fn application_drop_evidence_and_standalone_errors_are_visible() {
        let mut summary = row("application_metrics", 20_000_000_000);
        summary.service_namespace = Some("nimmly".into());
        summary.service_name = Some("nimmly-api".into());
        summary.body_json = Some(
            r#"{"operation":"GET /health","operationKind":"http","executions":1,"successful":1,"rejected":0,"failed":0,"durationBounds":[],"durationBucketCounts":[1],"sampledExecutions":0,"telemetryLossObserved":true}"#.into(),
        );
        let mut log = row("logs", 21_000_000_000);
        log.service_namespace = Some("nimmly".into());
        log.service_name = Some("nimmly-api".into());
        log.severity_number = Some(17);
        log.severity_text = Some("ERROR".into());
        log.body_json = Some(r#""uncorrelated failure""#.into());
        log.attributes_json = r#"{"telemetry.sample.reason":"standalone_error"}"#.into();

        let out = build_overview([summary, log], QueryParams::for_test(0, 30_000_000_000));

        assert!(!out.applications[0].complete);
        assert_eq!(out.applications[0].standalone_logs.len(), 1);
        assert_eq!(
            out.applications[0].standalone_logs[0].message,
            "uncorrelated failure"
        );
    }

    #[test]
    fn orphaned_correlated_error_is_visible_and_marks_service_partial() {
        let mut log = row("logs", 21_000_000_000);
        log.trace_id = Some("missing-root".into());
        log.span_id = Some("missing-span".into());
        log.service_namespace = Some("nimmly".into());
        log.service_name = Some("nimmly-api".into());
        log.severity_number = Some(17);
        log.severity_text = Some("ERROR".into());
        log.body_json = Some(r#""request failed""#.into());
        log.attributes_json = r#"{"telemetry.sample.reason":"orphaned_error","telemetry.sample.bundle_complete":false}"#.into();

        let out = build_overview([log], QueryParams::for_test(0, 30_000_000_000));

        assert_eq!(out.applications.len(), 1);
        assert!(!out.applications[0].complete);
        assert_eq!(out.applications[0].standalone_logs.len(), 1);
        assert!(out.applications[0].samples.is_empty());
    }

    #[test]
    fn late_orphaned_log_rejoins_a_root_retained_in_an_earlier_part() {
        let mut root = row("traces", 10_000_000_000);
        root.start_timestamp_ns = Some(10_000_000_000);
        root.end_timestamp_ns = Some(10_010_000_000);
        root.trace_id = Some("late".into());
        root.span_id = Some("root".into());
        root.name = Some("GET /health".into());
        root.service_namespace = Some("nimmly".into());
        root.service_name = Some("nimmly-api".into());
        root.attributes_json = r#"{"infra.operation.kind":"http","infra.execution.outcome":"success","telemetry.sample.reason":"slow","telemetry.sample.bundle_complete":true}"#.into();

        let mut log = row("logs", 20_000_000_000);
        log.trace_id = Some("late".into());
        log.span_id = Some("root".into());
        log.service_namespace = Some("nimmly".into());
        log.service_name = Some("nimmly-api".into());
        log.severity_number = Some(17);
        log.severity_text = Some("ERROR".into());
        log.body_json = Some(r#""request failed""#.into());
        log.attributes_json = r#"{"telemetry.sample.reason":"orphaned_error","telemetry.sample.bundle_complete":false}"#.into();

        let out = build_overview([root, log], QueryParams::for_test(0, 30_000_000_000));

        assert_eq!(out.applications[0].samples[0].logs.len(), 1);
        assert!(!out.applications[0].samples[0].complete);
        assert!(out.applications[0].standalone_logs.is_empty());
    }

    #[test]
    fn service_filter_keeps_the_complete_cross_service_trace_topology() {
        let mut api = row("traces", 10_000_000_000);
        api.start_timestamp_ns = Some(10_000_000_000);
        api.end_timestamp_ns = Some(10_100_000_000);
        api.trace_id = Some("shared".into());
        api.span_id = Some("api-root".into());
        api.name = Some("POST /crawl".into());
        api.service_namespace = Some("nimmly".into());
        api.service_name = Some("nimmly-api".into());
        api.attributes_json = r#"{"infra.operation.kind":"http","infra.execution.outcome":"success","telemetry.sample.reason":"slow","telemetry.sample.bundle_complete":true}"#.into();

        let mut worker = api.clone();
        worker.timestamp_ns = 10_020_000_000;
        worker.start_timestamp_ns = Some(10_020_000_000);
        worker.end_timestamp_ns = Some(10_090_000_000);
        worker.span_id = Some("worker-root".into());
        worker.parent_span_id = Some("api-root".into());
        worker.name = Some("process browser".into());
        worker.service_name = Some("nimmly-worker".into());
        worker.attributes_json = r#"{"infra.operation.kind":"crawler","infra.execution.outcome":"success","telemetry.sample.reason":"slow","telemetry.sample.bundle_complete":true}"#.into();

        let mut dependency = worker.clone();
        dependency.timestamp_ns = 10_030_000_000;
        dependency.start_timestamp_ns = Some(10_030_000_000);
        dependency.end_timestamp_ns = Some(10_080_000_000);
        dependency.span_id = Some("renderer".into());
        dependency.parent_span_id = Some("worker-root".into());
        dependency.name = Some("POST browser-renderer".into());
        dependency.attributes_json = r#"{"server.address":"browser-renderer","telemetry.sample.reason":"slow","telemetry.sample.bundle_complete":true}"#.into();

        let mut params = QueryParams::for_test(0, 30_000_000_000);
        params.service = Some("nimmly-api".into());
        let out = build_overview([api, worker, dependency], params);

        assert_eq!(out.applications.len(), 1);
        assert_eq!(out.applications[0].service, "nimmly-api");
        assert_eq!(out.applications[0].samples[0].spans.len(), 3);
        assert_eq!(
            out.applications[0].samples[0].spans[1].service,
            "nimmly-worker"
        );
    }

    #[test]
    fn service_filter_selects_only_the_requested_application() {
        let mut wanted = row("application_metrics", 20_000_000_000);
        wanted.service_namespace = Some("nimmly".into());
        wanted.service_name = Some("nimmly-api".into());
        wanted.body_json = Some(
            r#"{"operation":"GET /health","operationKind":"http","executions":1,"successful":1,"rejected":0,"failed":0,"durationBounds":[],"durationBucketCounts":[1],"sampledExecutions":0}"#.into(),
        );
        let mut other = wanted.clone();
        other.service_name = Some("nimmly-worker".into());
        let mut params = QueryParams::for_test(0, 30_000_000_000);
        params.service = Some("nimmly-api".into());

        let out = build_overview([wanted, other], params);

        assert_eq!(out.applications.len(), 1);
        assert_eq!(out.applications[0].service, "nimmly-api");
    }

    #[test]
    fn application_detail_budget_is_fair_and_does_not_change_total_completeness() {
        let mut rows = Vec::new();
        for service in ["nimmly-api", "nimmly-worker"] {
            for operation in ["GET /one", "GET /two"] {
                let mut summary = row("application_metrics", 20_000_000_000);
                summary.service_namespace = Some("nimmly".into());
                summary.service_name = Some(service.into());
                summary.body_json = Some(format!(
                    r#"{{"operation":"{operation}","operationKind":"http","executions":1,"successful":1,"rejected":0,"failed":0,"durationBounds":[],"durationBucketCounts":[1],"sampledExecutions":0}}"#
                ));
                rows.push(summary);
            }
        }
        let mut params = QueryParams::for_test(0, 30_000_000_000);
        params.limit = 2;

        let out = build_overview(rows, params);

        assert_eq!(out.applications.len(), 2);
        for service in &out.applications {
            assert_eq!(service.executions, 2);
            assert!(service.complete);
            assert!(service.details_truncated);
            assert_eq!(service.operations.len(), 1);
        }
    }
}
