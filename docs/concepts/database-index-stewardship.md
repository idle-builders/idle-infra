# Database index stewardship

## Physical database index

**Definition.** A physical database index is a PostgreSQL-maintained access path whose entries are updated transactionally with its owning table.

**Essential properties.**

- PostgreSQL, not a scheduled application job, keeps index entries synchronized with table writes.
- Its definition belongs to the schema owner and exists to serve demonstrated query and constraint access patterns.
- Its observable condition includes validity, integrity, size, use, write cost, and the maintenance state of its owning table.
- Its evidence is meaningful only within a named statistics epoch and across every endpoint that can serve reads.

**What it is not.**

- **Not a derived projection.** A table or materialized view that copies and reshapes business data has application-defined freshness semantics.
- **Not periodically refreshed.** Routine `VACUUM`/`ANALYZE`, integrity checks, and an evidence-driven rebuild are different operations with different failure modes.
- **Not unused because one counter is zero.** Statistics reset on some restarts, replica reads have separate counters, rare paths may be important, and unique or constraint-backed indexes have integrity semantics even without scans.
- **Not platform-owned merely because PostgreSQL hosts it.** Tenant schema definitions and queries remain owned by the tenant application.

**Composition.** A physical index belongs to exactly one table or materialized view in one database and schema. It may also enforce a primary-key, unique, or exclusion constraint. Its usefulness is evaluated against normalized query identities, table write activity, planner behavior, and primary plus read-replica observations.

**Lifecycle.** Declare in an owning migration -> build -> PostgreSQL maintains entries -> observe across complete statistics epochs -> tune or rebuild only from evidence -> remove only through an owning migration after review.

**Authority.** PostgreSQL owns entry maintenance. The application repository owns tenant index definitions and query changes. The infra repository owns engine configuration, cross-database health evidence, integrity verification, and the reviewed operational procedure for rebuilding a damaged or demonstrably bloated index.

## Derived read projection

**Definition.** A derived read projection is an application-owned, rebuildable representation of business data optimized for a named read contract.

**Essential properties.**

- It declares its source data, projection definition or generation, freshness boundary, and rebuild procedure.
- Incremental updates are idempotent and durable; a full rebuild does not destroy the last usable generation before the replacement is complete.
- Its application exposes projection lag and readiness as health evidence without exposing private payloads.

**What it is not.**

- **Not a PostgreSQL index.** PostgreSQL cannot infer or maintain application-specific transformation semantics.
- **Not a generic infra CronJob.** The application image, release, schema, definition version, and user-facing degradation policy move together in the application repository.
- **Not globally unavailable because one row is late by default.** A fail-closed boundary must be an explicit product integrity requirement; otherwise stale rows are isolated or the previous complete generation remains active.

**Composition.** A projection attaches to one application read contract, consumes one or more application-owned sources, and is maintained by application-owned workers or deploy/migration jobs. Shared infrastructure supplies execution, database, and observation capacity but does not run tenant business transformations.

**Lifecycle.** Define generation -> build alongside the active generation -> validate completeness -> atomically activate -> incrementally maintain -> retire the previous generation after the rollback window.

**Authority.** The application repository owns projection code, schema, workers, and availability semantics. Infra may observe a stable health contract but cannot infer or mutate projection contents.

## Index health evidence

**Definition.** Index health evidence is a versioned, read-only observation that supports a specific structural, maintenance, integrity, or workload-fit decision without containing raw SQL or tenant data.

**Essential properties.**

- Carries database, endpoint role, schema, table, index, observation time, PostgreSQL start time, and statistics-reset epoch.
- Records counters rather than interpreting one absolute sample as a trend.
- Covers primary and any endpoint that serves reads.
- Separates structural failure, integrity failure, maintenance pressure, and workload-fit candidates.
- Retains enough bounded history to survive counter resets and justify a reviewed recommendation.

**What it is not.**

- **Not query text.** Normalized query IDs and aggregate cost are the shared default; text is enabled only for a short privileged diagnostic.
- **Not a deletion recommendation from `idx_scan = 0`.** Constraint role, observation horizon, write cost, replica use, and source access paths must also be reconciled.
- **Not an autonomous remediation instruction.** Evidence may fail a health check or open a recommendation; it does not create, drop, or rebuild indexes.

**Lifecycle.** Collect -> label with statistics epoch -> retain -> compute deltas -> classify -> route to the owning repository -> verify the reviewed change against the same evidence.

**Authority.** Infra owns collection, retention, integrity checks, and routing. Application owners decide access-path changes in their schemas.

## Vocabulary

| Term | Definition | Aliases not to use |
|---|---|---|
| **physical database index** | PostgreSQL-maintained table access path | refreshed index, search cache |
| **derived read projection** | Application-maintained, rebuildable business-data representation | database index, infra index |
| **index health evidence** | Epoch-aware observation used for an index decision | unused-index list, auto-tuning command |
| **index rebuild** | Evidence-driven recreation of a physical index | refresh |
| **projection rebuild** | Construction and activation of a new projection generation | reindex |

## Contracts

- Every physical index belongs to exactly one schema owner; infra never invents a tenant index definition.
- PostgreSQL maintains physical index contents synchronously; there is no periodic physical-index refresh job.
- Scheduled collection is read-only and excludes SQL text, credentials, parameters, and tenant data.
- Counter comparisons never cross a PostgreSQL start or statistics-reset epoch.
- Read-replica observations are included before classifying an index as unused.
- Primary-key, unique, exclusion, and constraint-supporting indexes are never automated removal candidates.
- A zero-scan index needs at least 30 days of retained, reset-aware evidence plus owning-code reconciliation before a removal proposal.
- Structural invalidity and integrity-check failures are critical evidence, not automatic permission to rebuild.
- `REINDEX CONCURRENTLY` is a reviewed repair for a named index, never a blanket schedule; storage headroom, WAL, replication lag, long transactions, and unsupported exclusion indexes are checked first.
- Application projections publish generation, readiness, queue depth or due count, and oldest lag through an application-owned health contract.
- A projection rebuild preserves the last complete generation until activation succeeds.
- Persistent optimization changes flow through the repository that owns the schema or projection.

## Rationale

The word `index` can mean a PostgreSQL access path or an application-owned read projection. Combining their lifecycles creates the wrong owner and operation. The platform supplies health evidence and reviewed database-integrity procedures; applications supply their own migrations and projection workers.

## Required reading

- `docs/concepts/platform.md` — platform, tenant, health-evidence, and remediation-policy authority.
- `terraform/modules/platform` — PostgreSQL topology and shared services.
- `skills/database/SKILL.md` — PostgreSQL observation and recovery procedures.
- `helm/values/postgresql.yaml` — shared engine, exporter, and autovacuum-related configuration.
- `scripts/postgresql-observe.sh` — current query-pressure evidence contract.
