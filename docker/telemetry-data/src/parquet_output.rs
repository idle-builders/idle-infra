use std::sync::Arc;

use anyhow::{Context, Result, ensure};
use arrow_array::{
    Array, ArrayRef, BooleanArray, Int32Array, Int64Array, RecordBatch, StringArray,
};
use arrow_schema::{DataType, Field, Schema};
use bytes::Bytes;
use parquet::arrow::ArrowWriter;
use parquet::arrow::arrow_reader::ParquetRecordBatchReaderBuilder;
use parquet::basic::{Compression, ZstdLevel};
use parquet::file::properties::WriterProperties;

use crate::model::TelemetryRow;
use crate::query::QueryRow;

pub fn encode(rows: &[TelemetryRow]) -> Result<Vec<u8>> {
    ensure!(!rows.is_empty(), "refusing to write empty parquet output");
    let schema = schema();
    let props = WriterProperties::builder()
        .set_compression(Compression::ZSTD(ZstdLevel::default()))
        .set_write_batch_size(512)
        .set_max_row_group_row_count(Some(4096))
        .set_max_row_group_bytes(Some(4 * 1024 * 1024))
        .build();
    let mut writer = ArrowWriter::try_new(Vec::new(), schema.clone(), Some(props))
        .context("create Parquet writer")?;

    // Arrow and Parquet are batch-streaming formats. Keep each in-memory Arrow
    // batch small and let the writer flush bounded row groups instead of
    // materializing one record batch for the complete source object.
    for rows in rows.chunks(512) {
        writer
            .write(&record_batch(schema.clone(), rows)?)
            .context("write Parquet batch")?;
    }
    let bytes = writer.into_inner().context("close Parquet writer")?;
    validate(&bytes, rows.len())?;
    Ok(bytes)
}

fn record_batch(schema: Arc<Schema>, rows: &[TelemetryRow]) -> Result<RecordBatch> {
    RecordBatch::try_new(
        schema,
        vec![
            strings(rows.iter().map(|r| Some(r.signal.as_str()))),
            Arc::new(Int64Array::from_iter_values(
                rows.iter().map(|r| r.timestamp_ns),
            )),
            Arc::new(Int64Array::from_iter(
                rows.iter().map(|r| r.start_timestamp_ns),
            )),
            Arc::new(Int64Array::from_iter(
                rows.iter().map(|r| r.observed_timestamp_ns),
            )),
            Arc::new(Int64Array::from_iter(
                rows.iter().map(|r| r.end_timestamp_ns),
            )),
            strings(rows.iter().map(|r| r.name.as_deref())),
            strings(rows.iter().map(|r| r.kind.as_deref())),
            Arc::new(Int32Array::from_iter(
                rows.iter().map(|r| r.severity_number),
            )),
            strings(rows.iter().map(|r| r.severity_text.as_deref())),
            strings(rows.iter().map(|r| r.trace_id.as_deref())),
            strings(rows.iter().map(|r| r.span_id.as_deref())),
            strings(rows.iter().map(|r| r.parent_span_id.as_deref())),
            strings(rows.iter().map(|r| r.status_code.as_deref())),
            strings(rows.iter().map(|r| r.status_message.as_deref())),
            strings(rows.iter().map(|r| r.trace_state.as_deref())),
            Arc::new(Int64Array::from_iter(rows.iter().map(|r| r.flags))),
            strings(rows.iter().map(|r| r.events_json.as_deref())),
            strings(rows.iter().map(|r| r.links_json.as_deref())),
            Arc::new(Int64Array::from_iter(
                rows.iter().map(|r| r.dropped_attributes_count),
            )),
            Arc::new(Int64Array::from_iter(
                rows.iter().map(|r| r.dropped_events_count),
            )),
            Arc::new(Int64Array::from_iter(
                rows.iter().map(|r| r.dropped_links_count),
            )),
            strings(rows.iter().map(|r| r.metric_description.as_deref())),
            strings(rows.iter().map(|r| r.metric_unit.as_deref())),
            Arc::new(Int32Array::from_iter(
                rows.iter().map(|r| r.aggregation_temporality),
            )),
            Arc::new(BooleanArray::from_iter(rows.iter().map(|r| r.is_monotonic))),
            strings(rows.iter().map(|r| Some(r.scope_json.as_str()))),
            strings(rows.iter().map(|r| r.scope_schema_url.as_deref())),
            strings(rows.iter().map(|r| r.resource_schema_url.as_deref())),
            strings(rows.iter().map(|r| r.service_name.as_deref())),
            strings(rows.iter().map(|r| r.service_namespace.as_deref())),
            strings(rows.iter().map(|r| r.k8s_namespace.as_deref())),
            strings(rows.iter().map(|r| r.workload.as_deref())),
            strings(rows.iter().map(|r| r.node.as_deref())),
            strings(rows.iter().map(|r| r.body_json.as_deref())),
            strings(rows.iter().map(|r| Some(r.attributes_json.as_str()))),
            strings(rows.iter().map(|r| Some(r.resource_json.as_str()))),
        ],
    )
    .context("build Arrow record batch")
}

pub fn validate(bytes: &[u8], expected_rows: usize) -> Result<()> {
    let builder = ParquetRecordBatchReaderBuilder::try_new(Bytes::copy_from_slice(bytes))
        .context("reopen Parquet output")?;
    let actual = builder.metadata().file_metadata().num_rows() as usize;
    ensure!(
        actual == expected_rows,
        "Parquet row count mismatch: expected {expected_rows}, got {actual}"
    );
    Ok(())
}

// Visit query-relevant columns one bounded record batch at a time. The
// predefined dashboard query does not need DataFusion or an in-memory table:
// Parquet already supplies the columnar batches and the aggregation is tiny.
pub fn visit_query_rows(bytes: &[u8], mut visit: impl FnMut(QueryRow)) -> Result<()> {
    let mut reader = ParquetRecordBatchReaderBuilder::try_new(Bytes::copy_from_slice(bytes))
        .context("open Parquet query input")?
        .with_batch_size(512)
        .build()
        .context("build Parquet query reader")?;
    for batch in &mut reader {
        let batch = batch.context("read Parquet query batch")?;
        let strings = |column: usize| {
            batch
                .column(column)
                .as_any()
                .downcast_ref::<StringArray>()
                .expect("query column schema must match curated/v1")
        };
        let signal = strings(0);
        let timestamp = batch
            .column(1)
            .as_any()
            .downcast_ref::<Int64Array>()
            .expect("timestamp_ns must be int64");
        let start = batch
            .column(2)
            .as_any()
            .downcast_ref::<Int64Array>()
            .expect("start_timestamp_ns must be int64");
        let end = batch
            .column(4)
            .as_any()
            .downcast_ref::<Int64Array>()
            .expect("end_timestamp_ns must be int64");
        let name = strings(5);
        let severity = batch
            .column(7)
            .as_any()
            .downcast_ref::<Int32Array>()
            .expect("severity_number must be int32");
        let severity_text = strings(8);
        let trace_id = strings(9);
        let span_id = strings(10);
        let parent_span_id = strings(11);
        let status_code = strings(12);
        let service_name = strings(28);
        let service_namespace = strings(29);
        let namespace = strings(30);
        let workload = strings(31);
        let node = strings(32);
        let body = strings(33);
        let attributes = strings(34);
        let resource = strings(35);

        for row in 0..batch.num_rows() {
            visit(QueryRow {
                signal: signal.value(row).to_owned(),
                timestamp_ns: timestamp.value(row),
                start_timestamp_ns: int64_at(start, row),
                end_timestamp_ns: int64_at(end, row),
                name: string_at(name, row),
                severity_number: int32_at(severity, row),
                severity_text: string_at(severity_text, row),
                trace_id: string_at(trace_id, row),
                span_id: string_at(span_id, row),
                parent_span_id: string_at(parent_span_id, row),
                status_code: string_at(status_code, row),
                service_name: string_at(service_name, row),
                service_namespace: string_at(service_namespace, row),
                k8s_namespace: string_at(namespace, row),
                workload: string_at(workload, row),
                node: string_at(node, row),
                body_json: string_at(body, row),
                attributes_json: attributes.value(row).to_owned(),
                resource_json: resource.value(row).to_owned(),
            });
        }
    }
    Ok(())
}

fn int64_at(array: &Int64Array, row: usize) -> Option<i64> {
    (!array.is_null(row)).then(|| array.value(row))
}

fn int32_at(array: &Int32Array, row: usize) -> Option<i32> {
    (!array.is_null(row)).then(|| array.value(row))
}

fn string_at(array: &StringArray, row: usize) -> Option<String> {
    (!array.is_null(row)).then(|| array.value(row).to_owned())
}

fn schema() -> Arc<Schema> {
    Arc::new(Schema::new(vec![
        Field::new("signal", DataType::Utf8, false),
        Field::new("timestamp_ns", DataType::Int64, false),
        Field::new("start_timestamp_ns", DataType::Int64, true),
        Field::new("observed_timestamp_ns", DataType::Int64, true),
        Field::new("end_timestamp_ns", DataType::Int64, true),
        Field::new("name", DataType::Utf8, true),
        Field::new("kind", DataType::Utf8, true),
        Field::new("severity_number", DataType::Int32, true),
        Field::new("severity_text", DataType::Utf8, true),
        Field::new("trace_id", DataType::Utf8, true),
        Field::new("span_id", DataType::Utf8, true),
        Field::new("parent_span_id", DataType::Utf8, true),
        Field::new("status_code", DataType::Utf8, true),
        Field::new("status_message", DataType::Utf8, true),
        Field::new("trace_state", DataType::Utf8, true),
        Field::new("flags", DataType::Int64, true),
        Field::new("events_json", DataType::Utf8, true),
        Field::new("links_json", DataType::Utf8, true),
        Field::new("dropped_attributes_count", DataType::Int64, true),
        Field::new("dropped_events_count", DataType::Int64, true),
        Field::new("dropped_links_count", DataType::Int64, true),
        Field::new("metric_description", DataType::Utf8, true),
        Field::new("metric_unit", DataType::Utf8, true),
        Field::new("aggregation_temporality", DataType::Int32, true),
        Field::new("is_monotonic", DataType::Boolean, true),
        Field::new("scope_json", DataType::Utf8, false),
        Field::new("scope_schema_url", DataType::Utf8, true),
        Field::new("resource_schema_url", DataType::Utf8, true),
        Field::new("service_name", DataType::Utf8, true),
        Field::new("service_namespace", DataType::Utf8, true),
        Field::new("k8s_namespace", DataType::Utf8, true),
        Field::new("workload", DataType::Utf8, true),
        Field::new("node", DataType::Utf8, true),
        Field::new("body_json", DataType::Utf8, true),
        Field::new("attributes_json", DataType::Utf8, false),
        Field::new("resource_json", DataType::Utf8, false),
    ]))
}

fn strings<'a>(values: impl Iterator<Item = Option<&'a str>>) -> ArrayRef {
    Arc::new(StringArray::from_iter(values))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parquet_round_trip_validates() {
        let row = TelemetryRow {
            signal: "logs".into(),
            timestamp_ns: 1,
            start_timestamp_ns: None,
            observed_timestamp_ns: None,
            end_timestamp_ns: None,
            name: None,
            kind: None,
            severity_number: Some(9),
            severity_text: Some("INFO".into()),
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
            metric_description: None,
            metric_unit: None,
            aggregation_temporality: None,
            is_monotonic: None,
            scope_json: "{}".into(),
            scope_schema_url: None,
            resource_schema_url: None,
            service_name: Some("test".into()),
            service_namespace: None,
            k8s_namespace: Some("infra".into()),
            workload: Some("test".into()),
            node: Some("node-a".into()),
            body_json: Some("\"hello\"".into()),
            attributes_json: "{}".into(),
            resource_json: "{}".into(),
        };
        let bytes = encode(&[row]).unwrap();
        assert!(bytes.starts_with(b"PAR1"));
        validate(&bytes, 1).unwrap();
    }
}
