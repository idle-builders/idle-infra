mod application;
mod model;
mod otlp;
mod parquet_output;
mod query;

use std::collections::{BTreeMap, HashSet};
use std::env;
use std::io::Read;
use std::sync::Arc;
use std::time::Duration;

use anyhow::{Context, Result, bail, ensure};
use bytes::Bytes;
use chrono::{DateTime, Utc};
use flate2::read::GzDecoder;
use futures::TryStreamExt;
use object_store::aws::AmazonS3Builder;
use object_store::path::Path;
use object_store::{ObjectMeta, ObjectStore, ObjectStoreExt};
use sha2::{Digest, Sha256};
use uuid::Uuid;

use crate::model::{Manifest, OutputObject, RunReport, SCHEMA_VERSION, SourceObject};

const SIGNALS: [&str; 3] = ["logs", "metrics", "traces"];
const MAX_DECODED_OBJECT_BYTES: u64 = 64 * 1024 * 1024;
const MAX_OUTPUT_BYTES: usize = 16 * 1024 * 1024;

#[tokio::main]
async fn main() -> Result<()> {
    let command = env::args().nth(1).unwrap_or_else(|| "compact".to_owned());
    let store = s3_store()?;
    if command == "query" {
        let result = query::run(store, query::QueryParams::from_env()?).await?;
        println!("{}", serde_json::to_string(&result)?);
        return Ok(());
    }
    if command != "compact" {
        bail!("unsupported command {command}; expected compact or query");
    }
    let seal_delay = Duration::from_secs(env_u64("COMPACTION_SEAL_DELAY_SECONDS", 120)?);
    let raw_retention = Duration::from_secs(env_u64("RAW_RETENTION_SECONDS", 7200)?);
    let report = compact(store, seal_delay, raw_retention).await?;
    println!("{}", serde_json::to_string(&report)?);
    Ok(())
}

fn s3_store() -> Result<Arc<dyn ObjectStore>> {
    let endpoint = required_env("S3_ENDPOINT")?;
    let allow_http = endpoint.starts_with("http://");
    let store = AmazonS3Builder::new()
        .with_endpoint(endpoint)
        .with_region(env::var("S3_REGION").unwrap_or_else(|_| "us-east-1".to_owned()))
        .with_bucket_name(required_env("S3_BUCKET")?)
        .with_access_key_id(required_env("S3_ACCESS_KEY_ID")?)
        .with_secret_access_key(required_env("S3_SECRET_ACCESS_KEY")?)
        .with_virtual_hosted_style_request(false)
        .with_allow_http(allow_http)
        .build()
        .context("configure S3 object store")?;
    Ok(Arc::new(store))
}

async fn compact(
    store: Arc<dyn ObjectStore>,
    seal_delay: Duration,
    raw_retention: Duration,
) -> Result<RunReport> {
    let started = Utc::now();
    let run_id = Uuid::new_v4().simple().to_string();
    let mut report = RunReport {
        schema_version: SCHEMA_VERSION,
        run_id: run_id.clone(),
        started_at: started.to_rfc3339(),
        completed_at: String::new(),
        source_objects: 0,
        source_bytes: 0,
        output_objects: 0,
        output_bytes: 0,
        rows: 0,
        deleted_raw_objects: 0,
        deleted_raw_bytes: 0,
        periods: Vec::new(),
    };

    // Application summaries and sampled execution bundles must be durable
    // before the ordinary signal path can make raw objects cleanup-eligible.
    compact_applications(store.as_ref(), started, seal_delay, &run_id, &mut report).await?;

    for signal in SIGNALS {
        let prefix = Path::from(format!("raw/v1/{signal}"));
        let objects = store
            .list(Some(&prefix))
            .try_collect::<Vec<_>>()
            .await
            .with_context(|| format!("list raw {signal} objects"))?;

        let mut periods: BTreeMap<String, Vec<ObjectMeta>> = BTreeMap::new();
        for object in objects {
            if !is_sealed(&object, started, seal_delay) {
                continue;
            }
            if let Some(period) = period_from_key(object.location.as_ref()) {
                periods.entry(period).or_default().push(object);
            }
        }

        for (period, objects) in periods {
            let manifest_path = Path::from(format!("manifests/v1/{signal}/{period}.json"));
            let mut manifest = read_manifest(store.as_ref(), &manifest_path)
                .await?
                .unwrap_or_else(|| Manifest::empty(signal, &period));
            let committed: HashSet<String> = manifest
                .source_objects
                .iter()
                .map(|source| source.key.clone())
                .collect();
            let mut pending = Vec::new();
            for object in objects {
                if committed.contains(object.location.as_ref()) {
                    cleanup_raw(store.as_ref(), &object, started, raw_retention, &mut report)
                        .await?;
                } else {
                    pending.push(object);
                }
            }
            if pending.is_empty() {
                continue;
            }
            let mut period_had_output = false;
            for (part_index, object) in pending.iter().enumerate() {
                let mut rows = load_rows(store.as_ref(), signal, object).await?;
                if matches!(signal, "logs" | "traces") {
                    rows.retain(|row| !application::is_application_row(row));
                }
                let source = SourceObject {
                    key: object.location.to_string(),
                    size: object.size,
                    etag: object.e_tag.clone(),
                };
                let output = if rows.is_empty() {
                    None
                } else {
                    let output_path = Path::from(format!(
                        "curated/v1/{signal}/{period}/part-{run_id}-{part_index:04}.parquet"
                    ));
                    Some(write_output(store.as_ref(), &output_path, &rows).await?)
                };

                manifest.generation += 1;
                manifest.updated_at = Utc::now().to_rfc3339();
                manifest.total_rows += rows.len();
                manifest.source_objects.push(source);
                if let Some(output) = &output {
                    manifest.outputs.push(output.clone());
                }
                let manifest_bytes = serde_json::to_vec_pretty(&manifest)?;
                store
                    .put(&manifest_path, Bytes::from(manifest_bytes).into())
                    .await
                    .with_context(|| format!("commit manifest {manifest_path}"))?;

                cleanup_raw(store.as_ref(), object, started, raw_retention, &mut report).await?;

                report.source_objects += 1;
                report.source_bytes += object.size;
                report.output_objects += usize::from(output.is_some());
                report.output_bytes += output.as_ref().map_or(0, |value| value.bytes);
                report.rows += rows.len();
                period_had_output = true;
            }
            if period_had_output {
                report.periods.push(format!("{signal}/{period}"));
            }
        }
    }

    report.completed_at = Utc::now().to_rfc3339();
    if report.source_objects > 0 || report.deleted_raw_objects > 0 {
        let report_path = Path::from(format!(
            "reports/v1/date={}/{}.json",
            started.format("%Y-%m-%d"),
            run_id
        ));
        store
            .put(
                &report_path,
                Bytes::from(serde_json::to_vec_pretty(&report)?).into(),
            )
            .await
            .context("write compaction report")?;
    }
    Ok(report)
}

async fn compact_applications(
    store: &dyn ObjectStore,
    started: DateTime<Utc>,
    seal_delay: Duration,
    run_id: &str,
    report: &mut RunReport,
) -> Result<()> {
    let mut periods: BTreeMap<String, Vec<(String, ObjectMeta)>> = BTreeMap::new();
    for signal in ["logs", "traces"] {
        let prefix = Path::from(format!("raw/v1/{signal}"));
        for object in store
            .list(Some(&prefix))
            .try_collect::<Vec<_>>()
            .await
            .with_context(|| format!("list raw {signal} objects for applications"))?
        {
            if is_sealed(&object, started, seal_delay)
                && let Some(period) = period_from_key(object.location.as_ref())
            {
                periods
                    .entry(period)
                    .or_default()
                    .push((signal.to_owned(), object));
            }
        }
    }

    for (period, objects) in periods {
        let manifest_path = Path::from(format!("manifests/v1/applications/{period}.json"));
        let mut manifest = read_manifest(store, &manifest_path)
            .await?
            .unwrap_or_else(|| Manifest::empty("applications", &period));
        let committed = manifest
            .source_objects
            .iter()
            .map(|source| source.key.clone())
            .collect::<HashSet<_>>();
        let pending = objects
            .iter()
            .filter(|(_, object)| !committed.contains(object.location.as_ref()))
            .collect::<Vec<_>>();
        if pending.is_empty() {
            continue;
        }

        // Rebuild the minute from every retained raw input when a late object
        // arrives. The manifest then points only at the replacement generation,
        // so retried roots cannot inflate operation totals across parts. If an
        // older raw input has already expired, keep an immutable partial part
        // instead of pretending the cross-generation total is complete.
        let can_rebuild = manifest.source_objects.iter().all(|source| {
            objects
                .iter()
                .any(|(_, object)| object.location.as_ref() == source.key)
        });
        let selected = if can_rebuild {
            objects.iter().collect::<Vec<_>>()
        } else {
            pending.clone()
        };
        let mut window = application::ApplicationWindow::with_period(&period);
        if manifest.generation > 0 && !can_rebuild {
            window.mark_cross_generation_partial();
        }
        for (signal, object) in &selected {
            if signal == "logs" {
                window.observe_logs(&load_rows(store, signal, object).await?);
            }
        }
        for (signal, object) in &selected {
            if signal == "traces" {
                window.observe_traces(&load_rows(store, signal, object).await?);
            }
        }
        window.finish_decisions();

        let mut rows = window.summary_rows();
        for (signal, object) in &selected {
            for row in load_rows(store, signal, object).await? {
                if let Some(row) = window.retain(row) {
                    rows.push(row);
                }
            }
        }

        manifest.generation += 1;
        manifest.updated_at = Utc::now().to_rfc3339();
        if can_rebuild {
            manifest.total_rows = rows.len();
            manifest.outputs.clear();
        } else {
            manifest.total_rows += rows.len();
        }
        manifest
            .source_objects
            .extend(pending.iter().map(|(_, object)| SourceObject {
                key: object.location.to_string(),
                size: object.size,
                etag: object.e_tag.clone(),
            }));
        if !rows.is_empty() {
            let output_path = Path::from(format!(
                "curated/v1/applications/{period}/part-{run_id}-{:04}.parquet",
                manifest.generation
            ));
            let output = write_output(store, &output_path, &rows).await?;
            report.output_objects += 1;
            report.output_bytes += output.bytes;
            report.rows += rows.len();
            manifest.outputs.push(output);
        }
        store
            .put(
                &manifest_path,
                Bytes::from(serde_json::to_vec_pretty(&manifest)?).into(),
            )
            .await
            .with_context(|| format!("commit application manifest {manifest_path}"))?;
        report.periods.push(format!("applications/{period}"));
    }
    Ok(())
}

async fn load_rows(
    store: &dyn ObjectStore,
    signal: &str,
    object: &ObjectMeta,
) -> Result<Vec<model::TelemetryRow>> {
    let compressed = store
        .get(&object.location)
        .await
        .with_context(|| format!("get {}", object.location))?
        .bytes()
        .await
        .with_context(|| format!("read {}", object.location))?;
    let payload = decode_object(object.location.as_ref(), &compressed)?;
    otlp::flatten(signal, &payload).with_context(|| format!("flatten {}", object.location))
}

async fn write_output(
    store: &dyn ObjectStore,
    output_path: &Path,
    rows: &[model::TelemetryRow],
) -> Result<OutputObject> {
    let parquet = parquet_output::encode(rows)?;
    let bytes = parquet.len();
    ensure!(
        bytes <= MAX_OUTPUT_BYTES,
        "candidate exceeds 16 MiB; raw inputs remain uncommitted"
    );
    let checksum = hex::encode(Sha256::digest(&parquet));
    store
        .put(output_path, Bytes::from(parquet).into())
        .await
        .with_context(|| format!("upload candidate {output_path}"))?;
    let uploaded = store
        .get(output_path)
        .await
        .with_context(|| format!("reopen candidate {output_path}"))?
        .bytes()
        .await?;
    ensure!(
        checksum == hex::encode(Sha256::digest(&uploaded)),
        "candidate checksum mismatch for {output_path}"
    );
    parquet_output::validate(&uploaded, rows.len())?;
    Ok(OutputObject {
        key: output_path.to_string(),
        rows: rows.len(),
        bytes,
        sha256: checksum,
    })
}

fn is_sealed(meta: &ObjectMeta, now: DateTime<Utc>, seal_delay: Duration) -> bool {
    now.signed_duration_since(meta.last_modified)
        .to_std()
        .map(|age| age >= seal_delay)
        .unwrap_or(false)
}

async fn cleanup_raw(
    store: &dyn ObjectStore,
    meta: &ObjectMeta,
    now: DateTime<Utc>,
    raw_retention: Duration,
    report: &mut RunReport,
) -> Result<()> {
    if !is_sealed(meta, now, raw_retention) {
        return Ok(());
    }
    store
        .delete(&meta.location)
        .await
        .with_context(|| format!("delete committed raw object {}", meta.location))?;
    report.deleted_raw_objects += 1;
    report.deleted_raw_bytes += meta.size;
    Ok(())
}

fn period_from_key(key: &str) -> Option<String> {
    let date = key.split('/').find(|part| part.starts_with("date="))?;
    let hour = key.split('/').find(|part| part.starts_with("hour="))?;
    let minute = key.split('/').find(|part| part.starts_with("minute="))?;
    Some(format!("{date}/{hour}/{minute}"))
}

fn decode_object(key: &str, body: &[u8]) -> Result<Vec<u8>> {
    if key.ends_with(".gz") || body.starts_with(&[0x1f, 0x8b]) {
        let mut decoded = Vec::new();
        GzDecoder::new(body)
            .take(MAX_DECODED_OBJECT_BYTES + 1)
            .read_to_end(&mut decoded)
            .with_context(|| format!("decompress {key}"))?;
        ensure!(
            decoded.len() as u64 <= MAX_DECODED_OBJECT_BYTES,
            "decoded object exceeds {} MiB: {key}",
            MAX_DECODED_OBJECT_BYTES / 1024 / 1024
        );
        return Ok(decoded);
    }
    ensure!(
        body.len() as u64 <= MAX_DECODED_OBJECT_BYTES,
        "object exceeds {} MiB: {key}",
        MAX_DECODED_OBJECT_BYTES / 1024 / 1024
    );
    Ok(body.to_vec())
}

async fn read_manifest(store: &dyn ObjectStore, path: &Path) -> Result<Option<Manifest>> {
    match store.get(path).await {
        Ok(result) => {
            let bytes = result.bytes().await?;
            let manifest = serde_json::from_slice(&bytes)
                .with_context(|| format!("decode manifest {path}"))?;
            Ok(Some(manifest))
        }
        Err(object_store::Error::NotFound { .. }) => Ok(None),
        Err(error) => Err(error).with_context(|| format!("read manifest {path}")),
    }
}

fn required_env(key: &str) -> Result<String> {
    env::var(key).with_context(|| format!("{key} is required"))
}

fn env_u64(key: &str, default: u64) -> Result<u64> {
    match env::var(key) {
        Ok(value) => value.parse().with_context(|| format!("parse {key}")),
        Err(env::VarError::NotPresent) => Ok(default),
        Err(error) => Err(error).with_context(|| format!("read {key}")),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use flate2::Compression;
    use flate2::write::GzEncoder;
    use object_store::memory::InMemory;
    use std::io::Write;

    #[test]
    fn parses_period_anywhere_after_node_prefix() {
        assert_eq!(
            period_from_key("raw/v1/logs/node-a/date=2026-08-25/hour=12/minute=03/logs.json.gz"),
            Some("date=2026-08-25/hour=12/minute=03".into())
        );
    }

    #[tokio::test]
    async fn compaction_commits_manifest_after_parquet() {
        let store: Arc<dyn ObjectStore> = Arc::new(InMemory::new());
        let payload = br#"{"resourceLogs":[{"resource":{"attributes":[]},"scopeLogs":[{"logRecords":[{"timeUnixNano":"1","body":{"stringValue":"hello"}}]}]}]}"#;
        let mut encoder = GzEncoder::new(Vec::new(), Compression::default());
        encoder.write_all(payload).unwrap();
        let compressed = encoder.finish().unwrap();
        let raw = Path::from("raw/v1/logs/node-a/date=2026-08-25/hour=12/minute=03/logs.json.gz");
        store
            .put(&raw, Bytes::from(compressed).into())
            .await
            .unwrap();
        let second_raw =
            Path::from("raw/v1/logs/node-b/date=2026-08-25/hour=12/minute=03/logs.json.gz");
        let mut encoder = GzEncoder::new(Vec::new(), Compression::default());
        encoder.write_all(payload).unwrap();
        store
            .put(&second_raw, Bytes::from(encoder.finish().unwrap()).into())
            .await
            .unwrap();

        let report = compact(store.clone(), Duration::ZERO, Duration::ZERO)
            .await
            .unwrap();
        assert_eq!(report.output_objects, 2);
        assert_eq!(report.rows, 2);

        let manifest_path = Path::from("manifests/v1/logs/date=2026-08-25/hour=12/minute=03.json");
        let manifest = read_manifest(store.as_ref(), &manifest_path)
            .await
            .unwrap()
            .unwrap();
        assert_eq!(manifest.generation, 2);
        assert_eq!(manifest.total_rows, 2);
        assert_eq!(manifest.source_objects.len(), 2);
        assert_eq!(manifest.outputs.len(), 2);

        assert!(matches!(
            store.get(&raw).await,
            Err(object_store::Error::NotFound { .. })
        ));
        assert!(matches!(
            store.get(&second_raw).await,
            Err(object_store::Error::NotFound { .. })
        ));

        let second = compact(store.clone(), Duration::ZERO, Duration::ZERO)
            .await
            .unwrap();
        assert_eq!(second.output_objects, 0);
        let reports = store
            .list(Some(&Path::from("reports/v1")))
            .try_collect::<Vec<_>>()
            .await
            .unwrap();
        assert_eq!(reports.len(), 1);
    }

    #[tokio::test]
    async fn application_compaction_commits_summary_and_bundle_before_raw_cleanup() {
        let store: Arc<dyn ObjectStore> = Arc::new(InMemory::new());
        let trace = br#"{"resourceSpans":[{"resource":{"attributes":[{"key":"infra.telemetry.type","value":{"stringValue":"application"}},{"key":"service.namespace","value":{"stringValue":"nimmly"}},{"key":"service.name","value":{"stringValue":"nimmly-api"}},{"key":"k8s.namespace.name","value":{"stringValue":"nimmly"}},{"key":"k8s.deployment.name","value":{"stringValue":"nimmly-api"}}]},"scopeSpans":[{"spans":[{"traceId":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","spanId":"bbbbbbbbbbbbbbbb","name":"GET /health","kind":2,"startTimeUnixNano":"1000000000","endTimeUnixNano":"1010000000","status":{"code":2},"attributes":[{"key":"infra.operation.kind","value":{"stringValue":"http"}},{"key":"infra.execution.outcome","value":{"stringValue":"error"}}]}]}]}]}"#;
        let log = br#"{"resourceLogs":[{"resource":{"attributes":[{"key":"infra.telemetry.type","value":{"stringValue":"application"}},{"key":"service.namespace","value":{"stringValue":"nimmly"}},{"key":"service.name","value":{"stringValue":"nimmly-api"}},{"key":"k8s.namespace.name","value":{"stringValue":"nimmly"}},{"key":"k8s.deployment.name","value":{"stringValue":"nimmly-api"}}]},"scopeLogs":[{"logRecords":[{"timeUnixNano":"1010000000","severityNumber":17,"severityText":"ERROR","traceId":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","spanId":"bbbbbbbbbbbbbbbb","body":{"stringValue":"request failed"},"attributes":[{"key":"infra.operation.kind","value":{"stringValue":"http"}},{"key":"infra.execution.outcome","value":{"stringValue":"error"}}]}]}]}]}"#;
        for (signal, payload) in [("traces", trace.as_slice()), ("logs", log.as_slice())] {
            let raw = Path::from(format!(
                "raw/v1/{signal}/node-a/date=2026-08-25/hour=12/minute=03/{signal}.json"
            ));
            store
                .put(&raw, Bytes::copy_from_slice(payload).into())
                .await
                .unwrap();
        }

        compact(store.clone(), Duration::ZERO, Duration::from_secs(3_600))
            .await
            .unwrap();

        let application = read_manifest(
            store.as_ref(),
            &Path::from("manifests/v1/applications/date=2026-08-25/hour=12/minute=03.json"),
        )
        .await
        .unwrap()
        .unwrap();
        assert_eq!(application.source_objects.len(), 2);
        assert_eq!(application.outputs.len(), 1);
        assert_eq!(application.total_rows, 3);

        for signal in ["logs", "traces"] {
            let ordinary = read_manifest(
                store.as_ref(),
                &Path::from(format!(
                    "manifests/v1/{signal}/date=2026-08-25/hour=12/minute=03.json"
                )),
            )
            .await
            .unwrap()
            .unwrap();
            assert_eq!(ordinary.source_objects.len(), 1);
            assert!(ordinary.outputs.is_empty());
        }

        let late = Path::from("raw/v1/traces/node-b/date=2026-08-25/hour=12/minute=03/traces.json");
        store
            .put(&late, Bytes::copy_from_slice(trace).into())
            .await
            .unwrap();
        compact(store.clone(), Duration::ZERO, Duration::from_secs(3_600))
            .await
            .unwrap();
        let application = read_manifest(
            store.as_ref(),
            &Path::from("manifests/v1/applications/date=2026-08-25/hour=12/minute=03.json"),
        )
        .await
        .unwrap()
        .unwrap();
        assert_eq!(application.generation, 2);
        assert_eq!(application.source_objects.len(), 3);
        assert_eq!(application.outputs.len(), 1);
        assert_eq!(application.total_rows, 3);
    }
}
