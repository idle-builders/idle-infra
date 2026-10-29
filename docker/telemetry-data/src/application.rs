use std::collections::{BTreeMap, HashMap, HashSet};

use serde::{Deserialize, Serialize};
use serde_json::{Map, Value, json};
use sha2::{Digest, Sha256};

use crate::model::{POLICY_VERSION, TelemetryRow};

const DURATION_BOUNDS_SECONDS: [f64; 15] = [
    0.005, 0.01, 0.025, 0.05, 0.075, 0.1, 0.25, 0.5, 0.75, 1.0, 2.5, 5.0, 7.5, 10.0, 30.0,
];
const SLOW_SAMPLES_PER_OPERATION: usize = 3;
const HEALTHY_SAMPLE_PER_TEN_THOUSAND: u64 = 100;
const MAX_APPLICATION_SERVICES: usize = 8;
const MAX_TRACKED_TRACES: usize = 10_000;
const MAX_OPERATION_GROUPS: usize = 64;
const MAX_KEEP_DECISIONS: usize = 8;
const MAX_SEEN_EXECUTIONS: usize = 10_000;
const MAX_ROOTS_PER_TRACE: usize = 4;
const MAX_DEPENDENCY_SPANS_PER_TRACE: usize = 16;
const MAX_LOGS_PER_TRACE: usize = 8;
const MAX_STANDALONE_ERRORS: usize = 16;
const MAX_RETAINED_FIELD_BYTES: usize = 2_048;
const MAX_RETAINED_SCALAR_BYTES: usize = 253;

#[derive(Clone, Debug, Eq, Hash, Ord, PartialEq, PartialOrd)]
struct ServiceKey {
    namespace: String,
    service: String,
}

#[derive(Clone, Debug, Eq, Hash, Ord, PartialEq, PartialOrd)]
struct GroupKey {
    namespace: String,
    service: String,
    operation: String,
    kind: String,
}

impl GroupKey {
    fn service_key(&self) -> ServiceKey {
        ServiceKey {
            namespace: self.namespace.clone(),
            service: self.service.clone(),
        }
    }
}

#[derive(Default)]
struct SummaryAcc {
    first_ns: i64,
    last_ns: i64,
    executions: u64,
    successful: u64,
    rejected: u64,
    failed: u64,
    duration_count: u64,
    duration_sum_seconds: f64,
    duration_buckets: [u64; DURATION_BOUNDS_SECONDS.len() + 1],
    k8s_namespace: Option<String>,
    workload: Option<String>,
    node: Option<String>,
    telemetry_loss_observed: bool,
}

#[derive(Clone)]
struct SlowCandidate {
    trace_id: String,
    duration_ns: i64,
}

#[derive(Default)]
pub struct ApplicationWindow {
    sources: Vec<ApplicationSource>,
    source_window: String,
    important_log_traces: HashSet<String>,
    failed_execution_traces: HashSet<String>,
    seen_execution_spans: HashSet<String>,
    execution_traces: HashMap<String, Vec<GroupKey>>,
    decisions: HashMap<String, &'static str>,
    decision_counts: HashMap<ServiceKey, usize>,
    important_log_counts: HashMap<ServiceKey, usize>,
    observed_standalone_errors: HashMap<ServiceKey, usize>,
    groups: BTreeMap<GroupKey, SummaryAcc>,
    service_group_counts: HashMap<ServiceKey, usize>,
    slowest: BTreeMap<GroupKey, Vec<SlowCandidate>>,
    healthy_candidates: Vec<(String, ServiceKey)>,
    observed_roots: HashMap<String, usize>,
    observed_dependency_spans: HashMap<String, usize>,
    observed_logs: HashMap<String, usize>,
    retained_roots: HashMap<String, usize>,
    retained_dependency_spans: HashMap<String, usize>,
    retained_logs: HashMap<String, usize>,
    retained_standalone_errors: HashMap<ServiceKey, usize>,
    retained_row_keys: HashSet<String>,
    trace_services: HashMap<String, HashSet<ServiceKey>>,
    truncated_traces: HashSet<String>,
    orphaned_error_traces: HashSet<String>,
    partial_services: HashSet<ServiceKey>,
    admitted_services: HashSet<ServiceKey>,
    tracked_traces: HashSet<String>,
    cross_generation_partial: bool,
}

impl ApplicationWindow {
    pub fn with_period(period: &str, sources: &[ApplicationSource]) -> Self {
        Self {
            source_window: period.to_owned(),
            sources: sources.to_vec(),
            ..Self::default()
        }
    }

    pub fn mark_cross_generation_partial(&mut self) {
        self.cross_generation_partial = true;
    }

    pub fn observe_logs(&mut self, rows: &[TelemetryRow]) {
        for row in rows {
            if !is_application_row(row, &self.sources) {
                continue;
            }
            let Some(service_key) = service_key(row) else {
                continue;
            };
            if !self.admit_service(&service_key) {
                continue;
            }
            if retained_payload_too_large(row) {
                self.partial_services.insert(service_key.clone());
            }
            if let Some(trace_id) = row.trace_id.as_deref() {
                if !self.track_trace(trace_id, &service_key) {
                    continue;
                }
                self.trace_services
                    .entry(trace_id.to_owned())
                    .or_default()
                    .insert(service_key.clone());
                let count = self.observed_logs.entry(trace_id.to_owned()).or_default();
                *count += 1;
                if *count > MAX_LOGS_PER_TRACE || retained_payload_too_large(row) {
                    self.truncated_traces.insert(trace_id.to_owned());
                    self.partial_services.insert(service_key.clone());
                }
            }
            if row.severity_number.unwrap_or(0) < 17 {
                continue;
            }
            if let Some(trace_id) = row.trace_id.as_deref() {
                if self.important_log_traces.contains(trace_id) {
                    continue;
                }
                let count = self
                    .important_log_counts
                    .entry(service_key.clone())
                    .or_default();
                if *count >= MAX_KEEP_DECISIONS {
                    self.partial_services.insert(service_key);
                } else {
                    *count += 1;
                    self.important_log_traces.insert(trace_id.to_owned());
                }
            } else {
                let count = self
                    .observed_standalone_errors
                    .entry(service_key.clone())
                    .or_default();
                *count += 1;
                if *count > MAX_STANDALONE_ERRORS {
                    self.partial_services.insert(service_key);
                }
            }
        }
    }

    pub fn observe_traces(&mut self, rows: &[TelemetryRow]) {
        for row in rows {
            if row.signal != "traces" || !is_application_row(row, &self.sources) {
                continue;
            }
            let Some(trace_id) = row.trace_id.as_deref() else {
                continue;
            };
            let Some(service) = row.service_name.as_deref() else {
                continue;
            };
            let Some(namespace) = row.service_namespace.as_deref() else {
                continue;
            };
            let service_key = ServiceKey {
                namespace: namespace.to_owned(),
                service: service.to_owned(),
            };
            if !self.admit_service(&service_key) || !self.track_trace(trace_id, &service_key) {
                continue;
            }
            self.trace_services
                .entry(trace_id.to_owned())
                .or_default()
                .insert(service_key.clone());
            let is_root = is_application_execution(row, &self.sources);
            let count = if is_root {
                self.observed_roots.entry(trace_id.to_owned()).or_default()
            } else {
                self.observed_dependency_spans
                    .entry(trace_id.to_owned())
                    .or_default()
            };
            *count += 1;
            let row_cap = if is_root {
                MAX_ROOTS_PER_TRACE
            } else {
                MAX_DEPENDENCY_SPANS_PER_TRACE
            };
            if *count > row_cap || retained_payload_too_large(row) {
                self.truncated_traces.insert(trace_id.to_owned());
                self.partial_services.insert(service_key.clone());
            }
            if !is_root {
                continue;
            }
            let Some(operation) = row.name.as_deref() else {
                continue;
            };
            let execution_id = format!(
                "{}\0{}\0{}\0{}",
                namespace,
                service,
                trace_id,
                row.span_id.as_deref().unwrap_or(trace_id)
            );
            if !self.seen_execution_spans.contains(&execution_id) {
                if self.seen_execution_spans.len() >= MAX_SEEN_EXECUTIONS {
                    self.partial_services.insert(service_key.clone());
                    continue;
                }
                self.seen_execution_spans.insert(execution_id);
            } else {
                continue;
            }
            let attrs = json_object(&row.attributes_json);
            let kind = attrs
                .get("infra.operation.kind")
                .and_then(Value::as_str)
                .unwrap_or("unknown");
            let key = GroupKey {
                namespace: namespace.to_owned(),
                service: service.to_owned(),
                operation: bound_string(operation),
                kind: bound_string(kind),
            };
            let key = if self.groups.contains_key(&key) {
                key
            } else {
                let count = self
                    .service_group_counts
                    .entry(service_key.clone())
                    .or_default();
                if *count < MAX_OPERATION_GROUPS {
                    *count += 1;
                    key
                } else {
                    self.partial_services.insert(service_key.clone());
                    GroupKey {
                        namespace: namespace.to_owned(),
                        service: service.to_owned(),
                        operation: "other".to_owned(),
                        kind: "other".to_owned(),
                    }
                }
            };
            let outcome = attrs
                .get("infra.execution.outcome")
                .and_then(Value::as_str)
                .unwrap_or("error");
            let start_ns = row.start_timestamp_ns.unwrap_or(row.timestamp_ns);
            let end_ns = row.end_timestamp_ns.unwrap_or(start_ns).max(start_ns);
            let duration_ns = end_ns - start_ns;
            let duration_seconds = duration_ns as f64 / 1_000_000_000.0;
            let acc = self.groups.entry(key.clone()).or_default();
            acc.first_ns = if acc.first_ns == 0 {
                start_ns
            } else {
                acc.first_ns.min(start_ns)
            };
            acc.last_ns = acc.last_ns.max(end_ns);
            acc.executions += 1;
            match outcome {
                "success" => acc.successful += 1,
                "rejected" => acc.rejected += 1,
                _ => acc.failed += 1,
            }
            acc.duration_count += 1;
            acc.duration_sum_seconds += duration_seconds;
            acc.duration_buckets[bucket(duration_seconds)] += 1;
            acc.telemetry_loss_observed |= attrs
                .get("telemetry.sdk.dropped.records")
                .and_then(Value::as_u64)
                .unwrap_or(0)
                > 0;
            if acc.k8s_namespace.is_none() {
                acc.k8s_namespace.clone_from(&row.k8s_namespace);
            }
            if acc.workload.is_none() {
                acc.workload.clone_from(&row.workload);
            }
            if acc.node.is_none() {
                acc.node.clone_from(&row.node);
            }
            self.execution_traces
                .entry(trace_id.to_owned())
                .or_default()
                .push(key.clone());
            if !matches!(outcome, "success" | "rejected") {
                self.failed_execution_traces.insert(trace_id.to_owned());
                self.choose_for_service(trace_id, "failure", &service_key);
            } else {
                let candidates = self.slowest.entry(key).or_default();
                candidates.push(SlowCandidate {
                    trace_id: trace_id.to_owned(),
                    duration_ns,
                });
                candidates.sort_by(|left, right| right.duration_ns.cmp(&left.duration_ns));
                candidates.truncate(SLOW_SAMPLES_PER_OPERATION);
                if healthy_sample(trace_id) {
                    self.healthy_candidates
                        .push((trace_id.to_owned(), service_key));
                }
            }
        }
    }

    pub fn finish_decisions(&mut self) {
        for trace_id in self.important_log_traces.clone() {
            if let Some(group) = self
                .execution_traces
                .get(&trace_id)
                .and_then(|groups| groups.first())
                .cloned()
            {
                self.choose_for_service(&trace_id, "error_log", &group.service_key());
            } else {
                self.orphaned_error_traces.insert(trace_id.clone());
                if let Some(services) = self.trace_services.get(&trace_id) {
                    self.partial_services.extend(services.iter().cloned());
                }
            }
        }
        let slow = self
            .slowest
            .values()
            .flat_map(|candidates| {
                candidates
                    .iter()
                    .map(|candidate| candidate.trace_id.clone())
            })
            .collect::<Vec<_>>();
        for trace_id in slow {
            if let Some(group) = self
                .execution_traces
                .get(&trace_id)
                .and_then(|groups| groups.first())
                .cloned()
            {
                self.choose_for_service(&trace_id, "slow", &group.service_key());
            }
        }
        for (trace_id, service) in self.healthy_candidates.clone() {
            self.choose_for_service(&trace_id, "healthy_sample", &service);
        }
    }

    pub fn retain(&mut self, mut row: TelemetryRow) -> Option<TelemetryRow> {
        enum RetentionClass {
            TraceLog(String),
            Root(String),
            Dependency(String),
            Standalone(ServiceKey),
        }

        if !is_application_row(&row, &self.sources) {
            return None;
        }
        let decision = row
            .trace_id
            .as_deref()
            .and_then(|trace_id| self.decisions.get(trace_id).copied());
        let standalone_error = row.signal == "logs" && row.severity_number.unwrap_or(0) >= 17;
        if decision.is_none() && !standalone_error {
            return None;
        }
        let (reason, class) =
            if let (Some(trace_id), Some(reason)) = (row.trace_id.as_deref(), decision) {
                let class = if row.signal == "logs" {
                    RetentionClass::TraceLog(trace_id.to_owned())
                } else if is_application_execution(&row, &self.sources) {
                    RetentionClass::Root(trace_id.to_owned())
                } else {
                    RetentionClass::Dependency(trace_id.to_owned())
                };
                (reason, class)
            } else if standalone_error {
                let service_key = service_key(&row)?;
                let reason = if row.trace_id.is_some() {
                    "orphaned_error"
                } else {
                    "standalone_error"
                };
                (reason, RetentionClass::Standalone(service_key))
            } else {
                return None;
            };

        let at_cap = match &class {
            RetentionClass::TraceLog(trace_id) => {
                self.retained_logs.get(trace_id).copied().unwrap_or(0) >= MAX_LOGS_PER_TRACE
            }
            RetentionClass::Root(trace_id) => {
                self.retained_roots.get(trace_id).copied().unwrap_or(0) >= MAX_ROOTS_PER_TRACE
            }
            RetentionClass::Dependency(trace_id) => {
                self.retained_dependency_spans
                    .get(trace_id)
                    .copied()
                    .unwrap_or(0)
                    >= MAX_DEPENDENCY_SPANS_PER_TRACE
            }
            RetentionClass::Standalone(service) => {
                self.retained_standalone_errors
                    .get(service)
                    .copied()
                    .unwrap_or(0)
                    >= MAX_STANDALONE_ERRORS
            }
        };
        if at_cap || !self.retained_row_keys.insert(retained_row_key(&row)) {
            return None;
        }
        match class {
            RetentionClass::TraceLog(trace_id) => {
                *self.retained_logs.entry(trace_id).or_default() += 1;
            }
            RetentionClass::Root(trace_id) => {
                *self.retained_roots.entry(trace_id).or_default() += 1;
            }
            RetentionClass::Dependency(trace_id) => {
                *self.retained_dependency_spans.entry(trace_id).or_default() += 1;
            }
            RetentionClass::Standalone(service) => {
                *self.retained_standalone_errors.entry(service).or_default() += 1;
            }
        }
        let has_root = row
            .trace_id
            .as_deref()
            .is_some_and(|trace_id| self.execution_traces.contains_key(trace_id));
        let complete = row.trace_id.as_deref().is_some_and(|trace_id| {
            has_root
                && (!self.failed_execution_traces.contains(trace_id)
                    || self.important_log_traces.contains(trace_id))
                && !self.truncated_traces.contains(trace_id)
        });
        sanitize_retained_row(&mut row);
        let mut attrs = json_object(&row.attributes_json);
        attrs.insert("telemetry.sample.reason".to_owned(), json!(reason));
        attrs.insert(
            "telemetry.sample.bundle_complete".to_owned(),
            json!(complete),
        );
        attrs.insert(
            "telemetry.sample.probability".to_owned(),
            json!(if reason == "healthy_sample" {
                0.01
            } else {
                1.0
            }),
        );
        attrs.insert("telemetry.policy.version".to_owned(), json!(POLICY_VERSION));
        attrs.insert(
            "telemetry.source.window".to_owned(),
            json!(self.source_window),
        );
        row.attributes_json = Value::Object(attrs).to_string();
        Some(row)
    }

    pub fn summary_rows(&self) -> Vec<TelemetryRow> {
        self.groups
            .iter()
            .map(|(key, acc)| {
                let samples = self
                    .execution_traces
                    .iter()
                    .filter(|(trace_id, group)| {
                        group.contains(key) && self.decisions.contains_key(trace_id.as_str())
                    })
                    .count();
                let body = ApplicationSummaryBody {
                    operation: &key.operation,
                    operation_kind: &key.kind,
                    executions: acc.executions,
                    successful: acc.successful,
                    rejected: acc.rejected,
                    failed: acc.failed,
                    duration_count: acc.duration_count,
                    duration_sum_seconds: acc.duration_sum_seconds,
                    duration_bounds: &DURATION_BOUNDS_SECONDS,
                    duration_bucket_counts: &acc.duration_buckets,
                    sampled_executions: samples,
                    telemetry_loss_observed: acc.telemetry_loss_observed
                        || self.cross_generation_partial
                        || self.partial_services.contains(&ServiceKey {
                            namespace: key.namespace.clone(),
                            service: key.service.clone(),
                        })
                        || self.execution_traces.iter().any(|(trace_id, groups)| {
                            groups.contains(key) && self.truncated_traces.contains(trace_id)
                        }),
                };
                TelemetryRow {
                    signal: "application_metrics".to_owned(),
                    timestamp_ns: acc.last_ns,
                    start_timestamp_ns: Some(acc.first_ns),
                    observed_timestamp_ns: None,
                    end_timestamp_ns: None,
                    name: Some("application.operation.summary".to_owned()),
                    kind: Some("summary".to_owned()),
                    severity_number: None,
                    severity_text: None,
                    trace_id: None,
                    span_id: None,
                    parent_span_id: None,
                    status_code: None,
                    status_message: None,
                    trace_state: None,
                    flags: None,
                    events_json: None,
                    links_json: None,
                    dropped_attributes_count: None,
                    dropped_events_count: None,
                    dropped_links_count: None,
                    metric_description: Some("Complete application operation summary".to_owned()),
                    metric_unit: Some("s".to_owned()),
                    aggregation_temporality: None,
                    is_monotonic: None,
                    scope_json: "{\"name\":\"telemetry-data/application\"}".to_owned(),
                    scope_schema_url: None,
                    resource_schema_url: None,
                    service_name: Some(key.service.clone()),
                    service_namespace: Some(key.namespace.clone()),
                    k8s_namespace: acc.k8s_namespace.clone(),
                    workload: acc.workload.clone(),
                    node: acc.node.clone(),
                    body_json: Some(serde_json::to_string(&body).expect("summary is serializable")),
                    attributes_json: json!({
                        "infra.operation.kind": key.kind,
                        "telemetry.aggregate.period": "minute"
                    })
                    .to_string(),
                    resource_json: json!({
                        "service.namespace": key.namespace,
                        "service.name": key.service
                    })
                    .to_string(),
                }
            })
            .collect()
    }

    fn choose_for_service(&mut self, trace_id: &str, reason: &'static str, service: &ServiceKey) {
        if !self.decisions.contains_key(trace_id) {
            let count = self.decision_counts.entry(service.clone()).or_default();
            if *count >= MAX_KEEP_DECISIONS {
                self.partial_services.insert(service.clone());
                return;
            }
            *count += 1;
        }
        match self.decisions.get(trace_id) {
            Some(current) if reason_rank(current) >= reason_rank(reason) => {}
            _ => {
                self.decisions.insert(trace_id.to_owned(), reason);
            }
        }
    }

    fn admit_service(&mut self, service: &ServiceKey) -> bool {
        if self.admitted_services.contains(service) {
            return true;
        }
        if self.admitted_services.len() >= MAX_APPLICATION_SERVICES {
            return false;
        }
        self.admitted_services.insert(service.clone());
        true
    }

    fn track_trace(&mut self, trace_id: &str, service: &ServiceKey) -> bool {
        if self.tracked_traces.contains(trace_id) {
            return true;
        }
        if self.tracked_traces.len() >= MAX_TRACKED_TRACES {
            self.partial_services.insert(service.clone());
            return false;
        }
        self.tracked_traces.insert(trace_id.to_owned());
        true
    }
}

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct ApplicationSummaryBody<'a> {
    operation: &'a str,
    operation_kind: &'a str,
    executions: u64,
    successful: u64,
    rejected: u64,
    failed: u64,
    duration_count: u64,
    duration_sum_seconds: f64,
    duration_bounds: &'a [f64],
    duration_bucket_counts: &'a [u64],
    sampled_executions: usize,
    telemetry_loss_observed: bool,
}

pub fn is_application_row(row: &TelemetryRow, sources: &[ApplicationSource]) -> bool {
    if !matches!(row.signal.as_str(), "logs" | "traces") {
        return false;
    }
    onboarded_source(row, sources)
        && json_object(&row.resource_json)
            .get("infra.telemetry.type")
            .and_then(Value::as_str)
            == Some("application")
}

#[derive(Clone, Debug, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ApplicationSource {
    k8s_namespace: String,
    service_namespace: String,
    service_name: String,
    workload: Option<String>,
    cronjob: Option<String>,
}

pub fn parse_sources(json: &str) -> anyhow::Result<Vec<ApplicationSource>> {
    let sources: Vec<ApplicationSource> = serde_json::from_str(json)?;
    for source in &sources {
        let valid_name =
            |value: &str| !value.is_empty() && value.trim() == value && !value.contains('*');
        anyhow::ensure!(
            valid_name(&source.k8s_namespace)
                && valid_name(&source.service_namespace)
                && valid_name(&source.service_name),
            "application source identity fields must be nonempty exact names"
        );
        anyhow::ensure!(
            source.workload.is_some() != source.cronjob.is_some(),
            "application source needs exactly one workload or cronjob selector"
        );
        anyhow::ensure!(
            source
                .workload
                .as_deref()
                .or(source.cronjob.as_deref())
                .is_some_and(valid_name),
            "application workload selector must be an exact name"
        );
    }
    Ok(sources)
}

fn onboarded_source(row: &TelemetryRow, sources: &[ApplicationSource]) -> bool {
    sources.iter().any(|source| {
        row.k8s_namespace.as_deref() == Some(source.k8s_namespace.as_str())
            && row.service_namespace.as_deref() == Some(source.service_namespace.as_str())
            && row.service_name.as_deref() == Some(source.service_name.as_str())
            && match (&source.workload, &source.cronjob) {
                (Some(workload), None) => row.workload.as_deref() == Some(workload.as_str()),
                (None, Some(cronjob)) => is_cronjob_workload(row.workload.as_deref(), cronjob),
                _ => false,
            }
    })
}

fn is_cronjob_workload(workload: Option<&str>, cronjob: &str) -> bool {
    let Some(suffix) = workload.and_then(|value| value.strip_prefix(cronjob)) else {
        return false;
    };
    let Some(timestamp) = suffix.strip_prefix('-') else {
        return false;
    };
    (8..=10).contains(&timestamp.len()) && timestamp.bytes().all(|byte| byte.is_ascii_digit())
}

fn is_application_execution(row: &TelemetryRow, sources: &[ApplicationSource]) -> bool {
    is_application_row(row, sources)
        && json_object(&row.attributes_json).contains_key("infra.operation.kind")
}

fn service_key(row: &TelemetryRow) -> Option<ServiceKey> {
    Some(ServiceKey {
        namespace: row.service_namespace.clone()?,
        service: row.service_name.clone()?,
    })
}

fn retained_row_key(row: &TelemetryRow) -> String {
    let mut digest = Sha256::new();
    for value in [
        row.signal.as_str(),
        row.service_namespace.as_deref().unwrap_or(""),
        row.service_name.as_deref().unwrap_or(""),
        row.trace_id.as_deref().unwrap_or(""),
        row.span_id.as_deref().unwrap_or(""),
        row.body_json.as_deref().unwrap_or(""),
    ] {
        digest.update(value.as_bytes());
        digest.update([0]);
    }
    digest.update(row.timestamp_ns.to_le_bytes());
    hex::encode(digest.finalize())
}

fn retained_payload_too_large(row: &TelemetryRow) -> bool {
    [
        row.attributes_json.as_str(),
        row.resource_json.as_str(),
        row.scope_json.as_str(),
    ]
    .into_iter()
    .any(|value| value.len() > MAX_RETAINED_FIELD_BYTES)
        || [
            row.body_json.as_deref(),
            row.events_json.as_deref(),
            row.links_json.as_deref(),
        ]
        .into_iter()
        .flatten()
        .any(|value| value.len() > MAX_RETAINED_FIELD_BYTES)
        || row.events_json.is_some()
        || row.links_json.is_some()
        || [
            row.name.as_deref(),
            row.kind.as_deref(),
            row.severity_text.as_deref(),
            row.trace_id.as_deref(),
            row.span_id.as_deref(),
            row.parent_span_id.as_deref(),
            row.status_code.as_deref(),
            row.status_message.as_deref(),
            row.trace_state.as_deref(),
            row.service_name.as_deref(),
            row.service_namespace.as_deref(),
            row.k8s_namespace.as_deref(),
            row.workload.as_deref(),
            row.node.as_deref(),
        ]
        .into_iter()
        .flatten()
        .any(|value| value.len() > MAX_RETAINED_SCALAR_BYTES)
}

fn sanitize_retained_row(row: &mut TelemetryRow) {
    if row
        .body_json
        .as_ref()
        .is_some_and(|value| value.len() > MAX_RETAINED_FIELD_BYTES)
    {
        row.body_json = Some(json!("[truncated]").to_string());
    }
    row.events_json = None;
    row.links_json = None;
    row.scope_json = "{}".to_owned();
    row.scope_schema_url = None;
    row.resource_schema_url = None;
    row.status_message = None;
    row.trace_state = None;
    row.metric_description = None;
    row.metric_unit = None;
    row.aggregation_temporality = None;
    row.is_monotonic = None;
    row.attributes_json = bounded_whitelist_object(
        &row.attributes_json,
        &[
            "infra.operation.kind",
            "infra.execution.outcome",
            "telemetry.sdk.dropped.records",
            "http.request.method",
            "http.response.status_code",
            "http.route",
            "error.type",
            "code.file.path",
            "code.function.name",
            "code.line.number",
            "server.address",
            "messaging.system",
            "messaging.operation.name",
            "messaging.destination.name",
        ],
    );
    row.resource_json = bounded_whitelist_object(
        &row.resource_json,
        &[
            "infra.telemetry.type",
            "service.namespace",
            "service.name",
            "service.version",
            "service.instance.id",
            "deployment.environment.name",
        ],
    );
    for value in [
        &mut row.name,
        &mut row.kind,
        &mut row.severity_text,
        &mut row.trace_id,
        &mut row.span_id,
        &mut row.parent_span_id,
        &mut row.status_code,
        &mut row.service_name,
        &mut row.service_namespace,
        &mut row.k8s_namespace,
        &mut row.workload,
        &mut row.node,
    ] {
        bound_option(value);
    }
}

fn bounded_whitelist_object(value: &str, keys: &[&str]) -> String {
    let source = json_object(value);
    let mut output = Map::new();
    for key in keys {
        let Some(value) = source.get(*key).and_then(bounded_json_scalar) else {
            continue;
        };
        output.insert((*key).to_owned(), value);
        if Value::Object(output.clone()).to_string().len() > MAX_RETAINED_FIELD_BYTES {
            output.remove(*key);
            break;
        }
    }
    Value::Object(output).to_string()
}

fn bounded_json_scalar(value: &Value) -> Option<Value> {
    match value {
        Value::String(value) => Some(Value::String(bound_string(value))),
        Value::Number(_) | Value::Bool(_) | Value::Null => Some(value.clone()),
        _ => None,
    }
}

fn bound_option(value: &mut Option<String>) {
    if let Some(current) = value {
        *current = bound_string(current);
    }
}

fn bound_string(value: &str) -> String {
    if value.len() <= MAX_RETAINED_SCALAR_BYTES {
        return value.to_owned();
    }
    let mut end = MAX_RETAINED_SCALAR_BYTES;
    while !value.is_char_boundary(end) {
        end -= 1;
    }
    value[..end].to_owned()
}

fn json_object(value: &str) -> Map<String, Value> {
    serde_json::from_str::<Value>(value)
        .ok()
        .and_then(|value| value.as_object().cloned())
        .unwrap_or_default()
}

fn bucket(duration_seconds: f64) -> usize {
    DURATION_BOUNDS_SECONDS
        .iter()
        .position(|bound| duration_seconds <= *bound)
        .unwrap_or(DURATION_BOUNDS_SECONDS.len())
}

fn healthy_sample(trace_id: &str) -> bool {
    let digest = Sha256::digest(trace_id.as_bytes());
    let value = u64::from_be_bytes(digest[..8].try_into().expect("sha256 prefix is 8 bytes"));
    value % 10_000 < HEALTHY_SAMPLE_PER_TEN_THOUSAND
}

fn reason_rank(reason: &str) -> u8 {
    match reason {
        "failure" => 4,
        "error_log" => 3,
        "slow" => 2,
        "healthy_sample" => 1,
        _ => 0,
    }
}

#[cfg(test)]
pub fn test_sources() -> Vec<ApplicationSource> {
    parse_sources(r#"[
      {"k8s_namespace":"acme","service_namespace":"acme","service_name":"acme-api","workload":"acme-api"},
      {"k8s_namespace":"acme","service_namespace":"acme","service_name":"acme-api-crawler-request-worker-general","workload":"acme-api-crawler-request-worker"},
      {"k8s_namespace":"acme","service_namespace":"acme","service_name":"acme-api-source-normalizer","cronjob":"acme-api-source-normalizer"}
    ]"#).unwrap()
}

#[cfg(test)]
mod tests {
    use serde_json::json;

    use super::*;
    use crate::model::TelemetryRow;

    fn row(signal: &str, trace_id: &str, outcome: &str, duration_ms: i64) -> TelemetryRow {
        let padded_span_id = format!("{trace_id:0>16}");
        TelemetryRow {
            signal: signal.into(),
            timestamp_ns: 1_000_000_000,
            start_timestamp_ns: (signal == "traces").then_some(1_000_000_000),
            observed_timestamp_ns: None,
            end_timestamp_ns: (signal == "traces")
                .then_some(1_000_000_000 + duration_ms * 1_000_000),
            name: (signal == "traces").then(|| "GET /health".into()),
            kind: (signal == "traces").then(|| "2".into()),
            severity_number: (signal == "logs").then_some(17),
            severity_text: (signal == "logs").then(|| "ERROR".into()),
            trace_id: Some(trace_id.into()),
            span_id: Some(padded_span_id[padded_span_id.len() - 16..].into()),
            parent_span_id: None,
            status_code: (outcome == "error").then(|| "2".into()),
            status_message: None,
            trace_state: None,
            flags: Some(1),
            events_json: None,
            links_json: None,
            dropped_attributes_count: None,
            dropped_events_count: None,
            dropped_links_count: None,
            metric_description: None,
            metric_unit: None,
            aggregation_temporality: None,
            is_monotonic: None,
            scope_json: "{}".into(),
            scope_schema_url: None,
            resource_schema_url: None,
            service_name: Some("acme-api".into()),
            service_namespace: Some("acme".into()),
            k8s_namespace: Some("acme".into()),
            workload: Some("acme-api".into()),
            node: Some("node-a".into()),
            body_json: (signal == "logs").then(|| json!("request failed").to_string()),
            attributes_json: json!({
                "infra.operation.kind": "http",
                "infra.execution.outcome": outcome,
                "http.route": "/health"
            })
            .to_string(),
            resource_json: json!({
                "infra.telemetry.type": "application",
                "service.namespace": "acme",
                "service.name": "acme-api"
            })
            .to_string(),
        }
    }

    #[test]
    fn sources_are_explicit_and_configuration_is_validated() {
        let row = row("traces", "a", "success", 1);
        assert!(!is_application_row(&row, &parse_sources("[]").unwrap()));
        assert!(parse_sources("not json").is_err());
        assert!(parse_sources(r#"[{"k8s_namespace":"acme","service_namespace":"acme","service_name":"api","workload":"*"}]"#).is_err());
        assert!(parse_sources(r#"[{"k8s_namespace":"acme","service_namespace":"acme","service_name":"api","workload":"api","cronjob":"api"}]"#).is_err());
        assert!(
            parse_sources(
                r#"[{"k8s_namespace":"acme","service_namespace":"acme","service_name":"api"}]"#
            )
            .is_err()
        );
        let mut wrong_workload = row.clone();
        wrong_workload.workload = Some("acme-api-unreviewed".into());
        assert!(!is_application_row(&wrong_workload, &test_sources()));
        let mut window = ApplicationWindow::default();
        window.observe_traces(&[row]);
        assert!(window.summary_rows().is_empty());
    }

    #[test]
    fn aggregates_every_execution_before_sampling() {
        let mut window = ApplicationWindow::with_period("", &test_sources());
        window.observe_traces(&[
            row("traces", "a", "success", 10),
            row("traces", "a", "success", 10),
            row("traces", "b", "success", 20),
            row("traces", "c", "error", 30),
        ]);
        window.finish_decisions();
        let summaries = window.summary_rows();
        assert_eq!(summaries.len(), 1);
        let body: serde_json::Value =
            serde_json::from_str(summaries[0].body_json.as_deref().unwrap()).unwrap();
        assert_eq!(body["executions"], 3);
        assert_eq!(body["successful"], 2);
        assert_eq!(body["failed"], 1);
        assert_eq!(body["durationCount"], 3);
    }

    #[test]
    fn identical_span_ids_in_different_traces_are_distinct_executions() {
        let first = row("traces", "first", "success", 10);
        let mut second = row("traces", "second", "success", 20);
        second.span_id = first.span_id.clone();

        let mut window = ApplicationWindow::with_period("", &test_sources());
        window.observe_traces(&[first, second]);

        let body: Value =
            serde_json::from_str(window.summary_rows()[0].body_json.as_deref().unwrap()).unwrap();
        assert_eq!(body["executions"], 2);
        assert_eq!(body["successful"], 2);
    }

    #[test]
    fn counts_each_service_execution_inside_one_distributed_trace() {
        let mut api = row("traces", "shared", "success", 10);
        api.span_id = Some("aaaaaaaaaaaaaaaa".into());
        let mut worker = row("traces", "shared", "success", 20);
        worker.span_id = Some("bbbbbbbbbbbbbbbb".into());
        worker.service_name = Some("acme-api-crawler-request-worker-general".into());
        worker.workload = Some("acme-api-crawler-request-worker".into());
        worker.name = Some("process item".into());
        worker.attributes_json = json!({
            "infra.operation.kind": "crawler",
            "infra.execution.outcome": "success"
        })
        .to_string();

        let mut window = ApplicationWindow::with_period("", &test_sources());
        window.observe_traces(&[api, worker]);

        assert_eq!(window.summary_rows().len(), 2);
    }

    #[test]
    fn admits_only_the_reviewed_source_normalizer_cronjob_identity() {
        let mut normalizer = row("traces", "normalizer", "success", 10);
        normalizer.service_name = Some("acme-api-source-normalizer".into());
        normalizer.workload = Some("acme-api-source-normalizer-29814011".into());
        assert!(is_application_row(&normalizer, &test_sources()));

        normalizer.workload = Some("acme-api-source-normalizer-manual".into());
        assert!(!is_application_row(&normalizer, &test_sources()));

        normalizer.workload = Some("acme-api-source-normalizer-29814011".into());
        normalizer.service_name = Some("unreviewed-service".into());
        assert!(!is_application_row(&normalizer, &test_sources()));
    }

    #[test]
    fn an_error_log_keeps_its_trace_bundle_and_routine_logs_follow_that_decision() {
        let trace_id = "d";
        let mut window = ApplicationWindow::with_period("", &test_sources());
        window.observe_logs(&[row("logs", trace_id, "error", 0)]);
        let root = row("traces", trace_id, "success", 10);
        let mut dependency = row("traces", trace_id, "success", 4);
        dependency.span_id = Some("dddddddddddddddd".into());
        dependency.parent_span_id = root.span_id.clone();
        dependency.name = Some("SELECT".into());
        dependency.attributes_json = json!({"db.system.name": "postgresql"}).to_string();
        window.observe_traces(&[root.clone(), dependency.clone()]);
        window.finish_decisions();

        let trace = window.retain(root).unwrap();
        let dependency = window.retain(dependency).unwrap();
        let log = window.retain(row("logs", trace_id, "error", 0)).unwrap();
        assert!(trace.attributes_json.contains("error_log"));
        assert!(dependency.attributes_json.contains("error_log"));
        assert!(log.attributes_json.contains("error_log"));
        let summary: Value =
            serde_json::from_str(window.summary_rows()[0].body_json.as_deref().unwrap()).unwrap();
        assert_eq!(summary["executions"], 1);
    }

    #[test]
    fn failures_and_slowest_successes_survive_but_ordinary_successes_are_bounded() {
        let mut window = ApplicationWindow::with_period("", &test_sources());
        let traces = (0..20)
            .map(|index| {
                row(
                    "traces",
                    &format!("{index:032x}"),
                    if index == 0 { "error" } else { "success" },
                    index,
                )
            })
            .collect::<Vec<_>>();
        window.observe_traces(&traces);
        window.finish_decisions();
        let kept = traces
            .into_iter()
            .filter_map(|row| window.retain(row))
            .collect::<Vec<_>>();
        assert!(
            kept.iter()
                .any(|row| row.attributes_json.contains("failure"))
        );
        assert_eq!(
            kept.iter()
                .filter(|row| row.attributes_json.contains("slow"))
                .count(),
            3
        );
        assert!(kept.len() <= 5);
    }

    #[test]
    fn healthy_candidates_cannot_starve_a_later_failure() {
        let mut traces = Vec::new();
        let mut candidate = 0_u64;
        while traces.len() < MAX_KEEP_DECISIONS {
            let trace_id = format!("{candidate:032x}");
            if healthy_sample(&trace_id) {
                traces.push(row("traces", &trace_id, "success", 1));
            }
            candidate += 1;
        }
        let failure_id = "ffffffffffffffffffffffffffffffff";
        traces.push(row("traces", failure_id, "error", 1));

        let mut window = ApplicationWindow::with_period("", &test_sources());
        window.observe_traces(&traces);
        window.finish_decisions();

        assert_eq!(window.decisions.get(failure_id), Some(&"failure"));
    }

    #[test]
    fn non_application_rows_stay_on_the_ordinary_compaction_path() {
        let mut metric = row("metrics", "", "success", 0);
        metric.service_namespace = None;
        assert!(!is_application_row(&metric, &test_sources()));
        assert!(is_application_row(
            &row("traces", "a", "success", 1),
            &test_sources()
        ));
    }

    #[test]
    fn unsampled_rows_do_not_grow_the_retained_dedupe_set() {
        let mut window = ApplicationWindow::with_period("", &test_sources());
        for index in 0..1_000 {
            let unsampled = row("traces", &format!("{index:032x}"), "success", 1);
            assert!(window.retain(unsampled).is_none());
        }
        assert!(window.retained_row_keys.is_empty());
    }

    #[test]
    fn application_marker_requires_an_onboarded_kubernetes_identity() {
        let application = row("traces", "a", "success", 1);
        assert!(is_application_row(&application, &test_sources()));

        let mut spoofed = application.clone();
        spoofed.service_name = Some("arbitrary-service".into());
        assert!(!is_application_row(&spoofed, &test_sources()));

        let mut wrong_namespace = application;
        wrong_namespace.k8s_namespace = Some("another-tenant".into());
        wrong_namespace.service_namespace = Some("another-tenant".into());
        assert!(!is_application_row(&wrong_namespace, &test_sources()));
    }

    #[test]
    fn dependency_noise_cannot_evict_the_root_and_truncation_is_explicit() {
        let trace_id = "bounded";
        let root = row("traces", trace_id, "success", 1);
        let dependencies = (0..(MAX_DEPENDENCY_SPANS_PER_TRACE + 10))
            .map(|index| {
                let mut dependency = root.clone();
                dependency.span_id = Some(format!("{index:016x}"));
                dependency.attributes_json = "{}".into();
                dependency
            })
            .collect::<Vec<_>>();
        let mut window = ApplicationWindow::with_period("", &test_sources());
        window.observe_traces(&dependencies);
        window.observe_traces(std::slice::from_ref(&root));
        window.finish_decisions();

        let retained_dependencies = dependencies
            .into_iter()
            .filter_map(|dependency| window.retain(dependency))
            .count();
        let retained_root = window.retain(root).unwrap();

        assert_eq!(retained_dependencies, MAX_DEPENDENCY_SPANS_PER_TRACE);
        assert_eq!(
            window.retained_row_keys.len(),
            MAX_DEPENDENCY_SPANS_PER_TRACE + 1
        );
        assert!(
            retained_root
                .attributes_json
                .contains("bundle_complete\":false")
        );
    }

    #[test]
    fn a_correlated_error_without_its_root_keeps_trace_identity_and_is_partial() {
        let trace_id = "orphan";
        let log = row("logs", trace_id, "error", 0);
        let mut window = ApplicationWindow::with_period("", &test_sources());
        window.observe_logs(std::slice::from_ref(&log));
        window.finish_decisions();

        let retained = window.retain(log).unwrap();
        assert_eq!(retained.trace_id.as_deref(), Some(trace_id));
        assert!(retained.attributes_json.contains("orphaned_error"));
        assert!(retained.attributes_json.contains("bundle_complete\":false"));
    }

    #[test]
    fn excess_selected_logs_and_standalone_errors_do_not_grow_dedupe_state() {
        let trace_id = "selected";
        let root = row("traces", trace_id, "error", 1);
        let mut window = ApplicationWindow::with_period("", &test_sources());
        window.observe_traces(std::slice::from_ref(&root));
        window.finish_decisions();

        let retained_logs = (0..(MAX_LOGS_PER_TRACE + 20))
            .filter_map(|index| {
                let mut log = row("logs", trace_id, "success", 0);
                log.timestamp_ns += index as i64;
                log.span_id = Some(format!("{index:016x}"));
                window.retain(log)
            })
            .count();
        assert_eq!(retained_logs, MAX_LOGS_PER_TRACE);
        assert_eq!(window.retained_row_keys.len(), MAX_LOGS_PER_TRACE);

        let retained_standalone = (0..(MAX_STANDALONE_ERRORS + 20))
            .filter_map(|index| {
                let mut log = row("logs", "", "error", 0);
                log.trace_id = None;
                log.span_id = None;
                log.timestamp_ns += index as i64;
                window.retain(log)
            })
            .count();
        assert_eq!(retained_standalone, MAX_STANDALONE_ERRORS);
        assert_eq!(
            window.retained_row_keys.len(),
            MAX_LOGS_PER_TRACE + MAX_STANDALONE_ERRORS
        );
    }

    #[test]
    fn oversized_retained_fields_are_bounded_and_mark_the_service_partial() {
        let mut execution = row("traces", "large", "error", 1);
        execution.name = Some("n".repeat(MAX_RETAINED_FIELD_BYTES + 1));
        execution.attributes_json = json!({
            "infra.operation.kind": "k".repeat(MAX_RETAINED_FIELD_BYTES + 1),
            "infra.execution.outcome": "error"
        })
        .to_string();
        execution.scope_schema_url = Some("s".repeat(MAX_RETAINED_FIELD_BYTES + 1));
        execution.resource_schema_url = Some("r".repeat(MAX_RETAINED_FIELD_BYTES + 1));
        execution.body_json = Some(json!("x".repeat(MAX_RETAINED_FIELD_BYTES + 1)).to_string());
        let mut log = row("logs", "large", "error", 0);
        log.body_json = Some(json!("x".repeat(MAX_RETAINED_FIELD_BYTES + 1)).to_string());
        let mut window = ApplicationWindow::with_period("", &test_sources());
        window.observe_logs(std::slice::from_ref(&log));
        window.observe_traces(std::slice::from_ref(&execution));
        window.finish_decisions();

        let retained = window.retain(log).unwrap();
        let retained_execution = window.retain(execution).unwrap();
        assert_eq!(retained.body_json.as_deref(), Some("\"[truncated]\""));
        assert!(retained.attributes_json.contains("bundle_complete\":false"));
        assert_eq!(
            retained_execution.name.as_ref().unwrap().len(),
            MAX_RETAINED_SCALAR_BYTES
        );
        assert!(retained_execution.scope_schema_url.is_none());
        assert!(retained_execution.resource_schema_url.is_none());
        let body: Value =
            serde_json::from_str(window.summary_rows()[0].body_json.as_deref().unwrap()).unwrap();
        assert_eq!(
            body["operation"].as_str().unwrap().len(),
            MAX_RETAINED_SCALAR_BYTES
        );
        assert_eq!(
            body["operationKind"].as_str().unwrap().len(),
            MAX_RETAINED_SCALAR_BYTES
        );
        assert_eq!(body["telemetryLossObserved"], true);
    }

    #[test]
    fn producer_drop_evidence_marks_the_complete_summary_partial() {
        let mut execution = row("traces", "drop", "success", 1);
        execution.attributes_json = json!({
            "infra.operation.kind": "http",
            "infra.execution.outcome": "success",
            "telemetry.sdk.dropped.records": 2
        })
        .to_string();
        let mut window = ApplicationWindow::with_period("", &test_sources());
        window.observe_traces(&[execution]);

        let body: Value =
            serde_json::from_str(window.summary_rows()[0].body_json.as_deref().unwrap()).unwrap();
        assert_eq!(body["telemetryLossObserved"], true);
    }
}
