# Governed Object Storage

Examples use the fictional `acme` tenant. Workload authorities and producer
ServiceAccounts are deployment configuration, not preselected framework tenants.

## Required reading

- `docs/concepts/platform.md` — platform tenant, shared service and isolation-boundary authority
- `docs/concepts/shared-auth.md` — authentication is shared; application authorization remains tenant-owned

## Governed object

**Definition.** A governed object is an immutable broker row that identifies one stored object and binds it to
exactly one platform tenant and either tenant-public authority or one application workspace.

**Essential properties.**

- Opaque object identity independent of its bucket and key.
- Exactly one platform tenant derived from authenticated workload identity.
- Exactly one authority scope: tenant-public or one Workspace UUID.
- Verified content hash, byte size, media type and closed artifact class.
- Physical bucket/key known only to the broker and never used as authorization.
- Explicit lifecycle state so an incomplete or deleted object is not readable.

**What it is not.**

- **Not an S3 key or prefix.** Keys place bytes; renaming or copying a key cannot grant authority.
- **Not a workspace membership.** Each tenant application decide which human or job belongs to
  an application workspace. The broker enforces a supplied, already-authorized scope against its own rows.
- **Not the bytes.** Bytes remain in S3 and move through short-lived presigned transfers; the row is authorization
  metadata.
- **Not a mutable document record.** New content produces a new immutable object row. Application documents and
  versions reference it from their own schemas.
- **Not a generic catalogue.** Business meaning, source lineage, schemas, evidence and search metadata remain in
  the tenant application.

**Composition.** A governed object belongs to one platform tenant, carries one authority scope and one physical
locator, and is referenced by application records or bounded execution grants. Several rows may reference
content-identical bytes when their tenant/workspace authority differs.

**Lifecycle.** Ordinary immutable artifacts use `reserved` → staged upload → broker-controlled copy → hash/size
verification → `active` → optional `deleted`. Infomaniak Swift does not support conditional object PUTs, so a
client presign is issued only for a unique staging key. The broker copies those bytes to a final key that has
never had a write ticket, verifies that copy, activates the row and removes the staging object. A failed or
expired reservation never becomes readable. Deletion revokes new transfers before the physical object is
removed.

**Authority.** The broker owns the row, locator and lifecycle. An allowlisted producer ServiceAccount may request
registration only after its application has resolved domain authority. The broker derives tenant and exact
ServiceAccount from TokenReview, assigns scope to the new row, and never accepts a bucket/key as proof.

### Ordinary object deletion contract

Ordinary governed objects (every artifact class except `ducklake_data`) are deleted by opaque object ID through
the JSON control plane:

```http
DELETE /v2/objects/{objectId}
Authorization: Bearer <projected ServiceAccount token, audience storage-broker>
Content-Type: application/json

{"authority":{"kind":"workspace","workspaceId":"<workspace UUID>"}}
```

Tenant-public deletion uses `{"authority":{"kind":"tenant"}}`. The broker derives `tenantId` and the exact
ServiceAccount from TokenReview; neither is caller-supplied. The caller must hold the `object:delete` governed
operation. In this example, only `transformation-storage-authority` has that operation. Acme remains responsible for
membership authorization, selecting all dependent object IDs and stopping writers before deletion. The broker
does not query acme's schema or discover dependencies.

The supplied authority is an equality check, not the workspace read overlay. A workspace request cannot delete a
tenant-public object or an object from another workspace; a tenant-public request cannot delete a workspace
object. A wrong tenant, wrong authority, unknown ID, or `ducklake_data` ID returns the same `403` without changing
the row. `ducklake_data` remains exclusively on the existing exact-path Lake DELETE surface.

The response contract is:

```json
{
  "schemaVersion": 1,
  "objectId": "<opaque UUID>",
  "tenantId": "acme",
  "authority": {"kind": "workspace", "workspaceId": "<workspace UUID>"},
  "accessState": "revoked",
  "accessRevokedAt": "<RFC3339 timestamp>",
  "physicalRemovalState": "pending|confirmed",
  "physicalRemovalNotBefore": "<RFC3339 timestamp, pending only>",
  "physicalRemovalConfirmedAt": "<RFC3339 timestamp, confirmed only>",
  "retryAfterSeconds": 123
}
```

- `202 Accepted` means the row is durably tombstoned and no new grant consumption, transfer ticket or commit can
  authorize it, but physical removal is still pending. `Retry-After` and `retryAfterSeconds` tell the caller when
  to retry.
- `200 OK` with `physicalRemovalState: "confirmed"` means the broker deleted and then confirmed absence of every
  recorded physical locator (final/current plus staging, when one exists) and persisted that confirmation.
- `503 Service Unavailable` retains `accessState: "revoked"` and `physicalRemovalState: "pending"`: storage or
  confirmation failed after revocation. It is safe and required to retry the same request.
- Repeating a pending or completed deletion with the same tenant and exact authority is idempotent. The original
  revocation and removal-confirmation timestamps do not move.

Reserved, active and already-deleted rows all use this contract. An object-level PostgreSQL advisory lock on a
collision-proof internal key serializes commit and deletion across broker replicas. Brokers built before this
protocol also took a legacy `object` key first; they take the internal key as well, so mixed-version replicas still
serialize the same object. Tombstoning happens before S3 cleanup, and every activation statement excludes
`deleted`, so an interrupted or concurrent commit cannot reactivate the row. Read-ticket issuance takes no lock.
Its single authorization query observes either the committed active row or the committed tombstone, and activation
and tombstoning are each one atomic statement. A ticket authorized just before a tombstone is equivalent to a GET
ticket issued just before deletion, which this contract already allows to stay bearer-readable until it expires or
the bytes are removed.

Presigned URLs are bearer credentials at S3 and cannot be individually revoked. For an object that ever received
an ordinary upload ticket, the broker therefore will not attempt or report physical completion until the full
configured PUT-ticket lifetime has elapsed after `accessRevokedAt` (currently 300 seconds). During that window a
previously issued PUT may still recreate only its recorded staging object; it can never write the final locator or
commit the tombstoned row. On retry after the window, the broker removes both staging and final/current locators
and confirms both absent. Acme's required stop-writers step ensures no upload was already in flight when
deletion began; the broker cannot observe an HTTP request already executing directly against S3. Existing GET
tickets may remain bearer-readable until their expiry or earlier physical removal, so `accessState: "revoked"`
specifically means broker authorization is revoked, not that every previously issued bearer URL has expired. The
same conservative PUT-ticket window also applies to objects registered from pre-existing bytes because legacy
presigns are not recorded per object.

Objects activated before the broker recorded `staging_key` cannot provide a complete physical-removal proof. After
the ticket window, the broker streams bounded pages from only that exact tenant-public or Workspace staging prefix
and artifact class,
deleting keys that no reserved or active row for that tenant references. It durably records that first sweep, returns
`202`/`pending`, waits one more full PUT-ticket lifetime for an asynchronously maintained Swift listing to settle,
then streams and cleans the prefix again. Because an eventually consistent listing cannot prove absence of an unknown
key, these legacy rows remain `503`/`pending` until their exact locator history is remediated; the broker never reports
physical completion from elapsed time alone. New reservations and registrations retain their exact staging locator,
so they can complete normally. A referenced staging key is never deleted
speculatively: the response remains `503`/`pending` until Acme's dependency selection has deleted the owning object
as well. This conservative sweep also removes abandoned legacy uploads without crossing the tenant-public/Workspace
boundary. Physical cleanup and confirmation hold a cross-replica, bucket-wide registration barrier and refuse to
remove bytes unless an immutable `storage_bucket_owners` record and persisted object history both prove that the
bucket belongs exclusively to that tenant. The broker claims every configured namespace/bucket mapping before it
starts serving, and rechecks that claim before legacy presigns, multipart operations, governed reservations and
Lake PUTs. A later policy rollout cannot reassign a previously claimed bucket: the new broker fails closed instead
of issuing a cross-tenant ticket. Historical object rows are backfilled into the ownership table, and conflicting
history makes migration fail. The barrier is a reader/writer gate: cleanup holds it exclusively, every registration
holds it shared, so registrations never wait for each other and only cleanup excludes them. Every reservation takes
the shared gate inside the database transaction that creates its row and signs its upload ticket before that
transaction commits. A reservation never waits for the gate while holding a pooled connection: it tries the lock, and
while cleanup holds or awaits the gate it rolls back, frees the connection and retries with capped backoff. Otherwise
reservations arriving during cleanup could fill the pool that cleanup, `register-existing` and Lake PUT need to finish.
A reservation stops retrying after 60 seconds and returns `503` with `Retry-After`; it wrote nothing and is safe to
retry. A cleanup that tombstones the row must first take the gate exclusively, so its recorded
revocation time is never earlier than the ticket's signing time and the PUT-ticket window above stays sound.
`register-existing` holds the shared gate and its exclusive physical-key lock together on one PostgreSQL connection
while it checks ownership, verifies bytes and registers the row. Ownership does not need the gate: the owner table's
primary key admits one tenant per bucket and its rows are never updated or deleted, so two tenants cannot both claim
a bucket, and a foreign row can only follow a successful claim. Both facts only ever change from unproven to proven,
so each replica caches a positive ownership and history decision for 30 seconds and never caches a denial or an
error. Physical cleanup does not use that cache; it re-reads ownership and history under its exclusive gate.
**Never update or delete a `storage_bucket_owners` row**, including during remediation of shared bucket history. The
broker and its cache assume these rows are immutable; an edited row can be served as a stale positive for up to 30
seconds per replica. Remediate by moving or removing the foreign object rows and their bytes, never the owner. Even a
readable key omitted by an eventually consistent listing cannot become active between the final scan and
confirmation. A `register-existing` attempt that overlaps cleanup's exclusive barrier is rejected with `409`, rather
than waiting and becoming live immediately after a confirmed response; a later retry is a new registration decision
by Acme. Before recording
either sweep or final confirmation, the broker also queries for every live database locator under the exact staging
prefix. This catches a registration that completed before cleanup even when Swift omitted its key from the listing.
The object lock is released after the durable tombstone, before the barrier is taken, so concurrent cleanup cannot
recursively exhaust the broker's lock pool.

Brokers built before the shared gate take it exclusively for every reservation. While old and new replicas run
together (a rolling update to the shared gate, and again on rollback), old-replica reservations keep the gate
exclusively held or queued almost continuously, so new-replica reservations rarely acquire it and return `503` after
their 60-second wait. Isolation is unaffected; only liveness suffers. Quiesce reservation traffic (pause Acme's
ingestion writers) for that rollout or rollback, and resume once every pod runs the same image.

The first deployment of this lock protocol is deliberately staged without downtime. The fixed broker image must
finish its normal rolling rollout on every replica while `object:delete` is still absent from all workload policies.
Only after every running pod reports that image digest may a separate apply grant `object:delete`. Combining the
first image rollout and permission grant would allow a new replica to clean a key while an old replica accepted an
unlocked `register-existing`; permanently switching the highly available Deployment to `Recreate` is not acceptable.
Rollback reverses those gates: first remove `object:delete`, wait for the policy-checksum rollout to replace every
permission-bearing pod (which also drains or interrupts its in-flight deletions), and only then roll back the image.
The broker also rejects any policy that assigns one physical bucket to more than one tenant, because legacy
tenant-public staging prefixes predate tenant-qualified bucket sharing and cannot be swept safely in a shared bucket.
DuckLake PUT takes the shared bucket gate before the exclusive deployed and intermediate path-lock keys, in
deterministic order, so it cannot create a foreign row or overwrite a physical key while ordinary cleanup is
confirming absence.

### Cleanup-owner and whole-workspace deletion

New ordinary reservations carry a host-chosen immutable `cleanupOwnerId` UUID. `POST /v2/objects/reserve` and
`POST /v2/grants` require it; grant request idempotency includes it. A grant output reserve does not accept an
owner field and inherits the grant's owner, so a sandbox cannot move output into another cleanup group. Existing
`register-existing` rows remain owner-null. DuckLake rows never carry a cleanup owner.

After stopping and draining its writers, an application may revoke and physically remove a complete owner group:

```http
DELETE /v2/cleanup-owners/{cleanupOwnerId}/objects
Authorization: Bearer <projected ServiceAccount token, audience storage-broker>
Content-Type: application/json

{"authority":{"kind":"workspace","workspaceId":"<workspace UUID>"}}
```

Owner cleanup also accepts exact tenant-public authority. Whole-workspace cleanup is deliberately separate and
accepts workspace authority only:

```http
DELETE /v2/ordinary-objects
Authorization: Bearer <projected ServiceAccount token, audience storage-broker>
Content-Type: application/json

{"authority":{"kind":"workspace","workspaceId":"<workspace UUID>"}}
```

Both require `object:delete`, exclude `ducklake_data`, and use exact tenant/authority equality. Owner cleanup
selects only that owner; whole-workspace cleanup includes every ordinary row in the workspace, including legacy
owner-null rows. In one serializable database transaction, cleanup revokes matching execution grants and
tombstones matching reserved and active objects. This makes an unobserved or still-valid grant unusable for new
output reservations after access revocation. The bucket-wide registration barrier remains held through physical
cleanup, so a concurrent output reservation cannot pass between grant revocation and object tombstoning.

The aggregate response is:

```json
{
  "schemaVersion": 1,
  "tenantId": "acme",
  "authority": {"kind": "workspace", "workspaceId": "<workspace UUID>"},
  "cleanupOwnerId": "<owner UUID; owner cleanup only>",
  "accessState": "revoked",
  "physicalRemovalState": "pending|confirmed",
  "pendingObjectCount": 2,
  "physicalRemovalNotBefore": "<RFC3339 timestamp, pending only>",
  "retryAfterSeconds": 123,
  "error": "<retryable physical-cleanup reason, when present>"
}
```

`202` means access is revoked while ticket settling or a bounded batch remains; `503` means access remains revoked
but storage removal or proof failed; `200` is returned only when `pendingObjectCount` is zero and physical removal
is confirmed. Cleanup examines at most 64 pending rows per request and rotates failures so one bad row cannot
starve the rest. Retry the same request until `200`. A missing exact locator remains honestly pending; these
aggregate APIs never use S3 listing. There is no permanent owner/workspace fence, so stopping host issuance and
not reusing a cleaned owner ID remain application preconditions.

## DuckLake-managed physical paths

DuckLake stores its generated Parquet and delete-file paths in its PostgreSQL catalog. Those locators must remain
unchanged, so `ducklake_data` objects are the narrow exception to staged promotion: the broker reserves the exact
DuckLake key before accepting bytes, streams the body to S3 while independently hashing and counting it, and
activates the row at that same key. This changes placement, not authority. A key under `ducklake/` has no access
meaning until an active governed row binds it to one tenant and one tenant-public or exact-workspace scope.

The existing broker deployment exposes this compatibility surface on container port 8081 (service port 81).
DuckDB must use path-style URLs, the broker service as `ENDPOINT`, a dummy signing secret, and the projected
ServiceAccount token as `SESSION_TOKEN`. The SigV4 access-key identifier asserts either `tenant` or
`workspace-<uuid>`. The broker ignores the signature as authentication: it authenticates only the session token
with Kubernetes TokenReview, derives the tenant bucket from that reviewed namespace, and accepts the authority
assertion only from a ServiceAccount holding the corresponding `lake:*` role. The dummy secret and requested
bucket have no authority.

Only the trusted acme `transformation-storage-authority` ServiceAccount receives `lake:read`, `lake:write` and
`lake:delete`. The sandbox controller, generated SQL and model workloads receive neither those roles nor the
projected token or in-memory DuckDB secret. The acme host must resolve and pin one publication write authority
before creating that temporary secret; one publication must not mix tenant-public and workspace-private output
inside a Parquet object.

The data route implements this S3 subset:

| Request | Governed behavior |
| --- | --- |
| `PUT /<bucket>/<key>` | Exact-path reservation; bounded streaming upload; required `Content-Length` and hexadecimal `x-amz-content-sha256`; row activation before success |
| `GET`, with optional single `Range` | Active-row lookup and tenant/workspace read overlay; streamed `200` or `206` response |
| `HEAD` | Same row authorization as GET, physical existence/size confirmation, stable SHA-256 ETag and range capability |
| `DELETE` | Exact write-scope equality; reserved/active/retry-deleted row marked deleted before physical deletion; success only after backend absence is confirmed and persisted |
| `POST /<bucket>?delete` | MD5-verified XML `DeleteObjects` batch of 1–1,000 exact keys; path locks drain in-flight PUTs, every row is authorized and marked deleted atomically, and per-key absence confirmation remains retryable |

The configured single-PUT ceiling is 96 MiB, leaving headroom over acme's initial DuckLake
`target_file_size = '32MB'`. Multipart initiation, parts, completion and abort are explicitly rejected. So are
`ListObjectsV2` and every other form of object listing remain rejected. Multi-object deletion does not enumerate a
prefix: it accepts only exact keys named by DuckLake, applies the same exact write-scope equality as single-object
DELETE, rejects version IDs and mixed-authority batches without changing any row, and caps the XML body at 8 MiB.
The `ducklake/` root is deliberately absent from the v1 prefix policy.

DuckDB's documented S3 writer can use multipart, while the pinned `@duckdb/node-api` runtime has been measured to
use ordinary PUT below its multipart threshold. Therefore the real DuckLake acceptance probe is a release gate;
`target_file_size` is not itself a protocol guarantee. Governed multipart is deferred. If it becomes necessary,
it must reuse the broker's existing multipart machinery and add whole-object SHA-256 verification before row
activation.

Run `scripts/test-ducklake-storage-broker.sh` after deployment. It drives real DuckDB/DuckLake transactions,
snapshot metadata, Parquet reads, rollback deletion and a cross-workspace denial through the broker, verifies the
resulting governed rows, then removes every still-active acceptance object by exact DELETE.

An application that has stopped and drained one workspace can reconcile a transaction-crash orphan without S3
listing through the JSON control plane:

```http
POST /v2/lake-objects/inventory
Authorization: Bearer <projected ServiceAccount token, audience storage-broker>
Content-Type: application/json

{"schemaVersion":1,"authority":{"kind":"workspace","workspaceId":"<workspace UUID>"},"limit":200,"cursor":"<opaque optional cursor>"}
```

This requires `lake:delete`, workspace authority, and a limit from 1 through 1,000 (default 200). It returns only
that exact tenant/workspace's `ducklake_data` rows in immutable object-ID order: reserved, active, and deleted rows
whose physical absence has not been confirmed. Each item is `{physicalKey,state}`; `nextCursor`, when present, is
broker-issued and bound to the reviewed tenant and requested workspace. The application compares each page with
its authoritative current catalogue data and delete-file paths, then sends only absent paths through the existing
exact DELETE or DeleteObjects surface. This keeps arbitrarily large live sets outside request bodies and requires
no retained-set session or broker business schema. Ordinary, tenant-public, and other-workspace rows are excluded.

### Failure and recovery

Mutation of one physical locator is serialized across broker replicas with a PostgreSQL advisory lock held only
for the request. There is no durable lake session and authentication caches are replica-local. The cache key is a
token hash, stores only namespace/ServiceAccount and expiry, has a 30-second maximum TTL, never outlives the JWT,
and fails closed.

- An abandoned or interrupted PUT leaves a `reserved` row, which is unreadable and can be retried with the same
  scope, hash, size and media type.
- Bytes stored before a broker or activation failure remain bound to that reservation; the same immutable PUT
  safely overwrites them and retries activation.
- An active retry consumes and verifies the request body but never overwrites physical bytes. A different
  declaration conflicts.
- Hash or size mismatch removes the physical candidate and leaves no readable row.
- DELETE changes the row to `deleted` before touching S3. A failed physical delete or absence check is therefore
  unreadable and remains visible to registry inventory; a repeated exact-scope DELETE retries cleanup.
- Missing, reserved, deleted and unauthorized paths all appear unavailable on GET/HEAD, so path knowledge cannot
  reveal another workspace's rows.

The JSON control plane retains its short request timeout. The separate lake listener has a five-second header
timeout, five-minute overall operation deadline, rolling 30-second read/write deadlines and four concurrent data
operations per replica. Request and response bodies are streamed; the broker does not buffer an analytical file.

### Maintenance boundary

Do not call `ducklake_delete_orphaned_files` or issue a bare DuckLake `CHECKPOINT`: its orphan discovery requires
listing, which this surface denies because a naive prefix listing would disclose other authorities. Snapshot
expiration and `ducklake_cleanup_old_files` are supported only after application-level authority has been pinned;
cleanup sends the catalog's exact scheduled paths through governed `DeleteObjects`. Transaction-crash orphan
reconciliation instead uses the exact-workspace governed registry inventory above and never enumerates S3.

### Infrastructure caveat

Infomaniak S3 credentials are project-scoped rather than tenant-scoped. They stay only in the shared broker and
encrypted infrastructure state, but this means the provider credential is a blast-radius concern rather than a
hard tenant boundary. Cross-tenant isolation is enforced in the broker and its policy; the tenant workloads never
receive the provider credential.

## Storage scope

**Definition.** A storage scope is the closed authority partition attached to a governed object or execution
grant: `{tenant}` for tenant-public data, or `{tenant, workspace}` for one application's private workspace.

**Essential properties.**

- Tenant is always present and comes from the reviewed Kubernetes identity.
- Workspace is either absent because the object is tenant-public, or one non-null application-issued UUID.
- A workspace-scoped read may include tenant-public objects plus objects from that exact workspace.
- A workspace-scoped write always produces that exact workspace scope.

**What it is not.**

- **Not a Kubernetes namespace.** Namespace identifies the platform tenant; Workspace partitions data inside it.
- **Not a Zitadel Organization or claim.** Shared auth identifies a caller but does not own application workspace
  membership.
- **Not a path convention.** `workspace/<uuid>/...` may remain a useful physical layout but carries no authority.

**Authority.** Tenant identity is platform-owned and verified by the broker. The tenant application owns the
initial workspace authorization decision. After a row or grant is created, the broker independently prevents a
consumer from changing, omitting or crossing that recorded scope.

## Execution grant

**Definition.** An execution grant is an immutable, expiring broker relationship that lets one exact consumer
execution read a closed set of governed objects and, when declared, create a closed set of artifact classes in
the same scope.

**Essential properties.**

- Opaque grant ID, exact tenant/workspace scope and exact execution ID.
- Immutable host-chosen cleanup-owner UUID inherited by every reserved grant output.
- Closed, bounded readable object set.
- Closed writable artifact-class set, which may be empty for a read-only host execution.
- Issuer and consumer ServiceAccount roles recorded independently.
- Expiry and explicit revocation; no credential or conversation body stored in the row.

**What it is not.**

- **Not an agent session.** Conversation/checkpoint bodies stay in object storage and may expire independently.
- **Not a user token.** Background workers never persist or replay customer credentials.
- **Not a prefix grant.** It cannot authorize objects added later merely because their keys share a path.
- **Not a general capability language.** It has only the read-set and output-class operations required for one
  bounded execution.

**Composition.** A trusted tenant worker issues the grant after resolving application authority. A narrower
consumer workload, such as the transformation sandbox controller, consumes it. Governed objects and result rows
attach to the grant through broker-owned relationships.

**Lifecycle.** `active` → `expired` or `revoked`. Completion revokes it after result registration. Retry creates a
new grant rather than extending or mutating the old one.

**Authority.** The broker creates, evaluates and revokes grant rows. An allowlisted issuer workload may request a
grant for already-governed objects in one compatible scope. A consumer can use but never issue, widen or extend it.

## Transfer ticket

**Definition.** A transfer ticket is a short-lived, single-object presigned S3 URL returned only after a governed
row authorization decision.

It is deliberately not called a grant. The grant is durable authorization metadata; the ticket is a temporary
bearer mechanism for moving bytes. Ticket expiry is minutes, never the execution-grant lifetime by implication.

## Vocabulary

| Term | Definition | Aliases not to use |
| --- | --- | --- |
| **governed object** | Broker row binding one physical object to tenant/workspace authority | storage row, file record, key |
| **storage scope** | One tenant plus tenant-public or one exact Workspace partition | prefix, namespace, user scope |
| **execution grant** | Bounded row relationship for one consumer execution | session, token, prefix grant |
| **transfer ticket** | One short-lived presigned byte-transfer URL | grant, object permission |
| **physical locator** | Broker-private bucket/key placement | object identity, authority |

## Contracts

- Every governed object belongs to exactly one platform tenant and exactly one authority scope.
- A physical locator is unique among active rows but never appears in a governed application contract.
- Object identity is opaque; content hash proves bytes but does not confer authority or merge scopes.
- Tenant-public and workspace-private are the only storage authority kinds in this version.
- A workspace execution can read tenant-public plus its exact workspace; it cannot read another workspace.
- Every output inherits the grant scope; callers cannot supply a different tenant or workspace.
- Every grant names at most 256 readable objects and a closed set of artifact classes.
- Grant issuance, consumption, expiry and revocation are broker-row transactions.
- TokenReview must yield both namespace and ServiceAccount; v2 operation roles match both.
- Application membership authorization happens before grant issuance and remains application-owned.
- The broker never queries tenant application schemas and never trusts a Workspace claim from Zitadel.
- Ambiguous legacy objects remain unavailable; migration never derives authority from an S3 key.
- A presigned transfer is single-object, short-lived and returned only after the row decision.
- Ordinary object DELETE requires exact tenant/workspace equality, tombstones before physical cleanup, and never
  accepts `ducklake_data`.
- A tombstoned ordinary object cannot be committed, reactivated, added to a new grant or issued a new transfer;
  physical removal is confirmed separately after all recorded locators are absent.
- DuckLake path-shaped operations resolve an active governed row; neither the URL, key prefix nor dummy SigV4
  credential authenticates a caller or grants workspace access.
- DuckLake writes reserve the exact engine-generated key and activate only after streamed hash/size verification.
- DuckLake DELETE requires exact write-scope equality; the workspace read overlay never applies to deletion.
- DuckLake `DeleteObjects` authorizes the complete exact-key set in one row transaction before physical deletion;
  one unknown or differently scoped row rejects the whole batch with zero effects, while reserved and
  retry-deleted rows are accepted for crash recovery.

## Rationale

The prefix broker successfully removed S3 credentials from tenant pods and enforced cross-application buckets,
but it could not represent application workspace ownership or safely delegate a subset of objects to a narrow
background controller. Governed rows preserve the existing direct S3 data path while making authorization
explicit, queryable and testable. The application remains the source of its own Workspace and Membership truth;
the shared broker enforces the resulting partition without becoming coupled to every tenant schema.
