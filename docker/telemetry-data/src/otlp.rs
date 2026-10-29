use std::collections::BTreeMap;

use anyhow::{Context, Result, bail};
use serde_json::{Map, Value};

use crate::model::TelemetryRow;

pub fn flatten(signal: &str, payload: &[u8]) -> Result<Vec<TelemetryRow>> {
    let root: Value = serde_json::from_slice(payload).context("decode OTLP JSON")?;
    match signal {
        "logs" => flatten_logs(&root),
        "traces" => flatten_traces(&root),
        "metrics" => flatten_metrics(&root),
        other => bail!("unsupported signal {other}"),
    }
}

fn flatten_logs(root: &Value) -> Result<Vec<TelemetryRow>> {
    let mut rows = Vec::new();
    for resource_group in array(root, "resourceLogs") {
        let resource = attributes(resource_group.pointer("/resource/attributes"));
        let resource_schema_url = string(resource_group, "schemaUrl");
        for scope_group in array(resource_group, "scopeLogs") {
            let scope = scope_group
                .get("scope")
                .cloned()
                .unwrap_or_else(empty_object);
            let scope_schema_url = string(scope_group, "schemaUrl");
            for record in array(scope_group, "logRecords") {
                let attrs = attributes(record.get("attributes"));
                rows.push(base_row(
                    "logs",
                    number(record, "timeUnixNano")
                        .or_else(|| number(record, "observedTimeUnixNano"))
                        .unwrap_or(0),
                    &resource,
                    &attrs,
                    &scope,
                    scope_schema_url.clone(),
                    resource_schema_url.clone(),
                    TelemetrySpecific {
                        observed_timestamp_ns: number(record, "observedTimeUnixNano"),
                        severity_number: integer(record, "severityNumber").map(|n| n as i32),
                        severity_text: string(record, "severityText"),
                        trace_id: string(record, "traceId"),
                        span_id: string(record, "spanId"),
                        flags: integer(record, "flags"),
                        dropped_attributes_count: integer(record, "droppedAttributesCount"),
                        body_json: record.get("body").map(any_value).map(json_string),
                        ..Default::default()
                    },
                ));
            }
        }
    }
    Ok(rows)
}

fn flatten_traces(root: &Value) -> Result<Vec<TelemetryRow>> {
    let mut rows = Vec::new();
    for resource_group in array(root, "resourceSpans") {
        let resource = attributes(resource_group.pointer("/resource/attributes"));
        let resource_schema_url = string(resource_group, "schemaUrl");
        for scope_group in array(resource_group, "scopeSpans") {
            let scope = scope_group
                .get("scope")
                .cloned()
                .unwrap_or_else(empty_object);
            let scope_schema_url = string(scope_group, "schemaUrl");
            for span in array(scope_group, "spans") {
                let attrs = attributes(span.get("attributes"));
                rows.push(base_row(
                    "traces",
                    number(span, "startTimeUnixNano").unwrap_or(0),
                    &resource,
                    &attrs,
                    &scope,
                    scope_schema_url.clone(),
                    resource_schema_url.clone(),
                    TelemetrySpecific {
                        start_timestamp_ns: number(span, "startTimeUnixNano"),
                        end_timestamp_ns: number(span, "endTimeUnixNano"),
                        name: string(span, "name"),
                        kind: string_or_number(span, "kind"),
                        trace_id: string(span, "traceId"),
                        span_id: string(span, "spanId"),
                        parent_span_id: string(span, "parentSpanId"),
                        status_code: span
                            .pointer("/status/code")
                            .and_then(value_string_or_number),
                        status_message: span
                            .pointer("/status/message")
                            .and_then(value_string_or_number),
                        trace_state: string(span, "traceState"),
                        flags: integer(span, "flags"),
                        events_json: span.get("events").cloned().map(json_string),
                        links_json: span.get("links").cloned().map(json_string),
                        dropped_attributes_count: integer(span, "droppedAttributesCount"),
                        dropped_events_count: integer(span, "droppedEventsCount"),
                        dropped_links_count: integer(span, "droppedLinksCount"),
                        ..Default::default()
                    },
                ));
            }
        }
    }
    Ok(rows)
}

fn flatten_metrics(root: &Value) -> Result<Vec<TelemetryRow>> {
    let mut rows = Vec::new();
    for resource_group in array(root, "resourceMetrics") {
        let resource = attributes(resource_group.pointer("/resource/attributes"));
        let resource_schema_url = string(resource_group, "schemaUrl");
        for scope_group in array(resource_group, "scopeMetrics") {
            let scope = scope_group
                .get("scope")
                .cloned()
                .unwrap_or_else(empty_object);
            let scope_schema_url = string(scope_group, "schemaUrl");
            for metric in array(scope_group, "metrics") {
                let metric_name = string(metric, "name");
                let metric_description = string(metric, "description");
                let metric_unit = string(metric, "unit");
                for (kind, container) in [
                    ("gauge", "gauge"),
                    ("sum", "sum"),
                    ("histogram", "histogram"),
                    ("exponential_histogram", "exponentialHistogram"),
                    ("summary", "summary"),
                ] {
                    let Some(metric_data) = metric.get(container) else {
                        continue;
                    };
                    let aggregation_temporality =
                        integer(metric_data, "aggregationTemporality").map(|value| value as i32);
                    let is_monotonic = metric_data.get("isMonotonic").and_then(Value::as_bool);
                    for point in metric_data
                        .get("dataPoints")
                        .and_then(Value::as_array)
                        .into_iter()
                        .flatten()
                    {
                        let attrs = attributes(point.get("attributes"));
                        rows.push(base_row(
                            "metrics",
                            number(point, "timeUnixNano").unwrap_or(0),
                            &resource,
                            &attrs,
                            &scope,
                            scope_schema_url.clone(),
                            resource_schema_url.clone(),
                            TelemetrySpecific {
                                start_timestamp_ns: number(point, "startTimeUnixNano"),
                                name: metric_name.clone(),
                                kind: Some(kind.to_owned()),
                                flags: integer(point, "flags"),
                                dropped_attributes_count: integer(point, "droppedAttributesCount"),
                                metric_description: metric_description.clone(),
                                metric_unit: metric_unit.clone(),
                                aggregation_temporality,
                                is_monotonic,
                                body_json: Some(json_string(point.clone())),
                                ..Default::default()
                            },
                        ));
                    }
                }
            }
        }
    }
    Ok(rows)
}

#[derive(Default)]
struct TelemetrySpecific {
    start_timestamp_ns: Option<i64>,
    observed_timestamp_ns: Option<i64>,
    end_timestamp_ns: Option<i64>,
    name: Option<String>,
    kind: Option<String>,
    severity_number: Option<i32>,
    severity_text: Option<String>,
    trace_id: Option<String>,
    span_id: Option<String>,
    parent_span_id: Option<String>,
    status_code: Option<String>,
    status_message: Option<String>,
    trace_state: Option<String>,
    flags: Option<i64>,
    events_json: Option<String>,
    links_json: Option<String>,
    dropped_attributes_count: Option<i64>,
    dropped_events_count: Option<i64>,
    dropped_links_count: Option<i64>,
    metric_description: Option<String>,
    metric_unit: Option<String>,
    aggregation_temporality: Option<i32>,
    is_monotonic: Option<bool>,
    body_json: Option<String>,
}

fn base_row(
    signal: &str,
    timestamp_ns: i64,
    resource: &BTreeMap<String, Value>,
    attrs: &BTreeMap<String, Value>,
    scope: &Value,
    scope_schema_url: Option<String>,
    resource_schema_url: Option<String>,
    specific: TelemetrySpecific,
) -> TelemetryRow {
    TelemetryRow {
        signal: signal.to_owned(),
        timestamp_ns,
        start_timestamp_ns: specific.start_timestamp_ns,
        observed_timestamp_ns: specific.observed_timestamp_ns,
        end_timestamp_ns: specific.end_timestamp_ns,
        name: specific.name,
        kind: specific.kind,
        severity_number: specific.severity_number,
        severity_text: specific.severity_text,
        trace_id: specific.trace_id,
        span_id: specific.span_id,
        parent_span_id: specific.parent_span_id,
        status_code: specific.status_code,
        status_message: specific.status_message,
        trace_state: specific.trace_state,
        flags: specific.flags,
        events_json: specific.events_json,
        links_json: specific.links_json,
        dropped_attributes_count: specific.dropped_attributes_count,
        dropped_events_count: specific.dropped_events_count,
        dropped_links_count: specific.dropped_links_count,
        metric_description: specific.metric_description,
        metric_unit: specific.metric_unit,
        aggregation_temporality: specific.aggregation_temporality,
        is_monotonic: specific.is_monotonic,
        scope_json: json_string(scope.clone()),
        scope_schema_url,
        resource_schema_url,
        service_name: attr_string(resource, "service.name"),
        service_namespace: attr_string(resource, "service.namespace"),
        k8s_namespace: attr_string(resource, "k8s.namespace.name"),
        workload: workload(resource),
        node: attr_string(resource, "k8s.node.name")
            .or_else(|| attr_string(resource, "collector.node.name")),
        body_json: specific.body_json,
        attributes_json: json_string(map_value(attrs)),
        resource_json: json_string(map_value(resource)),
    }
}

fn workload(resource: &BTreeMap<String, Value>) -> Option<String> {
    for key in [
        "k8s.deployment.name",
        "k8s.statefulset.name",
        "k8s.daemonset.name",
        "k8s.job.name",
        "k8s.cronjob.name",
        "k8s.pod.name",
    ] {
        if let Some(value) = attr_string(resource, key) {
            return Some(value);
        }
    }
    None
}

fn attributes(value: Option<&Value>) -> BTreeMap<String, Value> {
    let mut out = BTreeMap::new();
    for item in value.and_then(Value::as_array).into_iter().flatten() {
        if let Some(key) = item.get("key").and_then(Value::as_str) {
            out.insert(
                key.to_owned(),
                item.get("value").map(any_value).unwrap_or(Value::Null),
            );
        }
    }
    out
}

fn any_value(value: &Value) -> Value {
    let Some(obj) = value.as_object() else {
        return value.clone();
    };
    for key in [
        "stringValue",
        "boolValue",
        "intValue",
        "doubleValue",
        "bytesValue",
    ] {
        if let Some(v) = obj.get(key) {
            return v.clone();
        }
    }
    if let Some(values) = obj
        .get("arrayValue")
        .and_then(|value| value.get("values"))
        .and_then(Value::as_array)
    {
        return Value::Array(values.iter().map(any_value).collect());
    }
    if let Some(values) = obj.get("kvlistValue").and_then(|value| value.get("values")) {
        return map_value(&attributes(Some(values)));
    }
    value.clone()
}

fn attr_string(attrs: &BTreeMap<String, Value>, key: &str) -> Option<String> {
    attrs.get(key).and_then(value_string_or_number)
}

fn array<'a>(value: &'a Value, key: &str) -> &'a [Value] {
    value
        .get(key)
        .and_then(Value::as_array)
        .map(Vec::as_slice)
        .unwrap_or_default()
}

fn number(value: &Value, key: &str) -> Option<i64> {
    value.get(key).and_then(|v| match v {
        Value::String(s) => s.parse::<u64>().ok().map(|n| n.min(i64::MAX as u64) as i64),
        Value::Number(n) => n.as_u64().map(|n| n.min(i64::MAX as u64) as i64),
        _ => None,
    })
}

fn integer(value: &Value, key: &str) -> Option<i64> {
    value.get(key).and_then(|v| match v {
        Value::String(s) => s.parse().ok(),
        Value::Number(n) => n.as_i64(),
        _ => None,
    })
}

fn string(value: &Value, key: &str) -> Option<String> {
    value.get(key).and_then(value_string_or_number)
}

fn string_or_number(value: &Value, key: &str) -> Option<String> {
    string(value, key)
}

fn value_string_or_number(value: &Value) -> Option<String> {
    match value {
        Value::String(s) => Some(s.clone()),
        Value::Number(n) => Some(n.to_string()),
        Value::Bool(v) => Some(v.to_string()),
        _ => None,
    }
}

fn map_value(values: &BTreeMap<String, Value>) -> Value {
    Value::Object(
        values
            .iter()
            .map(|(key, value)| (key.clone(), value.clone()))
            .collect::<Map<String, Value>>(),
    )
}

fn json_string(value: Value) -> String {
    serde_json::to_string(&value).expect("JSON values always serialize")
}

fn empty_object() -> Value {
    Value::Object(Map::new())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn flattens_correlated_log() {
        let payload = br#"{
          "resourceLogs": [{
            "resource": {"attributes": [
              {"key":"service.name","value":{"stringValue":"api"}},
              {"key":"k8s.namespace.name","value":{"stringValue":"acme"}},
              {"key":"k8s.deployment.name","value":{"stringValue":"acme-api"}}
            ]},
            "scopeLogs": [{"logRecords": [{
              "timeUnixNano":"100", "severityNumber":17, "severityText":"ERROR",
              "traceId":"abc", "spanId":"def", "body":{"stringValue":"failed"},
              "attributes":[{"key":"error.type","value":{"stringValue":"timeout"}}]
            }]}]
          }]
        }"#;
        let rows = flatten("logs", payload).unwrap();
        assert_eq!(rows.len(), 1);
        assert_eq!(rows[0].service_name.as_deref(), Some("api"));
        assert_eq!(rows[0].workload.as_deref(), Some("acme-api"));
        assert_eq!(rows[0].severity_number, Some(17));
        assert_eq!(rows[0].trace_id.as_deref(), Some("abc"));
        assert_eq!(rows[0].body_json.as_deref(), Some("\"failed\""));
    }

    #[test]
    fn flattens_span_and_metric() {
        let traces = br#"{"resourceSpans":[{"schemaUrl":"resource-v1","resource":{"attributes":[]},"scopeSpans":[{"schemaUrl":"scope-v1","scope":{"name":"http","version":"1.2"},"spans":[{"traceId":"t","spanId":"s","name":"GET /","startTimeUnixNano":"10","endTimeUnixNano":"30","traceState":"vendor=x","flags":1,"status":{"code":2,"message":"failed"},"events":[{"timeUnixNano":"20","name":"exception"}],"links":[{"traceId":"other","spanId":"linked"}],"droppedAttributesCount":1,"droppedEventsCount":2,"droppedLinksCount":3}]}]}]}"#;
        let spans = flatten("traces", traces).unwrap();
        assert_eq!(spans[0].name.as_deref(), Some("GET /"));
        assert_eq!(spans[0].end_timestamp_ns, Some(30));
        assert_eq!(spans[0].status_message.as_deref(), Some("failed"));
        assert_eq!(spans[0].trace_state.as_deref(), Some("vendor=x"));
        assert!(
            spans[0]
                .events_json
                .as_deref()
                .unwrap()
                .contains("exception")
        );
        assert!(spans[0].links_json.as_deref().unwrap().contains("linked"));
        assert_eq!(spans[0].scope_schema_url.as_deref(), Some("scope-v1"));
        assert_eq!(spans[0].resource_schema_url.as_deref(), Some("resource-v1"));

        let metrics = br#"{"resourceMetrics":[{"resource":{"attributes":[]},"scopeMetrics":[{"scope":{"name":"meter"},"metrics":[{"name":"requests","description":"completed requests","unit":"{request}","sum":{"aggregationTemporality":1,"isMonotonic":true,"dataPoints":[{"startTimeUnixNano":"40","timeUnixNano":"50","asInt":"2"}]}}]}]}]}"#;
        let points = flatten("metrics", metrics).unwrap();
        assert_eq!(points.len(), 1);
        assert_eq!(points[0].name.as_deref(), Some("requests"));
        assert_eq!(points[0].kind.as_deref(), Some("sum"));
        assert_eq!(points[0].start_timestamp_ns, Some(40));
        assert_eq!(points[0].metric_unit.as_deref(), Some("{request}"));
        assert_eq!(points[0].aggregation_temporality, Some(1));
        assert_eq!(points[0].is_monotonic, Some(true));
    }
}
