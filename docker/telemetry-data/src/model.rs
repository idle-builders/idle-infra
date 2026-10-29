use serde::{Deserialize, Serialize};

pub const SCHEMA_VERSION: u32 = 1;
pub const POLICY_VERSION: &str = "o1-initial-v1";

#[derive(Clone, Debug, PartialEq)]
pub struct TelemetryRow {
    pub signal: String,
    pub timestamp_ns: i64,
    pub start_timestamp_ns: Option<i64>,
    pub observed_timestamp_ns: Option<i64>,
    pub end_timestamp_ns: Option<i64>,
    pub name: Option<String>,
    pub kind: Option<String>,
    pub severity_number: Option<i32>,
    pub severity_text: Option<String>,
    pub trace_id: Option<String>,
    pub span_id: Option<String>,
    pub parent_span_id: Option<String>,
    pub status_code: Option<String>,
    pub status_message: Option<String>,
    pub trace_state: Option<String>,
    pub flags: Option<i64>,
    pub events_json: Option<String>,
    pub links_json: Option<String>,
    pub dropped_attributes_count: Option<i64>,
    pub dropped_events_count: Option<i64>,
    pub dropped_links_count: Option<i64>,
    pub metric_description: Option<String>,
    pub metric_unit: Option<String>,
    pub aggregation_temporality: Option<i32>,
    pub is_monotonic: Option<bool>,
    pub scope_json: String,
    pub scope_schema_url: Option<String>,
    pub resource_schema_url: Option<String>,
    pub service_name: Option<String>,
    pub service_namespace: Option<String>,
    pub k8s_namespace: Option<String>,
    pub workload: Option<String>,
    pub node: Option<String>,
    pub body_json: Option<String>,
    pub attributes_json: String,
    pub resource_json: String,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct SourceObject {
    pub key: String,
    pub size: u64,
    pub etag: Option<String>,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct OutputObject {
    pub key: String,
    pub rows: usize,
    pub bytes: usize,
    pub sha256: String,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct Manifest {
    pub schema_version: u32,
    pub policy_version: String,
    pub signal: String,
    pub period: String,
    pub generation: u64,
    pub updated_at: String,
    pub source_objects: Vec<SourceObject>,
    pub outputs: Vec<OutputObject>,
    pub total_rows: usize,
}

impl Manifest {
    pub fn empty(signal: &str, period: &str) -> Self {
        Self {
            schema_version: SCHEMA_VERSION,
            policy_version: POLICY_VERSION.to_owned(),
            signal: signal.to_owned(),
            period: period.to_owned(),
            generation: 0,
            updated_at: String::new(),
            source_objects: Vec::new(),
            outputs: Vec::new(),
            total_rows: 0,
        }
    }
}

#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct RunReport {
    pub schema_version: u32,
    pub run_id: String,
    pub started_at: String,
    pub completed_at: String,
    pub source_objects: usize,
    pub source_bytes: u64,
    pub output_objects: usize,
    pub output_bytes: usize,
    pub rows: usize,
    pub deleted_raw_objects: usize,
    pub deleted_raw_bytes: u64,
    pub periods: Vec<String>,
}
