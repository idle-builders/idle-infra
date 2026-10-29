import { pathToFileURL } from "node:url";
import { createHash } from "node:crypto";

const required = [
  "DUCKDB_NODE_API_ROOT",
  "DUCKLAKE_BROKER_ENDPOINT",
  "DUCKLAKE_BUCKET",
  "DUCKLAKE_SESSION_TOKEN",
  "DUCKLAKE_AUTHORITY",
  "DUCKLAKE_OTHER_AUTHORITY",
  "DUCKLAKE_DATA_PREFIX",
  "DUCKLAKE_METADATA_PATH",
];
for (const name of required) {
  if (!process.env[name]) throw new Error(`${name} is required`);
}

const apiURL = pathToFileURL(`${process.env.DUCKDB_NODE_API_ROOT}/lib/duckdb.js`).href;
const { DuckDBInstance } = await import(apiURL);
const sqlString = (value) => `'${value.replaceAll("'", "''")}'`;

const instance = await DuckDBInstance.create(":memory:");
const connection = await instance.connect();

async function run(sql) {
  return connection.run(sql);
}

async function rows(sql) {
  return (await run(sql)).getRowObjectsJson();
}

async function installS3Secret(target, authority) {
  await target.run("DROP SECRET IF EXISTS governed_lake");
  await target.run(`CREATE SECRET governed_lake (
    TYPE s3,
    PROVIDER config,
    KEY_ID ${sqlString(authority)},
    SECRET 'dummy-no-authority',
    SESSION_TOKEN ${sqlString(process.env.DUCKLAKE_SESSION_TOKEN)},
    REGION 'us-east-1',
    ENDPOINT ${sqlString(process.env.DUCKLAKE_BROKER_ENDPOINT)},
    URL_STYLE 'path',
    USE_SSL false,
    SCOPE ${sqlString(`s3://${process.env.DUCKLAKE_BUCKET}/${process.env.DUCKLAKE_DATA_PREFIX}`)}
  )`);
}

function governedURL(key, query = "") {
  const encoded = key.split("/").map(encodeURIComponent).join("/");
  return `http://${process.env.DUCKLAKE_BROKER_ENDPOINT}/${process.env.DUCKLAKE_BUCKET}/${encoded}${query}`;
}

function authorization(authority) {
  return `AWS4-HMAC-SHA256 Credential=${authority}/20260908/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date;x-amz-security-token, Signature=ignored`;
}

async function putSentinel(key, authority, payload) {
  const response = await fetch(governedURL(key), {
    method: "PUT",
    headers: {
      Authorization: authorization(authority),
      "Content-Type": "application/octet-stream",
      "x-amz-content-sha256": createHash("sha256").update(payload).digest("hex"),
      "x-amz-security-token": process.env.DUCKLAKE_SESSION_TOKEN,
    },
    body: payload,
  });
  if (!response.ok) throw new Error(`sentinel PUT failed with ${response.status}: ${await response.text()}`);
}

async function sentinelReadable(key, authority, expected) {
  const response = await fetch(governedURL(key), {
    headers: {
      Authorization: authorization(authority),
      "x-amz-security-token": process.env.DUCKLAKE_SESSION_TOKEN,
    },
  });
  return response.ok && Buffer.from(await response.arrayBuffer()).equals(expected);
}

function storageKey(path) {
  const absolutePrefix = `s3://${process.env.DUCKLAKE_BUCKET}/`;
  if (path.startsWith(absolutePrefix)) return path.slice(absolutePrefix.length);
  if (path.startsWith(process.env.DUCKLAKE_DATA_PREFIX)) return path;
  return `${process.env.DUCKLAKE_DATA_PREFIX}${path}`;
}

try {
  await run("INSTALL httpfs; LOAD httpfs; INSTALL ducklake; LOAD ducklake;");
  await installS3Secret(connection, process.env.DUCKLAKE_AUTHORITY);
  const dataPath = `s3://${process.env.DUCKLAKE_BUCKET}/${process.env.DUCKLAKE_DATA_PREFIX}`;
  const publicSentinelKey = `${process.env.DUCKLAKE_DATA_PREFIX}public-sentinel.bin`;
  const peerSentinelKey = `${process.env.DUCKLAKE_DATA_PREFIX}peer-sentinel.bin`;
  const publicSentinel = Buffer.from("tenant-public cleanup sentinel");
  const peerSentinel = Buffer.from("peer-workspace cleanup sentinel");
  await putSentinel(publicSentinelKey, "tenant", publicSentinel);
  await putSentinel(peerSentinelKey, process.env.DUCKLAKE_OTHER_AUTHORITY, peerSentinel);
  await run(`ATTACH ${sqlString(`ducklake:${process.env.DUCKLAKE_METADATA_PATH}`)} AS lake (
    DATA_PATH ${sqlString(dataPath)}, DATA_INLINING_ROW_LIMIT 0
  )`);
  await run("CALL lake.set_option('target_file_size', '32MB')");
  await run("CREATE TABLE lake.facts (id BIGINT, payload VARCHAR)");
  await run("INSERT INTO lake.facts SELECT i, repeat(md5(i::VARCHAR), 32) FROM range(0, 25000) t(i)");
  const inserted = await rows("SELECT count(*) AS count FROM lake.facts");
  if (Number(inserted[0].count) !== 25000) throw new Error("insert/read count mismatch");

  await run(`MERGE INTO lake.facts AS target
    USING (VALUES (1, 'merged'), (25000, 'inserted')) AS source(id, payload)
    ON target.id = source.id
    WHEN MATCHED THEN UPDATE SET payload = source.payload
    WHEN NOT MATCHED THEN INSERT (id, payload) VALUES (source.id, source.payload)`);
  await run("DELETE FROM lake.facts WHERE id = 2");
  // Force DuckLake to create and then discard transaction-local data/delete
  // files through exact DELETE requests; no listing or bulk-delete maintenance.
  await run("BEGIN TRANSACTION");
  await run("INSERT INTO lake.facts SELECT i, repeat('rollback', 128) FROM range(30000, 50000) t(i)");
  await run("DELETE FROM lake.facts WHERE id BETWEEN 10 AND 1000");
  await run("ROLLBACK");
  await run("BEGIN TRANSACTION");
  await run("INSERT INTO lake.facts VALUES (25001, 'commit-metadata')");
  await run("CALL lake.set_commit_message('storage-broker-probe', 'governed DuckLake acceptance')");
  await run("COMMIT");

  const committed = await rows("SELECT count(*) AS count FROM lake.facts");
  if (Number(committed[0].count) !== 25001) throw new Error("MERGE/DELETE/commit count mismatch");
  const snapshotsBeforeExpiry = await rows("SELECT snapshot_id, author, commit_message FROM lake.snapshots() ORDER BY snapshot_id");
  if (!snapshotsBeforeExpiry.some((snapshot) => snapshot.author === "storage-broker-probe")) {
    throw new Error("snapshot commit metadata missing");
  }

  const rewritten = await rows("FROM ducklake_rewrite_data_files('lake', 'facts', delete_threshold => 0)");
  if (!rewritten.some((row) => Number(row.files_processed) > 0)) {
    throw new Error("DuckLake did not rewrite an obsolete data file");
  }
  await run("CALL ducklake_expire_snapshots('lake', older_than => now())");
  const scheduled = await rows("SELECT path FROM ducklake_cleanup_old_files('lake', dry_run => true, cleanup_all => true)");
  if (scheduled.length === 0) throw new Error("DuckLake scheduled no obsolete files for deletion");
  const cleaned = await rows("SELECT path FROM ducklake_cleanup_old_files('lake', dry_run => false, cleanup_all => true)");
  if (cleaned.length !== scheduled.length) throw new Error("DuckLake cleanup did not process the scheduled set");
  const remainingScheduled = await rows("SELECT path FROM ducklake_cleanup_old_files('lake', dry_run => true, cleanup_all => true)");
  if (remainingScheduled.length !== 0) throw new Error("DuckLake cleanup left scheduled files behind");

  let obsoleteMissing = true;
  for (const row of scheduled) {
    const response = await fetch(governedURL(storageKey(row.path)), {
      method: "HEAD",
      headers: {
        Authorization: authorization(process.env.DUCKLAKE_AUTHORITY),
        "x-amz-security-token": process.env.DUCKLAKE_SESSION_TOKEN,
      },
    });
    if (response.status !== 404) obsoleteMissing = false;
  }
  if (!obsoleteMissing) throw new Error("an obsolete object remains readable after cleanup");

  const afterCleanup = await rows("SELECT count(*) AS count FROM lake.facts");
  const currentReadable = Number(afterCleanup[0].count) === 25001;
  if (!currentReadable) throw new Error("current DuckLake data became unreadable after cleanup");
  const files = await rows("FROM ducklake_list_files('lake', 'facts')");
  const candidate = files.map((row) => row.data_file).find(Boolean);
  if (!candidate) throw new Error("DuckLake produced no current Parquet data file");
  const candidatePath = candidate.startsWith("s3://") ? candidate : `${dataPath}${candidate}`;

  const otherInstance = await DuckDBInstance.create(":memory:");
  const otherConnection = await otherInstance.connect();
  await otherConnection.run("INSTALL httpfs; LOAD httpfs;");
  await installS3Secret(otherConnection, process.env.DUCKLAKE_OTHER_AUTHORITY);
  let crossWorkspaceDenied = false;
  try {
    await otherConnection.run(`SELECT count(*) FROM read_parquet(${sqlString(candidatePath)})`);
  } catch {
    crossWorkspaceDenied = true;
  } finally {
    otherConnection.closeSync();
  }
  if (!crossWorkspaceDenied) throw new Error("second workspace read unexpectedly succeeded");

  const publicUntouched = await sentinelReadable(publicSentinelKey, "tenant", publicSentinel);
  const peerUntouched = await sentinelReadable(peerSentinelKey, process.env.DUCKLAKE_OTHER_AUTHORITY, peerSentinel);
  if (!publicUntouched || !peerUntouched) throw new Error("cleanup changed an object outside its exact workspace");

  const listingResponse = await fetch(`http://${process.env.DUCKLAKE_BROKER_ENDPOINT}/${process.env.DUCKLAKE_BUCKET}/?list-type=2`);
  const multipartResponse = await fetch(governedURL(`${process.env.DUCKLAKE_DATA_PREFIX}unsupported.parquet`, "?uploads"), { method: "POST" });
  const listingDenied = listingResponse.status === 501;
  const multipartDenied = multipartResponse.status === 501;
  if (!listingDenied || !multipartDenied) throw new Error("listing or multipart unexpectedly became available");

  process.stdout.write(`${JSON.stringify({
    inserted: 25000,
    committed: 25001,
    snapshots: snapshotsBeforeExpiry.length,
    files: files.length,
    crossWorkspaceDenied,
    scheduled: scheduled.map((row) => row.path),
    cleaned: cleaned.map((row) => row.path),
    obsoleteMissing,
    currentReadable,
    publicUntouched,
    peerUntouched,
    listingDenied,
    multipartDenied,
  })}\n`);
} finally {
  connection.closeSync();
}
