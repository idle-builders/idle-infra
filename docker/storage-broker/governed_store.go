package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	ErrGovernedDenied   = errors.New("governed storage denied")
	ErrGovernedConflict = errors.New("governed storage identity conflict")
	// ErrRegistrationBusy means a registration gave up waiting for the bucket
	// registration barrier, which cleanup held for longer than the wait bound.
	// Nothing was written; the request is safe to retry.
	ErrRegistrationBusy = errors.New("governed registration barrier busy")
)

const (
	maxExecutionGrantLifetime          = 24 * time.Hour
	defaultConcurrentMutationLockLimit = 8
	// bucketOwnershipCacheTTL bounds how long a replica trusts a positive bucket
	// ownership decision without asking PostgreSQL again. Ownership rows are
	// insert-only, so a positive decision cannot become false through the broker.
	bucketOwnershipCacheTTL = 30 * time.Second
	// defaultRegistrationBarrierWait bounds how long one reservation retries for
	// the shared registration barrier. Request contexts carry no deadline, so
	// without it a reservation would retry for as long as cleanup holds the gate.
	defaultRegistrationBarrierWait = 60 * time.Second
)

type PostgresGovernedStore struct {
	db                      *sql.DB
	schema                  string
	mutationLockSlots       chan struct{}
	registrationBarrierWait time.Duration
	bucketClaims            *positiveDecisionCache
	bucketExclusivity       *positiveDecisionCache
}

var schemaNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)
var contentHashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func NewPostgresGovernedStore(db *sql.DB, schema string) (*PostgresGovernedStore, error) {
	if db == nil {
		return nil, fmt.Errorf("governed store database is required")
	}
	if !schemaNamePattern.MatchString(schema) {
		return nil, fmt.Errorf("invalid governed store schema")
	}
	lockLimit := defaultConcurrentMutationLockLimit
	if maxOpen := db.Stats().MaxOpenConnections; maxOpen > 0 {
		if maxOpen < 2 {
			return nil, fmt.Errorf("governed store database requires at least two open connections")
		}
		if lockLimit >= maxOpen {
			lockLimit = maxOpen - 1
		}
	}
	return &PostgresGovernedStore{
		db: db, schema: `"` + schema + `"`, mutationLockSlots: make(chan struct{}, lockLimit),
		registrationBarrierWait: defaultRegistrationBarrierWait,
		bucketClaims:            newPositiveDecisionCache(bucketOwnershipCacheTTL, time.Now),
		bucketExclusivity:       newPositiveDecisionCache(bucketOwnershipCacheTTL, time.Now),
	}, nil
}

// positiveDecisionCache remembers only successful, monotonic authorization
// facts for a bounded time. Failures and errors are never cached, so a denial
// or a transient database fault is always re-evaluated on the next request.
type positiveDecisionCache struct {
	ttl     time.Duration
	now     func() time.Time
	mu      sync.Mutex
	expires map[string]time.Time
}

func newPositiveDecisionCache(ttl time.Duration, now func() time.Time) *positiveDecisionCache {
	return &positiveDecisionCache{ttl: ttl, now: now, expires: make(map[string]time.Time)}
}

func (c *positiveDecisionCache) verify(key string, decide func() error) error {
	c.mu.Lock()
	expiresAt, cached := c.expires[key]
	c.mu.Unlock()
	if cached && c.now().Before(expiresAt) {
		return nil
	}
	if err := decide(); err != nil {
		return err
	}
	c.mu.Lock()
	c.expires[key] = c.now().Add(c.ttl)
	c.mu.Unlock()
	return nil
}

func (s *PostgresGovernedStore) Migrate(ctx context.Context) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS ` + s.schema + `.storage_bucket_owners (
			physical_bucket text PRIMARY KEY,
			tenant_id text NOT NULL,
			claimed_at timestamptz NOT NULL DEFAULT clock_timestamp()
		)`,
		`CREATE TABLE IF NOT EXISTS ` + s.schema + `.storage_objects (
			object_id uuid PRIMARY KEY,
			tenant_id text NOT NULL,
			authority_kind text NOT NULL CHECK (authority_kind IN ('tenant', 'workspace')),
			workspace_id uuid,
			artifact_class text NOT NULL,
			physical_bucket text NOT NULL,
			physical_key text NOT NULL,
			content_hash char(64) NOT NULL CHECK (content_hash ~ '^[0-9a-f]{64}$'),
			byte_size bigint NOT NULL CHECK (byte_size >= 0),
			media_type varchar(255) NOT NULL,
			state text NOT NULL CHECK (state IN ('reserved', 'active', 'deleted')),
			created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
			activated_at timestamptz,
			deleted_at timestamptz,
			CONSTRAINT storage_objects_scope_check CHECK (
				(authority_kind = 'tenant' AND workspace_id IS NULL) OR
				(authority_kind = 'workspace' AND workspace_id IS NOT NULL)
			),
			CONSTRAINT storage_objects_locator_unique UNIQUE (tenant_id, physical_bucket, physical_key)
		)`,
		`CREATE INDEX IF NOT EXISTS storage_objects_scope_idx ON ` + s.schema +
			`.storage_objects (tenant_id, authority_kind, workspace_id, state)`,
		`ALTER TABLE ` + s.schema + `.storage_objects
			ADD COLUMN IF NOT EXISTS staging_key text`,
		`ALTER TABLE ` + s.schema + `.storage_objects
			ADD COLUMN IF NOT EXISTS physical_removal_confirmed_at timestamptz`,
		`ALTER TABLE ` + s.schema + `.storage_objects
			ADD COLUMN IF NOT EXISTS legacy_staging_swept_at timestamptz`,
		`ALTER TABLE ` + s.schema + `.storage_objects
			ADD COLUMN IF NOT EXISTS reservation_cleanup_attempted_at timestamptz`,
		`ALTER TABLE ` + s.schema + `.storage_objects
			ADD COLUMN IF NOT EXISTS cleanup_owner_id uuid`,
		`CREATE INDEX IF NOT EXISTS storage_objects_cleanup_owner_idx ON ` + s.schema +
			`.storage_objects (tenant_id, authority_kind, workspace_id, cleanup_owner_id, state)
			WHERE artifact_class <> 'ducklake_data'`,
		`CREATE INDEX IF NOT EXISTS storage_objects_live_physical_key_idx ON ` + s.schema +
			`.storage_objects (tenant_id, physical_bucket, physical_key) WHERE state IN ('reserved','active')`,
		`CREATE INDEX IF NOT EXISTS storage_objects_live_staging_key_idx ON ` + s.schema +
			`.storage_objects (tenant_id, physical_bucket, staging_key) WHERE state IN ('reserved','active') AND staging_key IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS storage_objects_bucket_tenant_idx ON ` + s.schema +
			`.storage_objects (physical_bucket, tenant_id)`,
		// Tombstones are never purged, so cleanup's pending-removal reads would
		// otherwise revisit every deleted row of the scope on each retry.
		`CREATE INDEX IF NOT EXISTS storage_objects_pending_removal_idx ON ` + s.schema +
			`.storage_objects (tenant_id, authority_kind, workspace_id, cleanup_owner_id)
			WHERE state = 'deleted' AND physical_removal_confirmed_at IS NULL
			  AND artifact_class <> 'ducklake_data'`,
		// Lake inventory pages by object_id within one workspace. Without this the
		// cached generic plan walked the primary key across every tenant's rows.
		`CREATE INDEX IF NOT EXISTS storage_objects_lake_inventory_idx ON ` + s.schema +
			`.storage_objects (tenant_id, authority_kind, workspace_id, object_id)
			WHERE artifact_class = 'ducklake_data'`,
		`INSERT INTO ` + s.schema + `.storage_bucket_owners (physical_bucket, tenant_id)
			SELECT physical_bucket, MIN(tenant_id)
			FROM ` + s.schema + `.storage_objects
			GROUP BY physical_bucket
			ON CONFLICT (physical_bucket) DO NOTHING`,
		`UPDATE ` + s.schema + `.storage_objects
			SET staging_key = physical_key
			WHERE state = 'reserved' AND artifact_class <> 'ducklake_data' AND staging_key IS NULL`,
		`CREATE TABLE IF NOT EXISTS ` + s.schema + `.storage_execution_grants (
			grant_id uuid PRIMARY KEY,
			tenant_id text NOT NULL,
			authority_kind text NOT NULL CHECK (authority_kind IN ('tenant', 'workspace')),
			workspace_id uuid,
			execution_id uuid NOT NULL,
			request_hash char(64) NOT NULL CHECK (request_hash ~ '^[0-9a-f]{64}$'),
			issuer_service_account text NOT NULL,
			expires_at timestamptz NOT NULL,
			revoked_at timestamptz,
			created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
			CONSTRAINT storage_execution_grants_scope_check CHECK (
				(authority_kind = 'tenant' AND workspace_id IS NULL) OR
				(authority_kind = 'workspace' AND workspace_id IS NOT NULL)
			),
			CONSTRAINT storage_execution_grants_execution_unique UNIQUE (tenant_id, execution_id)
		)`,
		`ALTER TABLE ` + s.schema + `.storage_execution_grants
			ADD COLUMN IF NOT EXISTS cleanup_owner_id uuid`,
		// Cleanup revokes an owner's grants inside a serializable transaction. A
		// sequential scan there read every grant ever issued and took a
		// relation-wide predicate lock that conflicted with concurrent issuance.
		`CREATE INDEX IF NOT EXISTS storage_execution_grants_cleanup_owner_idx ON ` + s.schema +
			`.storage_execution_grants (tenant_id, authority_kind, workspace_id, cleanup_owner_id)`,
		`CREATE TABLE IF NOT EXISTS ` + s.schema + `.storage_execution_grant_objects (
			grant_id uuid NOT NULL REFERENCES ` + s.schema + `.storage_execution_grants(grant_id) ON DELETE CASCADE,
			object_id uuid NOT NULL REFERENCES ` + s.schema + `.storage_objects(object_id),
			PRIMARY KEY (grant_id, object_id)
		)`,
		`CREATE TABLE IF NOT EXISTS ` + s.schema + `.storage_execution_grant_output_classes (
			grant_id uuid NOT NULL REFERENCES ` + s.schema + `.storage_execution_grants(grant_id) ON DELETE CASCADE,
			artifact_class text NOT NULL,
			PRIMARY KEY (grant_id, artifact_class)
		)`,
		`CREATE TABLE IF NOT EXISTS ` + s.schema + `.storage_execution_grant_outputs (
			grant_id uuid NOT NULL REFERENCES ` + s.schema + `.storage_execution_grants(grant_id) ON DELETE CASCADE,
			object_id uuid NOT NULL REFERENCES ` + s.schema + `.storage_objects(object_id),
			artifact_class text NOT NULL,
			PRIMARY KEY (grant_id, object_id)
		)`,
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin governed migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(824677231)`); err != nil {
		return fmt.Errorf("lock governed migration: %w", err)
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("migrate governed store: %w", err)
		}
	}
	// Check each owned bucket with constant parameters so PostgreSQL can bound
	// the index on both sides of its owner. A single join on tenant_id <> owner
	// read every object row (7.4 s in production); the planner also mis-plans a
	// correlated form of the same ranges, so the loop is deliberate.
	conflictingBucketHistory, err := s.ownedBucketHistoryConflicts(ctx, tx)
	if err != nil {
		return fmt.Errorf("verify governed bucket ownership history: %w", err)
	}
	if conflictingBucketHistory {
		return fmt.Errorf("governed bucket ownership history contains more than one tenant")
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit governed migration: %w", err)
	}
	return nil
}

func (s *PostgresGovernedStore) ownedBucketHistoryConflicts(ctx context.Context, tx *sql.Tx) (bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT physical_bucket, tenant_id FROM `+s.schema+
		`.storage_bucket_owners ORDER BY physical_bucket`)
	if err != nil {
		return false, err
	}
	type owner struct{ bucket, tenant string }
	owners := make([]owner, 0)
	for rows.Next() {
		var o owner
		if err := rows.Scan(&o.bucket, &o.tenant); err != nil {
			_ = rows.Close()
			return false, err
		}
		owners = append(owners, o)
	}
	if err := rows.Close(); err != nil {
		return false, err
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	for _, o := range owners {
		var foreign bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (
			SELECT 1 FROM `+s.schema+`.storage_objects
			WHERE physical_bucket = $1 AND (tenant_id < $2 OR tenant_id > $2)
		)`, o.bucket, o.tenant).Scan(&foreign); err != nil {
			return false, err
		}
		if foreign {
			return true, nil
		}
	}
	return false, nil
}

func (s *PostgresGovernedStore) AcquireObjectLock(
	ctx context.Context,
	tenant, objectID string,
) (func(), error) {
	if !kubernetesName.MatchString(tenant) || !uuidPattern.MatchString(objectID) {
		return nil, ErrGovernedDenied
	}
	// Every deployed broker already takes this collision-proof internal key
	// (after the legacy "object\n..." key), so a replica that takes only the
	// internal key still serializes with an older replica during a rollout.
	return s.acquireMutationLocks(ctx, exclusiveLock(objectLockKey(tenant, objectID)))
}

func objectLockKey(tenant, objectID string) string {
	return internalLockKey("object", tenant, objectID)
}

func (s *PostgresGovernedStore) AcquireRegistrationBarrier(
	ctx context.Context,
	tenant, bucket string,
) (func(), error) {
	if !kubernetesName.MatchString(tenant) || bucket == "" {
		return nil, ErrGovernedDenied
	}
	return s.acquireMutationLocks(ctx, exclusiveLock(registrationBarrierKey(bucket)))
}

func (s *PostgresGovernedStore) AcquireRegistrationBarriers(
	ctx context.Context,
	tenant string,
	buckets []string,
) (func(), error) {
	if !kubernetesName.MatchString(tenant) || len(buckets) == 0 {
		return nil, ErrGovernedDenied
	}
	uniqueBuckets := append([]string(nil), buckets...)
	for _, bucket := range uniqueBuckets {
		if bucket == "" {
			return nil, ErrGovernedDenied
		}
	}
	sort.Strings(uniqueBuckets)
	uniqueBuckets = slices.Compact(uniqueBuckets)
	locks := make([]advisoryLock, 0, len(uniqueBuckets))
	for _, bucket := range uniqueBuckets {
		locks = append(locks, exclusiveLock(registrationBarrierKey(bucket)))
	}
	return s.acquireMutationLocks(ctx, locks...)
}

func (s *PostgresGovernedStore) AcquireRegisterExistingLock(
	ctx context.Context,
	tenant, bucket, key string,
) (func(), error) {
	if !kubernetesName.MatchString(tenant) || bucket == "" || validKey(key) != nil {
		return nil, ErrGovernedDenied
	}
	// Registrations share the bucket barrier with each other and exclude only
	// cleanup, which takes it exclusively. The physical key stays exclusive.
	return s.acquireMutationLocksTryFirst(ctx,
		sharedLock(registrationBarrierKey(bucket)), exclusiveLock(physicalKeyLockKey(tenant, bucket, key)))
}

func (s *PostgresGovernedStore) AcquireLakeRegistrationLock(
	ctx context.Context,
	tenant, bucket, key string,
) (func(), error) {
	if !kubernetesName.MatchString(tenant) || bucket == "" || validKey(key) != nil {
		return nil, ErrGovernedDenied
	}
	deployedKey := physicalKeyLockKey(tenant, bucket, key)
	pathKeys := []string{deployedKey, "lake\n" + deployedKey}
	sort.Strings(pathKeys)
	return s.acquireMutationLocks(ctx, sharedLock(registrationBarrierKey(bucket)),
		exclusiveLock(pathKeys[0]), exclusiveLock(pathKeys[1]))
}

// ClaimBucketOwnership proves that bucket belongs to tenant, claiming it when it
// is still unowned. Ownership rows are insert-only (the primary key admits one
// tenant per bucket and nothing updates or deletes them), so a positive answer
// is cached briefly; conflicts and errors are always re-evaluated.
func (s *PostgresGovernedStore) ClaimBucketOwnership(ctx context.Context, tenant, bucket string) error {
	if !kubernetesName.MatchString(tenant) || bucket == "" {
		return ErrGovernedDenied
	}
	return s.bucketClaims.verify(tenant+"\n"+bucket, func() error {
		return s.claimBucketOwnership(ctx, tenant, bucket)
	})
}

// VerifyBucketRegistration proves, before a new object row is created, that
// bucket belongs to tenant and that its persisted history holds no other
// tenant's rows. Both facts are monotonic once true: a foreign row can only be
// written after its tenant claims the bucket, which the immutable owner row
// refuses. A positive answer is therefore cached briefly. Physical cleanup
// never uses this cache; it calls IsBucketExclusiveToTenant under its barrier.
func (s *PostgresGovernedStore) VerifyBucketRegistration(ctx context.Context, tenant, bucket string) error {
	if !kubernetesName.MatchString(tenant) || bucket == "" {
		return ErrGovernedDenied
	}
	if err := s.ClaimBucketOwnership(ctx, tenant, bucket); err != nil {
		return err
	}
	return s.bucketExclusivity.verify(tenant+"\n"+bucket, func() error {
		exclusive, err := s.IsBucketExclusiveToTenant(ctx, tenant, bucket)
		if err != nil {
			return err
		}
		if !exclusive {
			return ErrGovernedConflict
		}
		return nil
	})
}

func (s *PostgresGovernedStore) claimBucketOwnership(ctx context.Context, tenant, bucket string) error {
	var owner string
	err := s.db.QueryRowContext(ctx, `SELECT tenant_id
		FROM `+s.schema+`.storage_bucket_owners
		WHERE physical_bucket = $1`, bucket).Scan(&owner)
	if err == nil {
		if owner != tenant {
			return ErrGovernedConflict
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO `+s.schema+`.storage_bucket_owners (
		physical_bucket, tenant_id
	) VALUES ($1, $2)
	ON CONFLICT (physical_bucket) DO NOTHING`, bucket, tenant); err != nil {
		return err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT tenant_id
		FROM `+s.schema+`.storage_bucket_owners
		WHERE physical_bucket = $1`, bucket).Scan(&owner); err != nil {
		return err
	}
	if owner != tenant {
		return ErrGovernedConflict
	}
	return nil
}

func (s *PostgresGovernedStore) TombstoneObject(
	ctx context.Context,
	tenant string,
	scope StorageScope,
	objectID string,
	now time.Time,
) (GovernedObjectDeletion, error) {
	if !kubernetesName.MatchString(tenant) || scope.Validate() != nil || !uuidPattern.MatchString(objectID) {
		return GovernedObjectDeletion{}, ErrGovernedDenied
	}
	query := `UPDATE ` + s.schema + `.storage_objects
	SET state = 'deleted', deleted_at = COALESCE(deleted_at, $5)
	WHERE tenant_id = $1 AND object_id = $2
	  AND authority_kind = $3 AND workspace_id IS NOT DISTINCT FROM $4
	  AND artifact_class <> 'ducklake_data'
	  AND state IN ('reserved','active','deleted')
	RETURNING object_id::text, tenant_id, authority_kind, workspace_id::text,
		artifact_class, content_hash, byte_size, media_type, state, physical_bucket, physical_key,
		COALESCE(staging_key, ''), deleted_at, legacy_staging_swept_at,
		reservation_cleanup_attempted_at, physical_removal_confirmed_at`
	deletion, err := scanGovernedObjectDeletion(s.db.QueryRowContext(ctx, query, tenant, objectID,
		scope.Kind, nullableWorkspace(scope), now.UTC()))
	if errors.Is(err, sql.ErrNoRows) {
		return GovernedObjectDeletion{}, ErrGovernedDenied
	}
	return deletion, err
}

func (s *PostgresGovernedStore) ReservationCleanupBuckets(
	ctx context.Context,
	tenant string,
	scope StorageScope,
) ([]string, error) {
	if !kubernetesName.MatchString(tenant) || scope.Validate() != nil {
		return nil, ErrGovernedDenied
	}
	selector, args := exactScopePredicate(tenant, scope)
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT physical_bucket
		FROM `+s.schema+`.storage_objects
		WHERE `+selector+`
		  AND artifact_class <> 'ducklake_data' AND activated_at IS NULL
		  AND (state = 'reserved' OR
		       (state = 'deleted' AND physical_removal_confirmed_at IS NULL))
		ORDER BY physical_bucket`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	buckets := make([]string, 0)
	for rows.Next() {
		var bucket string
		if err := rows.Scan(&bucket); err != nil {
			return nil, err
		}
		buckets = append(buckets, bucket)
	}
	return buckets, rows.Err()
}

func (s *PostgresGovernedStore) TombstoneReservations(
	ctx context.Context,
	tenant string,
	scope StorageScope,
	now time.Time,
) error {
	if !kubernetesName.MatchString(tenant) || scope.Validate() != nil {
		return ErrGovernedDenied
	}
	selector, args := exactScopePredicate(tenant, scope)
	args = append(args, now.UTC())
	_, err := s.db.ExecContext(ctx, `UPDATE `+s.schema+`.storage_objects
		SET state = 'deleted', deleted_at = COALESCE(deleted_at, `+placeholder(args)+`)
		WHERE `+selector+`
		  AND artifact_class <> 'ducklake_data'
		  AND activated_at IS NULL AND state = 'reserved'`, args...)
	return err
}

func (s *PostgresGovernedStore) PendingReservationDeletions(
	ctx context.Context,
	tenant string,
	scope StorageScope,
	limit int,
) ([]GovernedObjectDeletion, error) {
	if !kubernetesName.MatchString(tenant) || scope.Validate() != nil || limit < 1 {
		return nil, ErrGovernedDenied
	}
	selector, args := exactScopePredicate(tenant, scope)
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, `SELECT object_id::text, tenant_id, authority_kind, workspace_id::text,
		artifact_class, content_hash, byte_size, media_type, state, physical_bucket, physical_key,
		COALESCE(staging_key, ''), deleted_at, legacy_staging_swept_at,
		reservation_cleanup_attempted_at, physical_removal_confirmed_at
		FROM `+s.schema+`.storage_objects
		WHERE `+selector+`
		  AND artifact_class <> 'ducklake_data' AND activated_at IS NULL
		  AND state = 'deleted' AND physical_removal_confirmed_at IS NULL
		ORDER BY reservation_cleanup_attempted_at NULLS FIRST,
			reservation_cleanup_attempted_at, deleted_at, object_id
		LIMIT `+placeholder(args), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	deletions := make([]GovernedObjectDeletion, 0)
	for rows.Next() {
		deletion, err := scanGovernedObjectDeletion(rows)
		if err != nil {
			return nil, err
		}
		deletions = append(deletions, deletion)
	}
	return deletions, rows.Err()
}

func (s *PostgresGovernedStore) RecordReservationCleanupAttempt(
	ctx context.Context,
	tenant string,
	scope StorageScope,
	objectID string,
	now time.Time,
) error {
	if !kubernetesName.MatchString(tenant) || scope.Validate() != nil || !uuidPattern.MatchString(objectID) {
		return ErrGovernedDenied
	}
	result, err := s.db.ExecContext(ctx, `UPDATE `+s.schema+`.storage_objects
		SET reservation_cleanup_attempted_at = $5
		WHERE tenant_id = $1 AND object_id = $2
		  AND authority_kind = $3 AND workspace_id IS NOT DISTINCT FROM $4
		  AND artifact_class <> 'ducklake_data'
		  AND state = 'deleted' AND physical_removal_confirmed_at IS NULL`,
		tenant, objectID, scope.Kind, nullableWorkspace(scope), now.UTC())
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return ErrGovernedDenied
	}
	return nil
}

func (s *PostgresGovernedStore) ReservationCleanupStatus(
	ctx context.Context,
	tenant string,
	scope StorageScope,
) (int, *time.Time, error) {
	if !kubernetesName.MatchString(tenant) || scope.Validate() != nil {
		return 0, nil, ErrGovernedDenied
	}
	var pending int
	var oldest sql.NullTime
	selector, args := exactScopePredicate(tenant, scope)
	err := s.db.QueryRowContext(ctx, `SELECT count(*), min(deleted_at)
		FROM `+s.schema+`.storage_objects
		WHERE `+selector+`
		  AND artifact_class <> 'ducklake_data' AND activated_at IS NULL
		  AND state = 'deleted' AND physical_removal_confirmed_at IS NULL`,
		args...).Scan(&pending, &oldest)
	if err != nil {
		return 0, nil, err
	}
	if !oldest.Valid {
		return pending, nil, nil
	}
	value := oldest.Time
	return pending, &value, nil
}

func validateOrdinaryCleanupSelector(tenant string, scope StorageScope, ownerID string, allOwners bool) error {
	if !kubernetesName.MatchString(tenant) || scope.Validate() != nil {
		return ErrGovernedDenied
	}
	if allOwners {
		if ownerID != "" || scope.Kind != StorageScopeWorkspace {
			return ErrGovernedDenied
		}
		return nil
	}
	if !uuidPattern.MatchString(ownerID) {
		return ErrGovernedDenied
	}
	return nil
}

func (s *PostgresGovernedStore) OrdinaryCleanupBuckets(
	ctx context.Context, tenant string, scope StorageScope, ownerID string, allOwners bool,
) ([]string, error) {
	if err := validateOrdinaryCleanupSelector(tenant, scope, ownerID, allOwners); err != nil {
		return nil, err
	}
	selector, args := ordinaryCleanupPredicate(tenant, scope, ownerID, allOwners)
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT physical_bucket
		FROM `+s.schema+`.storage_objects
		WHERE `+selector+`
		  AND artifact_class <> 'ducklake_data'
		  AND (state IN ('reserved','active') OR
		       (state = 'deleted' AND physical_removal_confirmed_at IS NULL))
		ORDER BY physical_bucket`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	buckets := make([]string, 0)
	for rows.Next() {
		var bucket string
		if err := rows.Scan(&bucket); err != nil {
			return nil, err
		}
		buckets = append(buckets, bucket)
	}
	return buckets, rows.Err()
}

func (s *PostgresGovernedStore) TombstoneOrdinaryObjects(
	ctx context.Context, tenant string, scope StorageScope, ownerID string, allOwners bool, now time.Time,
) error {
	if err := validateOrdinaryCleanupSelector(tenant, scope, ownerID, allOwners); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	selector, args := ordinaryCleanupPredicate(tenant, scope, ownerID, allOwners)
	args = append(args, now.UTC())
	// Already-revoked grants are skipped rather than rewritten: cleanup is retried
	// until physical removal is confirmed, and each retry used to write a new
	// version of every grant the owner ever had.
	if _, err := tx.ExecContext(ctx, `UPDATE `+s.schema+`.storage_execution_grants
		SET revoked_at = `+placeholder(args)+`
		WHERE `+selector+` AND revoked_at IS NULL`, args...); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE `+s.schema+`.storage_objects
		SET state = 'deleted', deleted_at = COALESCE(deleted_at, `+placeholder(args)+`)
		WHERE `+selector+`
		  AND artifact_class <> 'ducklake_data'
		  AND state IN ('reserved','active')`, args...); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *PostgresGovernedStore) PendingOrdinaryObjectDeletions(
	ctx context.Context, tenant string, scope StorageScope, ownerID string, allOwners bool, limit int,
) ([]GovernedObjectDeletion, error) {
	if err := validateOrdinaryCleanupSelector(tenant, scope, ownerID, allOwners); err != nil || limit < 1 {
		return nil, ErrGovernedDenied
	}
	selector, args := ordinaryCleanupPredicate(tenant, scope, ownerID, allOwners)
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, `SELECT object_id::text, tenant_id, authority_kind, workspace_id::text,
		artifact_class, content_hash, byte_size, media_type, state, physical_bucket, physical_key,
		COALESCE(staging_key, ''), deleted_at, legacy_staging_swept_at,
		reservation_cleanup_attempted_at, physical_removal_confirmed_at
		FROM `+s.schema+`.storage_objects
		WHERE `+selector+`
		  AND artifact_class <> 'ducklake_data'
		  AND state = 'deleted' AND physical_removal_confirmed_at IS NULL
		ORDER BY reservation_cleanup_attempted_at NULLS FIRST,
			reservation_cleanup_attempted_at, deleted_at, object_id
		LIMIT `+placeholder(args), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	deletions := make([]GovernedObjectDeletion, 0)
	for rows.Next() {
		deletion, err := scanGovernedObjectDeletion(rows)
		if err != nil {
			return nil, err
		}
		deletions = append(deletions, deletion)
	}
	return deletions, rows.Err()
}

func (s *PostgresGovernedStore) OrdinaryCleanupStatus(
	ctx context.Context, tenant string, scope StorageScope, ownerID string, allOwners bool,
) (int, *time.Time, error) {
	if err := validateOrdinaryCleanupSelector(tenant, scope, ownerID, allOwners); err != nil {
		return 0, nil, err
	}
	var pending int
	var oldest sql.NullTime
	selector, args := ordinaryCleanupPredicate(tenant, scope, ownerID, allOwners)
	err := s.db.QueryRowContext(ctx, `SELECT count(*), min(deleted_at)
		FROM `+s.schema+`.storage_objects
		WHERE `+selector+`
		  AND artifact_class <> 'ducklake_data'
		  AND state = 'deleted' AND physical_removal_confirmed_at IS NULL`,
		args...).Scan(&pending, &oldest)
	if err != nil {
		return 0, nil, err
	}
	if !oldest.Valid {
		return pending, nil, nil
	}
	value := oldest.Time
	return pending, &value, nil
}

func (s *PostgresGovernedStore) RecordLegacyStagingSweep(
	ctx context.Context,
	tenant string,
	scope StorageScope,
	objectID string,
	now time.Time,
) (GovernedObjectDeletion, error) {
	if !kubernetesName.MatchString(tenant) || scope.Validate() != nil || !uuidPattern.MatchString(objectID) {
		return GovernedObjectDeletion{}, ErrGovernedDenied
	}
	query := `UPDATE ` + s.schema + `.storage_objects
	SET legacy_staging_swept_at = COALESCE(legacy_staging_swept_at, $5)
	WHERE tenant_id = $1 AND object_id = $2
	  AND authority_kind = $3 AND workspace_id IS NOT DISTINCT FROM $4
	  AND artifact_class <> 'ducklake_data' AND state = 'deleted' AND staging_key IS NULL
	RETURNING object_id::text, tenant_id, authority_kind, workspace_id::text,
		artifact_class, content_hash, byte_size, media_type, state, physical_bucket, physical_key,
		COALESCE(staging_key, ''), deleted_at, legacy_staging_swept_at,
		reservation_cleanup_attempted_at, physical_removal_confirmed_at`
	deletion, err := scanGovernedObjectDeletion(s.db.QueryRowContext(ctx, query, tenant, objectID,
		scope.Kind, nullableWorkspace(scope), now.UTC()))
	if errors.Is(err, sql.ErrNoRows) {
		return GovernedObjectDeletion{}, ErrGovernedDenied
	}
	return deletion, err
}

func (s *PostgresGovernedStore) ConfirmObjectRemoval(
	ctx context.Context,
	tenant string,
	scope StorageScope,
	objectID string,
	now time.Time,
) (GovernedObjectDeletion, error) {
	if !kubernetesName.MatchString(tenant) || scope.Validate() != nil || !uuidPattern.MatchString(objectID) {
		return GovernedObjectDeletion{}, ErrGovernedDenied
	}
	query := `UPDATE ` + s.schema + `.storage_objects
	SET physical_removal_confirmed_at = COALESCE(physical_removal_confirmed_at, $5)
	WHERE tenant_id = $1 AND object_id = $2
	  AND authority_kind = $3 AND workspace_id IS NOT DISTINCT FROM $4
	  AND artifact_class <> 'ducklake_data' AND state = 'deleted' AND staging_key IS NOT NULL
	RETURNING object_id::text, tenant_id, authority_kind, workspace_id::text,
		artifact_class, content_hash, byte_size, media_type, state, physical_bucket, physical_key,
		COALESCE(staging_key, ''), deleted_at, legacy_staging_swept_at,
		reservation_cleanup_attempted_at, physical_removal_confirmed_at`
	deletion, err := scanGovernedObjectDeletion(s.db.QueryRowContext(ctx, query, tenant, objectID,
		scope.Kind, nullableWorkspace(scope), now.UTC()))
	if errors.Is(err, sql.ErrNoRows) {
		return GovernedObjectDeletion{}, ErrGovernedDenied
	}
	return deletion, err
}

func (s *PostgresGovernedStore) IsLiveObjectKeyReferenced(
	ctx context.Context,
	tenant, bucket, key string,
) (bool, error) {
	if !kubernetesName.MatchString(tenant) || bucket == "" || validKey(key) != nil {
		return false, ErrGovernedDenied
	}
	var referenced bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM `+s.schema+`.storage_objects
		WHERE tenant_id = $1 AND physical_bucket = $2 AND state IN ('reserved','active')
		  AND (physical_key = $3 OR staging_key = $3)
	)`, tenant, bucket, key).Scan(&referenced)
	return referenced, err
}

func (s *PostgresGovernedStore) IsBucketExclusiveToTenant(
	ctx context.Context,
	tenant, bucket string,
) (bool, error) {
	if !kubernetesName.MatchString(tenant) || bucket == "" {
		return false, ErrGovernedDenied
	}
	// tenant_id <> $1 cannot bound an index scan, so it walked every row of the
	// bucket (~100ms at 500k rows). Two open ranges around $1 are equivalent for
	// a NOT NULL column and touch only foreign rows, which normally do not exist.
	var exclusive bool
	err := s.db.QueryRowContext(ctx, `SELECT
		EXISTS (
			SELECT 1 FROM `+s.schema+`.storage_bucket_owners
			WHERE physical_bucket = $2 AND tenant_id = $1
		) AND NOT EXISTS (
			SELECT 1 FROM `+s.schema+`.storage_objects
			WHERE physical_bucket = $2 AND (tenant_id < $1 OR tenant_id > $1)
		)`, tenant, bucket).Scan(&exclusive)
	return exclusive, err
}

func (s *PostgresGovernedStore) HasLiveObjectKeyUnderPrefix(
	ctx context.Context,
	tenant, bucket, prefix string,
) (bool, error) {
	if !kubernetesName.MatchString(tenant) || bucket == "" || prefix == "" || validKey(prefix+"probe") != nil {
		return false, ErrGovernedDenied
	}
	var referenced bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM `+s.schema+`.storage_objects
		WHERE tenant_id = $1 AND physical_bucket = $2 AND state IN ('reserved','active')
		  AND (starts_with(physical_key, $3) OR starts_with(COALESCE(staging_key, ''), $3))
	)`, tenant, bucket, prefix).Scan(&referenced)
	return referenced, err
}

type RegisterActiveObjectInput struct {
	Tenant         string
	Scope          StorageScope
	ArtifactClass  ArtifactClass
	ContentHash    string
	ByteSize       int64
	MediaType      string
	Bucket         string
	Key            string
	CleanupOwnerID string
}

func (s *PostgresGovernedStore) RegisterActiveObject(
	ctx context.Context,
	input RegisterActiveObjectInput,
) (GovernedObject, error) {
	if err := validateObjectRegistration(input); err != nil {
		return GovernedObject{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return GovernedObject{}, err
	}
	defer func() { _ = tx.Rollback() }()
	object, err := s.registerActiveObjectTx(ctx, tx, input)
	if err != nil {
		return GovernedObject{}, err
	}
	if err := tx.Commit(); err != nil {
		return GovernedObject{}, err
	}
	return object, nil
}

// ReservationIssuer runs after a reservation row is written and before that row
// commits, while the bucket registration barrier is still held. The broker signs
// the upload ticket here, so any cleanup that tombstones the row (and therefore
// must first take the barrier exclusively) records a revocation time no earlier
// than the ticket's signing time. Returning an error rolls the reservation back.
type ReservationIssuer func(GovernedObject) error

// ReserveObject creates an ordinary reserved row under a shared, transaction-
// scoped registration barrier. Reservations never exclude one another; only
// physical cleanup, which holds the barrier exclusively, excludes them.
//
// The transaction is READ COMMITTED on purpose. Its only statement besides the
// barrier is one INSERT ... ON CONFLICT, which is atomic at any isolation level.
// SERIALIZABLE would add page-granular predicate locks on the locator index, and
// concurrent reservations into one workspace share those pages, so they would
// abort each other with serialization failures once the barrier is shared.
func (s *PostgresGovernedStore) ReserveObject(
	ctx context.Context,
	input RegisterActiveObjectInput,
	issue ReservationIssuer,
) (GovernedObject, error) {
	return s.reserveObject(ctx, input, true, issue)
}

func (s *PostgresGovernedStore) reserveObject(
	ctx context.Context,
	input RegisterActiveObjectInput,
	holdRegistrationBarrier bool,
	issue ReservationIssuer,
) (GovernedObject, error) {
	if err := validateObjectRegistration(input); err != nil {
		return GovernedObject{}, err
	}
	if input.ArtifactClass != ArtifactDuckLakeData && !uuidPattern.MatchString(input.CleanupOwnerID) {
		return GovernedObject{}, fmt.Errorf("object cleanup owner ID is invalid")
	}
	var tx *sql.Tx
	var err error
	if holdRegistrationBarrier {
		tx, err = s.beginWithSharedRegistrationBarrier(ctx, input.Bucket)
	} else {
		tx, err = s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	}
	if err != nil {
		return GovernedObject{}, err
	}
	defer func() { _ = tx.Rollback() }()
	object, err := s.registerReservedObjectTx(ctx, tx, input)
	if err != nil {
		return GovernedObject{}, err
	}
	if issue != nil {
		if err := issue(object); err != nil {
			return GovernedObject{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return GovernedObject{}, err
	}
	return object, nil
}

const (
	registrationBarrierRetryMin = 5 * time.Millisecond
	registrationBarrierRetryMax = 200 * time.Millisecond
)

// beginWithSharedRegistrationBarrier opens a READ COMMITTED transaction that
// holds the bucket registration barrier in shared mode until it ends. The shared
// lock conflicts with cleanup's exclusive session-level barrier on every replica
// and never with other registrations.
//
// It never waits on the barrier while holding a pooled connection. PostgreSQL
// queues a new shared request behind a waiting exclusive one, so a blocking wait
// would let every reservation that arrives during cleanup park on a connection.
// They could then exhaust the pool that cleanup itself (and register-existing
// or Lake PUT, which hold the shared barrier on one connection and register on
// another) needs to finish, a circular wait PostgreSQL cannot detect. Instead
// it tries the lock, and on a conflict rolls back, returns the connection and
// retries with capped exponential backoff. Waiting reservations therefore hold
// no connection, and only the session-level lock holders remain bounded by
// mutationLockSlots. It gives up with ErrRegistrationBusy once
// registrationBarrierWait has elapsed, or earlier if the request context ends.
func (s *PostgresGovernedStore) beginWithSharedRegistrationBarrier(
	ctx context.Context,
	bucket string,
) (*sql.Tx, error) {
	if bucket == "" {
		return nil, ErrGovernedDenied
	}
	key := registrationBarrierKey(bucket)
	backoff := registrationBarrierRetryMin
	deadline := time.Now().Add(s.registrationBarrierWait)
	for {
		tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
		if err != nil {
			return nil, err
		}
		var acquired bool
		if err := tx.QueryRowContext(ctx,
			`SELECT pg_try_advisory_xact_lock_shared(hashtextextended($1, 0))`, key).Scan(&acquired); err != nil {
			_ = tx.Rollback()
			return nil, err
		}
		if acquired {
			return tx, nil
		}
		if err := tx.Rollback(); err != nil {
			return nil, err
		}
		delay := jitter(backoff)
		if remaining := time.Until(deadline); remaining <= 0 {
			return nil, ErrRegistrationBusy
		} else if delay > remaining {
			delay = remaining
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
		backoff = min(2*backoff, registrationBarrierRetryMax)
	}
}

// jitter spreads retries over [d/2, d) so waiters released together by one
// cleanup do not retry in lockstep.
func jitter(d time.Duration) time.Duration {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return d
	}
	half := d / 2
	return half + time.Duration(binary.LittleEndian.Uint64(b[:])%uint64(half))
}

func (s *PostgresGovernedStore) AuthorizeObjectReservation(
	ctx context.Context,
	tenant, objectID string,
) (GovernedObject, error) {
	if !kubernetesName.MatchString(tenant) || !uuidPattern.MatchString(objectID) {
		return GovernedObject{}, ErrGovernedDenied
	}
	query := `SELECT object_id::text, tenant_id, authority_kind, workspace_id::text,
		artifact_class, content_hash, byte_size, media_type, state, physical_bucket, physical_key
	FROM ` + s.schema + `.storage_objects
	WHERE tenant_id = $1 AND object_id = $2 AND state IN ('reserved','active')`
	object, err := scanGovernedObject(s.db.QueryRowContext(ctx, query, tenant, objectID))
	if errors.Is(err, sql.ErrNoRows) {
		return GovernedObject{}, ErrGovernedDenied
	}
	return object, err
}

func (s *PostgresGovernedStore) ActivateObject(
	ctx context.Context,
	tenant, objectID, bucket, key string,
) (GovernedObject, error) {
	if !kubernetesName.MatchString(tenant) || !uuidPattern.MatchString(objectID) ||
		bucket == "" || validKey(key) != nil {
		return GovernedObject{}, ErrGovernedDenied
	}
	query := `UPDATE ` + s.schema + `.storage_objects
	SET state = 'active', activated_at = COALESCE(activated_at, clock_timestamp()),
		physical_bucket = $3, physical_key = $4
	WHERE tenant_id = $1 AND object_id = $2 AND state IN ('reserved','active')
	RETURNING object_id::text, tenant_id, authority_kind, workspace_id::text,
		artifact_class, content_hash, byte_size, media_type, state, physical_bucket, physical_key`
	object, err := scanGovernedObject(s.db.QueryRowContext(ctx, query, tenant, objectID, bucket, key))
	if errors.Is(err, sql.ErrNoRows) {
		return GovernedObject{}, ErrGovernedDenied
	}
	return object, err
}

// AcquireLakePathLock serializes mutation of one physical locator across all
// broker replicas. Closing a sql.Conn returns it to the pool without releasing
// session advisory locks, so the explicit unlock is required.
func (s *PostgresGovernedStore) AcquireLakePathLock(
	ctx context.Context,
	tenant, bucket, key string,
) (func(), error) {
	if !kubernetesName.MatchString(tenant) || bucket == "" || validKey(key) != nil {
		return nil, ErrGovernedDenied
	}
	deployedKey := physicalKeyLockKey(tenant, bucket, key)
	intermediateKey := "lake\n" + deployedKey
	keys := []string{deployedKey, intermediateKey}
	sort.Strings(keys)
	return s.acquireMutationLocks(ctx, exclusiveLocks(keys)...)
}

func (s *PostgresGovernedStore) AcquireLakePathLocks(
	ctx context.Context,
	tenant, bucket string,
	keys []string,
) (func(), error) {
	if !kubernetesName.MatchString(tenant) || bucket == "" || len(keys) == 0 || len(keys) > maxLakeDeleteObjects {
		return nil, ErrGovernedDenied
	}
	lockKeys := make([]string, 0, len(keys)*2)
	seen := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		if validKey(key) != nil {
			return nil, ErrGovernedDenied
		}
		if _, duplicate := seen[key]; duplicate {
			return nil, ErrGovernedDenied
		}
		seen[key] = struct{}{}
		deployedKey := physicalKeyLockKey(tenant, bucket, key)
		lockKeys = append(lockKeys, deployedKey, "lake\n"+deployedKey)
	}
	sort.Strings(lockKeys)
	return s.acquireMutationLocks(ctx, exclusiveLocks(lockKeys)...)
}

func (s *PostgresGovernedStore) AcquirePhysicalKeyLock(
	ctx context.Context,
	tenant, bucket, key string,
) (func(), error) {
	if !kubernetesName.MatchString(tenant) || bucket == "" || validKey(key) != nil {
		return nil, ErrGovernedDenied
	}
	return s.acquireMutationLocks(ctx, exclusiveLock(physicalKeyLockKey(tenant, bucket, key)))
}

// advisoryLock is one session-level PostgreSQL advisory lock request. Shared
// and exclusive requests on the same key conflict across sessions and across
// session/transaction level, so mixed-mode replicas still exclude each other.
type advisoryLock struct {
	key    string
	shared bool
}

func exclusiveLock(key string) advisoryLock { return advisoryLock{key: key} }
func sharedLock(key string) advisoryLock    { return advisoryLock{key: key, shared: true} }

func exclusiveLocks(keys []string) []advisoryLock {
	locks := make([]advisoryLock, len(keys))
	for index, key := range keys {
		locks[index] = exclusiveLock(key)
	}
	return locks
}

func (l advisoryLock) statement(try bool) string {
	switch {
	case try && l.shared:
		return `SELECT pg_try_advisory_lock_shared(hashtextextended($1, 0))`
	case try:
		return `SELECT pg_try_advisory_lock(hashtextextended($1, 0))`
	case l.shared:
		return `SELECT pg_advisory_lock_shared(hashtextextended($1, 0))`
	default:
		return `SELECT pg_advisory_lock(hashtextextended($1, 0))`
	}
}

func (l advisoryLock) unlockStatement() string {
	if l.shared {
		return `SELECT pg_advisory_unlock_shared(hashtextextended($1, 0))`
	}
	return `SELECT pg_advisory_unlock(hashtextextended($1, 0))`
}

func (s *PostgresGovernedStore) acquireMutationLocks(ctx context.Context, locks ...advisoryLock) (func(), error) {
	return s.acquireMutationLocksMode(ctx, false, locks...)
}

func (s *PostgresGovernedStore) acquireMutationLocksTryFirst(
	ctx context.Context,
	locks ...advisoryLock,
) (func(), error) {
	return s.acquireMutationLocksMode(ctx, true, locks...)
}

// acquireMutationLocksMode holds session-level advisory locks on one dedicated
// pooled connection. It is only for mutations whose critical section spans
// object-storage I/O, which must not run inside an open transaction. Short
// database-only critical sections use pg_advisory_xact_lock* inside their own
// transaction instead and never come through here.
func (s *PostgresGovernedStore) acquireMutationLocksMode(
	ctx context.Context,
	tryFirst bool,
	locks ...advisoryLock,
) (func(), error) {
	select {
	case s.mutationLockSlots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		<-s.mutationLockSlots
		return nil, err
	}
	for index, lock := range locks {
		if tryFirst && index == 0 {
			var acquired bool
			if err := conn.QueryRowContext(ctx, lock.statement(true), lock.key).Scan(&acquired); err != nil {
				// The server may have acquired the session lock even when the result was
				// not delivered. Discard the connection rather than returning an
				// uncertain barrier lock to the pool.
				_ = conn.Raw(func(any) error { return driver.ErrBadConn })
				_ = conn.Close()
				<-s.mutationLockSlots
				return nil, err
			}
			if !acquired {
				_ = conn.Close()
				<-s.mutationLockSlots
				return nil, ErrGovernedConflict
			}
			continue
		}
		if _, err := conn.ExecContext(ctx, lock.statement(false), lock.key); err != nil {
			// Dropping the session releases any earlier locks acquired on it.
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
			_ = conn.Close()
			<-s.mutationLockSlots
			return nil, err
		}
	}
	return func() {
		defer func() { <-s.mutationLockSlots }()
		unlockCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		clean := true
		for index := len(locks) - 1; index >= 0; index-- {
			var unlocked bool
			if err := conn.QueryRowContext(unlockCtx, locks[index].unlockStatement(),
				locks[index].key).Scan(&unlocked); err != nil || !unlocked {
				clean = false
				break
			}
		}
		if !clean {
			// Never return a connection with a leaked session lock to the pool.
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
		_ = conn.Close()
	}, nil
}

func registrationBarrierKey(bucket string) string {
	return internalLockKey("registration", bucket)
}

func internalLockKey(family string, components ...string) string {
	// A colon cannot occur in a Kubernetes tenant name, so these internal lock
	// families cannot equal the deployed tenant\nbucket\nkey physical-path format.
	return "internal:" + family + "\n" + strings.Join(components, "\n")
}

func physicalKeyLockKey(tenant, bucket, key string) string {
	// validKey rejects control characters, so newlines make these components
	// unambiguous while preserving the pre-existing DuckLake lock key.
	return tenant + "\n" + bucket + "\n" + key
}

func (s *PostgresGovernedStore) ReserveLakeObject(
	ctx context.Context,
	input RegisterActiveObjectInput,
) (GovernedObject, error) {
	if input.ArtifactClass != ArtifactDuckLakeData {
		return GovernedObject{}, ErrGovernedDenied
	}
	// The Lake PUT caller already holds the shared barrier and both path locks on
	// a session connection. Taking the barrier again from this second connection
	// could queue behind a waiting cleanup that is itself waiting on the caller.
	return s.reserveObject(ctx, input, false, nil)
}

func (s *PostgresGovernedStore) ActivateLakeObject(
	ctx context.Context,
	tenant, objectID string,
) (GovernedObject, error) {
	if !kubernetesName.MatchString(tenant) || !uuidPattern.MatchString(objectID) {
		return GovernedObject{}, ErrGovernedDenied
	}
	query := `UPDATE ` + s.schema + `.storage_objects
	SET state = 'active', activated_at = COALESCE(activated_at, clock_timestamp())
	WHERE tenant_id = $1 AND object_id = $2 AND artifact_class = $3
	  AND state IN ('reserved','active')
	RETURNING object_id::text, tenant_id, authority_kind, workspace_id::text,
		artifact_class, content_hash, byte_size, media_type, state, physical_bucket, physical_key`
	object, err := scanGovernedObject(s.db.QueryRowContext(ctx, query, tenant, objectID, ArtifactDuckLakeData))
	if errors.Is(err, sql.ErrNoRows) {
		return GovernedObject{}, ErrGovernedDenied
	}
	return object, err
}

func (s *PostgresGovernedStore) ReadLakeObject(
	ctx context.Context,
	tenant string,
	scope StorageScope,
	bucket, key string,
) (GovernedObject, error) {
	if !kubernetesName.MatchString(tenant) || scope.Validate() != nil || bucket == "" || validKey(key) != nil {
		return GovernedObject{}, ErrGovernedDenied
	}
	query := `SELECT object_id::text, tenant_id, authority_kind, workspace_id::text,
		artifact_class, content_hash, byte_size, media_type, state, physical_bucket, physical_key
	FROM ` + s.schema + `.storage_objects
	WHERE tenant_id = $1 AND physical_bucket = $2 AND physical_key = $3
	  AND artifact_class = $4 AND state = 'active'
	  AND (
		($5 = 'tenant' AND authority_kind = 'tenant') OR
		($5 = 'workspace' AND (
			authority_kind = 'tenant' OR
			(authority_kind = 'workspace' AND workspace_id = $6)
		))
	  )`
	object, err := scanGovernedObject(s.db.QueryRowContext(ctx, query, tenant, bucket, key,
		ArtifactDuckLakeData, scope.Kind, nullableWorkspace(scope)))
	if errors.Is(err, sql.ErrNoRows) {
		return GovernedObject{}, ErrGovernedDenied
	}
	return object, err
}

func (s *PostgresGovernedStore) DeleteLakeObject(
	ctx context.Context,
	tenant string,
	scope StorageScope,
	bucket, key string,
	now time.Time,
) (GovernedObject, error) {
	if !kubernetesName.MatchString(tenant) || scope.Validate() != nil || bucket == "" || validKey(key) != nil {
		return GovernedObject{}, ErrGovernedDenied
	}
	query := `UPDATE ` + s.schema + `.storage_objects
	SET state = 'deleted', deleted_at = COALESCE(deleted_at, $7)
	WHERE tenant_id = $1 AND physical_bucket = $2 AND physical_key = $3
	  AND artifact_class = $4 AND authority_kind = $5
	  AND workspace_id IS NOT DISTINCT FROM $6
	  AND state IN ('reserved','active','deleted')
	RETURNING object_id::text, tenant_id, authority_kind, workspace_id::text,
		artifact_class, content_hash, byte_size, media_type, state, physical_bucket, physical_key`
	object, err := scanGovernedObject(s.db.QueryRowContext(ctx, query, tenant, bucket, key,
		ArtifactDuckLakeData, scope.Kind, nullableWorkspace(scope), now.UTC()))
	if errors.Is(err, sql.ErrNoRows) {
		return GovernedObject{}, ErrGovernedDenied
	}
	return object, err
}

func (s *PostgresGovernedStore) DeleteLakeObjects(
	ctx context.Context,
	tenant string,
	scope StorageScope,
	bucket string,
	keys []string,
	now time.Time,
) ([]GovernedObject, error) {
	if !kubernetesName.MatchString(tenant) || scope.Validate() != nil || bucket == "" ||
		len(keys) == 0 || len(keys) > maxLakeDeleteObjects {
		return nil, ErrGovernedDenied
	}
	seen := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		if validKey(key) != nil {
			return nil, ErrGovernedDenied
		}
		if _, duplicate := seen[key]; duplicate {
			return nil, ErrGovernedDenied
		}
		seen[key] = struct{}{}
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	query := `SELECT object_id::text, tenant_id, authority_kind, workspace_id::text,
		artifact_class, content_hash, byte_size, media_type, state, physical_bucket, physical_key
	FROM ` + s.schema + `.storage_objects
	WHERE tenant_id = $1 AND physical_bucket = $2 AND physical_key = ANY($3::text[])
	  AND artifact_class = $4 AND authority_kind = $5
	  AND workspace_id IS NOT DISTINCT FROM $6
	  AND state IN ('reserved','active','deleted')
	FOR UPDATE`
	rows, err := tx.QueryContext(ctx, query, tenant, bucket, keys, ArtifactDuckLakeData,
		scope.Kind, nullableWorkspace(scope))
	if err != nil {
		return nil, err
	}
	objectsByKey := make(map[string]GovernedObject, len(keys))
	for rows.Next() {
		object, scanErr := scanGovernedObject(rows)
		if scanErr != nil {
			_ = rows.Close()
			return nil, scanErr
		}
		objectsByKey[object.Key] = object
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(objectsByKey) != len(keys) {
		return nil, ErrGovernedDenied
	}

	result, err := tx.ExecContext(ctx, `UPDATE `+s.schema+`.storage_objects
		SET state = 'deleted', deleted_at = COALESCE(deleted_at, $7)
		WHERE tenant_id = $1 AND physical_bucket = $2 AND physical_key = ANY($3::text[])
		  AND artifact_class = $4 AND authority_kind = $5
		  AND workspace_id IS NOT DISTINCT FROM $6`,
		tenant, bucket, keys, ArtifactDuckLakeData, scope.Kind, nullableWorkspace(scope), now.UTC())
	if err != nil {
		return nil, err
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if rowsAffected != int64(len(keys)) {
		return nil, ErrGovernedDenied
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}

	objects := make([]GovernedObject, len(keys))
	for index, key := range keys {
		object := objectsByKey[key]
		object.State = ObjectStateDeleted
		objects[index] = object
	}
	return objects, nil
}

func (s *PostgresGovernedStore) ConfirmLakeObjectRemoval(
	ctx context.Context,
	tenant string,
	scope StorageScope,
	bucket, key string,
	now time.Time,
) error {
	if !kubernetesName.MatchString(tenant) || scope.Validate() != nil || bucket == "" || validKey(key) != nil {
		return ErrGovernedDenied
	}
	result, err := s.db.ExecContext(ctx, `UPDATE `+s.schema+`.storage_objects
		SET physical_removal_confirmed_at = COALESCE(physical_removal_confirmed_at, $7)
		WHERE tenant_id = $1 AND physical_bucket = $2 AND physical_key = $3
		  AND artifact_class = $4 AND authority_kind = $5
		  AND workspace_id IS NOT DISTINCT FROM $6 AND state = 'deleted'`,
		tenant, bucket, key, ArtifactDuckLakeData, scope.Kind, nullableWorkspace(scope), now.UTC())
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return ErrGovernedDenied
	}
	return nil
}

func (s *PostgresGovernedStore) InventoryLakeObjects(
	ctx context.Context,
	tenant string,
	scope StorageScope,
	afterObjectID string,
	limit int,
) ([]GovernedObject, bool, error) {
	if !kubernetesName.MatchString(tenant) || scope.Validate() != nil || scope.Kind != StorageScopeWorkspace ||
		(afterObjectID != "" && !uuidPattern.MatchString(afterObjectID)) || limit < 1 || limit > maxLakeDeleteObjects {
		return nil, false, ErrGovernedDenied
	}
	// The first page starts after the nil UUID, which no v4 object ID equals, so
	// every page is one bounded range on storage_objects_lake_inventory_idx. An
	// "$4 IS NULL OR object_id > $4" form cannot bound it in a generic plan.
	if afterObjectID == "" {
		afterObjectID = nilUUID
	}
	rows, err := s.db.QueryContext(ctx, `SELECT object_id::text, tenant_id, authority_kind, workspace_id::text,
		artifact_class, content_hash, byte_size, media_type, state, physical_bucket, physical_key
		FROM `+s.schema+`.storage_objects
		WHERE tenant_id = $1 AND authority_kind = $2 AND workspace_id = $3::uuid
		  AND artifact_class = 'ducklake_data'
		  AND object_id > $4::uuid
		  AND (state IN ('reserved','active') OR
		       (state = 'deleted' AND physical_removal_confirmed_at IS NULL))
		ORDER BY object_id LIMIT $5`, tenant, scope.Kind, scope.WorkspaceID,
		afterObjectID, limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	objects := make([]GovernedObject, 0, limit+1)
	for rows.Next() {
		object, err := scanGovernedObject(rows)
		if err != nil {
			return nil, false, err
		}
		objects = append(objects, object)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	hasMore := len(objects) > limit
	if hasMore {
		objects = objects[:limit]
	}
	return objects, hasMore, nil
}

func (s *PostgresGovernedStore) registerActiveObjectTx(
	ctx context.Context,
	tx *sql.Tx,
	input RegisterActiveObjectInput,
) (GovernedObject, error) {
	objectID, err := newUUIDv4()
	if err != nil {
		return GovernedObject{}, err
	}
	workspace := nullableWorkspace(input.Scope)
	query := `INSERT INTO ` + s.schema + `.storage_objects (
		object_id, tenant_id, authority_kind, workspace_id, artifact_class,
		physical_bucket, physical_key, content_hash, byte_size, media_type,
		state, activated_at, staging_key
	) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'active',clock_timestamp(),$11)
	ON CONFLICT (tenant_id, physical_bucket, physical_key) DO UPDATE
	SET physical_key = EXCLUDED.physical_key
	WHERE storage_objects.authority_kind = EXCLUDED.authority_kind
	  AND storage_objects.workspace_id IS NOT DISTINCT FROM EXCLUDED.workspace_id
	  AND storage_objects.artifact_class = EXCLUDED.artifact_class
	  AND storage_objects.content_hash = EXCLUDED.content_hash
	  AND storage_objects.byte_size = EXCLUDED.byte_size
	  AND storage_objects.media_type = EXCLUDED.media_type
	  AND storage_objects.state = 'active'
	RETURNING object_id::text, tenant_id, authority_kind, workspace_id::text,
		artifact_class, content_hash, byte_size, media_type, state, physical_bucket, physical_key`
	var stagingKey any
	if input.ArtifactClass != ArtifactDuckLakeData {
		// register-existing's physical key is exact and never came from a broker
		// upload ticket. Recording it prevents new rows from falling into the
		// unprovable legacy-staging recovery path.
		stagingKey = input.Key
	}
	row := tx.QueryRowContext(ctx, query, objectID, input.Tenant, input.Scope.Kind, workspace,
		input.ArtifactClass, input.Bucket, input.Key, input.ContentHash, input.ByteSize, input.MediaType, stagingKey)
	object, err := scanGovernedObject(row)
	if errors.Is(err, sql.ErrNoRows) {
		return GovernedObject{}, ErrGovernedConflict
	}
	return object, err
}

type IssueGrantInput struct {
	Tenant                  string
	Scope                   StorageScope
	ExecutionID             string
	ReadableObjectIDs       []string
	WritableArtifactClasses []ArtifactClass
	IssuerServiceAccount    string
	ExpiresAt               time.Time
	CleanupOwnerID          string
}

func (s *PostgresGovernedStore) IssueGrant(ctx context.Context, input IssueGrantInput) (ExecutionGrant, error) {
	if err := validateIssueGrantInput(input, time.Now()); err != nil {
		return ExecutionGrant{}, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return ExecutionGrant{}, err
	}
	defer func() { _ = tx.Rollback() }()

	objects, err := s.loadObjectsForGrant(ctx, tx, input.ReadableObjectIDs)
	if err != nil {
		return ExecutionGrant{}, err
	}
	if err := validateGrantObjects(input.Tenant, input.Scope, objects); err != nil {
		return ExecutionGrant{}, fmt.Errorf("%w: %v", ErrGovernedDenied, err)
	}
	requestHash, err := grantRequestHash(input)
	if err != nil {
		return ExecutionGrant{}, err
	}
	grantID, err := newUUIDv4()
	if err != nil {
		return ExecutionGrant{}, err
	}
	query := `INSERT INTO ` + s.schema + `.storage_execution_grants (
		grant_id, tenant_id, authority_kind, workspace_id, execution_id,
		request_hash, issuer_service_account, expires_at, cleanup_owner_id
	) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
	ON CONFLICT (tenant_id, execution_id) DO NOTHING
	RETURNING grant_id::text`
	var insertedID string
	err = tx.QueryRowContext(ctx, query, grantID, input.Tenant, input.Scope.Kind,
		nullableWorkspace(input.Scope), input.ExecutionID, requestHash,
		input.IssuerServiceAccount, input.ExpiresAt.UTC(), input.CleanupOwnerID).Scan(&insertedID)
	if errors.Is(err, sql.ErrNoRows) {
		existing, loadErr := s.loadGrantByExecution(ctx, tx, input.Tenant, input.ExecutionID)
		if loadErr != nil {
			return ExecutionGrant{}, loadErr
		}
		if existing.requestHash != requestHash {
			return ExecutionGrant{}, ErrGovernedConflict
		}
		if err := tx.Commit(); err != nil {
			return ExecutionGrant{}, err
		}
		return existing.ExecutionGrant, nil
	}
	if err != nil {
		return ExecutionGrant{}, err
	}
	// One statement per association table instead of one per row keeps grant
	// issuance at a constant number of round trips however many objects it names.
	if len(input.ReadableObjectIDs) > 0 {
		if _, err := tx.ExecContext(ctx, `INSERT INTO `+s.schema+
			`.storage_execution_grant_objects (grant_id, object_id)
			SELECT $1::uuid, object_id::uuid FROM unnest($2::text[]) AS object_id`,
			insertedID, input.ReadableObjectIDs); err != nil {
			return ExecutionGrant{}, err
		}
	}
	if len(input.WritableArtifactClasses) > 0 {
		classes := make([]string, len(input.WritableArtifactClasses))
		for index, artifactClass := range input.WritableArtifactClasses {
			classes[index] = string(artifactClass)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO `+s.schema+
			`.storage_execution_grant_output_classes (grant_id, artifact_class)
			SELECT $1::uuid, artifact_class FROM unnest($2::text[]) AS artifact_class`,
			insertedID, classes); err != nil {
			return ExecutionGrant{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return ExecutionGrant{}, err
	}
	return ExecutionGrant{
		SchemaVersion: 1, ID: insertedID, Tenant: input.Tenant, Scope: input.Scope, ExecutionID: input.ExecutionID,
		ReadableObjectIDs:       append([]string(nil), input.ReadableObjectIDs...),
		WritableArtifactClasses: append([]ArtifactClass(nil), input.WritableArtifactClasses...),
		ExpiresAt:               input.ExpiresAt.UTC(), CleanupOwnerID: input.CleanupOwnerID,
	}, nil
}

type ConsumeGrantObjectInput struct {
	Tenant      string
	GrantID     string
	ExecutionID string
	ObjectID    string
	Now         time.Time
}

func (s *PostgresGovernedStore) ConsumeGrantObject(
	ctx context.Context,
	input ConsumeGrantObjectInput,
) (GovernedObject, error) {
	if input.Tenant == "" || !uuidPattern.MatchString(input.GrantID) ||
		!uuidPattern.MatchString(input.ExecutionID) || !uuidPattern.MatchString(input.ObjectID) {
		return GovernedObject{}, ErrGovernedDenied
	}
	query := `SELECT o.object_id::text, o.tenant_id, o.authority_kind, o.workspace_id::text,
		o.artifact_class, o.content_hash, o.byte_size, o.media_type, o.state,
		o.physical_bucket, o.physical_key
	FROM ` + s.schema + `.storage_execution_grants g
	JOIN ` + s.schema + `.storage_execution_grant_objects go ON go.grant_id = g.grant_id
	JOIN ` + s.schema + `.storage_objects o ON o.object_id = go.object_id
	WHERE g.grant_id = $1 AND g.tenant_id = $2 AND g.execution_id = $3
	  AND g.revoked_at IS NULL AND g.expires_at > $4
	  AND o.object_id = $5 AND o.tenant_id = g.tenant_id AND o.state = 'active'
	  AND (
		(g.authority_kind = 'tenant' AND o.authority_kind = 'tenant') OR
		(g.authority_kind = 'workspace' AND (
			o.authority_kind = 'tenant' OR
			(o.authority_kind = 'workspace' AND o.workspace_id = g.workspace_id)
		))
	  )`
	object, err := scanGovernedObject(s.db.QueryRowContext(ctx, query, input.GrantID,
		input.Tenant, input.ExecutionID, input.Now.UTC(), input.ObjectID))
	if errors.Is(err, sql.ErrNoRows) {
		return GovernedObject{}, ErrGovernedDenied
	}
	return object, err
}

type ReserveGrantOutputInput struct {
	Tenant        string
	GrantID       string
	ExecutionID   string
	ArtifactClass ArtifactClass
	ContentHash   string
	ByteSize      int64
	MediaType     string
	Bucket        string
	Now           time.Time
}

func (s *PostgresGovernedStore) ReserveGrantOutput(
	ctx context.Context,
	input ReserveGrantOutputInput,
	issue ReservationIssuer,
) (GovernedObject, error) {
	if input.Tenant == "" || !uuidPattern.MatchString(input.GrantID) ||
		!uuidPattern.MatchString(input.ExecutionID) {
		return GovernedObject{}, ErrGovernedDenied
	}
	// READ COMMITTED is sufficient: the grant row is locked FOR UPDATE, so
	// revocation and activation serialize on it, and its readable-object and
	// output-class rows are written once with the grant and never change. Cleanup
	// that revokes grants holds the registration barrier exclusively, which the
	// shared barrier below waits for. See ReserveObject for why not SERIALIZABLE.
	tx, err := s.beginWithSharedRegistrationBarrier(ctx, input.Bucket)
	if err != nil {
		return GovernedObject{}, err
	}
	defer func() { _ = tx.Rollback() }()
	grant, err := s.loadUsableGrant(ctx, tx, input.Tenant, input.GrantID, input.ExecutionID, input.Now)
	if errors.Is(err, sql.ErrNoRows) {
		return GovernedObject{}, ErrGovernedDenied
	}
	if err != nil {
		return GovernedObject{}, err
	}
	if err := s.loadGrantOutputClasses(ctx, tx, &grant); err != nil {
		return GovernedObject{}, err
	}
	if err := validateOutput(grant.ExecutionGrant, input.ArtifactClass); err != nil {
		return GovernedObject{}, fmt.Errorf("%w: %v", ErrGovernedDenied, err)
	}
	reservationID, err := newUUIDv4()
	if err != nil {
		return GovernedObject{}, err
	}
	registration := RegisterActiveObjectInput{
		Tenant: input.Tenant, Scope: grant.Scope, ArtifactClass: input.ArtifactClass,
		ContentHash: input.ContentHash, ByteSize: input.ByteSize, MediaType: input.MediaType,
		Bucket: input.Bucket, Key: governedReservationKey(grant.Scope, input.ArtifactClass, reservationID),
		CleanupOwnerID: grant.CleanupOwnerID,
	}
	if err := validateObjectRegistration(registration); err != nil {
		return GovernedObject{}, err
	}
	object, err := s.registerReservedObjectTx(ctx, tx, registration)
	if err != nil {
		return GovernedObject{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO `+s.schema+
		`.storage_execution_grant_outputs (grant_id, object_id, artifact_class)
		VALUES ($1,$2,$3) ON CONFLICT (grant_id, object_id) DO NOTHING`,
		grant.ID, object.ID, input.ArtifactClass); err != nil {
		return GovernedObject{}, err
	}
	if issue != nil {
		if err := issue(object); err != nil {
			return GovernedObject{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return GovernedObject{}, err
	}
	return object, nil
}

type GrantOutputReservationInput struct {
	Tenant      string
	GrantID     string
	ExecutionID string
	ObjectID    string
	Now         time.Time
}

func (s *PostgresGovernedStore) AuthorizeGrantOutputReservation(
	ctx context.Context,
	input GrantOutputReservationInput,
) (GovernedObject, error) {
	if input.Tenant == "" || !uuidPattern.MatchString(input.GrantID) ||
		!uuidPattern.MatchString(input.ExecutionID) || !uuidPattern.MatchString(input.ObjectID) {
		return GovernedObject{}, ErrGovernedDenied
	}
	query := `SELECT o.object_id::text, o.tenant_id, o.authority_kind, o.workspace_id::text,
		o.artifact_class, o.content_hash, o.byte_size, o.media_type, o.state,
		o.physical_bucket, o.physical_key
	FROM ` + s.schema + `.storage_execution_grants g
	JOIN ` + s.schema + `.storage_execution_grant_outputs go ON go.grant_id = g.grant_id
	JOIN ` + s.schema + `.storage_objects o ON o.object_id = go.object_id
	WHERE g.grant_id = $1 AND g.tenant_id = $2 AND g.execution_id = $3
	  AND g.revoked_at IS NULL AND g.expires_at > $4
	  AND o.object_id = $5 AND o.tenant_id = g.tenant_id
	  AND o.artifact_class = go.artifact_class AND o.state IN ('reserved','active')
	  AND o.authority_kind = g.authority_kind
	  AND o.workspace_id IS NOT DISTINCT FROM g.workspace_id`
	object, err := scanGovernedObject(s.db.QueryRowContext(ctx, query, input.GrantID,
		input.Tenant, input.ExecutionID, input.Now.UTC(), input.ObjectID))
	if errors.Is(err, sql.ErrNoRows) {
		return GovernedObject{}, ErrGovernedDenied
	}
	return object, err
}

func (s *PostgresGovernedStore) ActivateGrantOutput(
	ctx context.Context,
	input GrantOutputReservationInput,
	bucket, key string,
) (GovernedObject, error) {
	if input.Tenant == "" || !uuidPattern.MatchString(input.GrantID) ||
		!uuidPattern.MatchString(input.ExecutionID) || !uuidPattern.MatchString(input.ObjectID) ||
		bucket == "" || validKey(key) != nil {
		return GovernedObject{}, ErrGovernedDenied
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return GovernedObject{}, err
	}
	defer func() { _ = tx.Rollback() }()
	grant, err := s.loadUsableGrant(ctx, tx, input.Tenant, input.GrantID, input.ExecutionID, input.Now)
	if errors.Is(err, sql.ErrNoRows) {
		return GovernedObject{}, ErrGovernedDenied
	}
	if err != nil {
		return GovernedObject{}, err
	}
	query := `UPDATE ` + s.schema + `.storage_objects o
	SET state = 'active', activated_at = COALESCE(activated_at, clock_timestamp()),
		physical_bucket = $6, physical_key = $7
	FROM ` + s.schema + `.storage_execution_grant_outputs go
	WHERE go.grant_id = $1 AND go.object_id = o.object_id
	  AND o.object_id = $2 AND o.tenant_id = $3
	  AND o.authority_kind = $4 AND o.workspace_id IS NOT DISTINCT FROM $5
	  AND o.artifact_class = go.artifact_class AND o.state IN ('reserved','active')
	RETURNING o.object_id::text, o.tenant_id, o.authority_kind, o.workspace_id::text,
		o.artifact_class, o.content_hash, o.byte_size, o.media_type, o.state,
		o.physical_bucket, o.physical_key`
	object, err := scanGovernedObject(tx.QueryRowContext(ctx, query, grant.ID, input.ObjectID,
		input.Tenant, grant.Scope.Kind, nullableWorkspace(grant.Scope), bucket, key))
	if errors.Is(err, sql.ErrNoRows) {
		return GovernedObject{}, ErrGovernedDenied
	}
	if err != nil {
		return GovernedObject{}, err
	}
	if err := tx.Commit(); err != nil {
		return GovernedObject{}, err
	}
	return object, nil
}

func (s *PostgresGovernedStore) registerReservedObjectTx(
	ctx context.Context,
	tx *sql.Tx,
	input RegisterActiveObjectInput,
) (GovernedObject, error) {
	objectID, err := newUUIDv4()
	if err != nil {
		return GovernedObject{}, err
	}
	query := `INSERT INTO ` + s.schema + `.storage_objects (
		object_id, tenant_id, authority_kind, workspace_id, artifact_class,
		physical_bucket, physical_key, content_hash, byte_size, media_type, state, staging_key, cleanup_owner_id
	) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'reserved',$11,$12)
	ON CONFLICT (tenant_id, physical_bucket, physical_key) DO UPDATE
	SET physical_key = EXCLUDED.physical_key
	WHERE storage_objects.authority_kind = EXCLUDED.authority_kind
	  AND storage_objects.workspace_id IS NOT DISTINCT FROM EXCLUDED.workspace_id
	  AND storage_objects.artifact_class = EXCLUDED.artifact_class
	  AND storage_objects.content_hash = EXCLUDED.content_hash
	  AND storage_objects.byte_size = EXCLUDED.byte_size
	  AND storage_objects.media_type = EXCLUDED.media_type
	  AND storage_objects.state IN ('reserved','active')
	RETURNING object_id::text, tenant_id, authority_kind, workspace_id::text,
		artifact_class, content_hash, byte_size, media_type, state, physical_bucket, physical_key`
	var stagingKey any
	if input.ArtifactClass != ArtifactDuckLakeData {
		stagingKey = input.Key
	}
	object, err := scanGovernedObject(tx.QueryRowContext(ctx, query, objectID, input.Tenant,
		input.Scope.Kind, nullableWorkspace(input.Scope), input.ArtifactClass, input.Bucket,
		input.Key, input.ContentHash, input.ByteSize, input.MediaType, stagingKey,
		nullableCleanupOwner(input.CleanupOwnerID)))
	if errors.Is(err, sql.ErrNoRows) {
		return GovernedObject{}, ErrGovernedConflict
	}
	return object, err
}

func (s *PostgresGovernedStore) RevokeGrant(ctx context.Context, tenant, grantID string, now time.Time) error {
	if tenant == "" || !uuidPattern.MatchString(grantID) {
		return ErrGovernedDenied
	}
	result, err := s.db.ExecContext(ctx, `UPDATE `+s.schema+
		`.storage_execution_grants SET revoked_at = COALESCE(revoked_at, $3)
		WHERE tenant_id = $1 AND grant_id = $2`, tenant, grantID, now.UTC())
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrGovernedDenied
	}
	return nil
}

func (s *PostgresGovernedStore) loadObjectsForGrant(
	ctx context.Context,
	tx *sql.Tx,
	objectIDs []string,
) ([]GovernedObject, error) {
	seen := make(map[string]struct{}, len(objectIDs))
	for _, objectID := range objectIDs {
		if !uuidPattern.MatchString(objectID) {
			return nil, fmt.Errorf("readable object ID is invalid")
		}
		if _, duplicate := seen[objectID]; duplicate {
			return nil, fmt.Errorf("readable object ID is duplicated")
		}
		seen[objectID] = struct{}{}
	}
	// One array parameter keeps a single statement text, so each pooled
	// connection prepares it once instead of once per distinct object count.
	query := `SELECT object_id::text, tenant_id, authority_kind, workspace_id::text,
		artifact_class, content_hash, byte_size, media_type, state, physical_bucket, physical_key
	FROM ` + s.schema + `.storage_objects WHERE object_id = ANY($1::uuid[])
	FOR SHARE`
	rows, err := tx.QueryContext(ctx, query, objectIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	objects := make([]GovernedObject, 0, len(objectIDs))
	for rows.Next() {
		object, err := scanGovernedObject(rows)
		if err != nil {
			return nil, err
		}
		objects = append(objects, object)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(objects) != len(objectIDs) {
		return nil, ErrGovernedDenied
	}
	return objects, nil
}

type storedGrant struct {
	ExecutionGrant
	requestHash string
}

func (s *PostgresGovernedStore) loadGrantByExecution(
	ctx context.Context,
	tx *sql.Tx,
	tenant, executionID string,
) (storedGrant, error) {
	query := `SELECT grant_id::text, tenant_id, authority_kind, workspace_id::text,
		execution_id::text, request_hash, expires_at, cleanup_owner_id::text
	FROM ` + s.schema + `.storage_execution_grants
	WHERE tenant_id = $1 AND execution_id = $2`
	grant, err := scanStoredGrant(tx.QueryRowContext(ctx, query, tenant, executionID))
	if err != nil {
		return storedGrant{}, err
	}
	if err := s.loadGrantAssociations(ctx, tx, &grant); err != nil {
		return storedGrant{}, err
	}
	return grant, nil
}

func (s *PostgresGovernedStore) loadUsableGrant(
	ctx context.Context,
	tx *sql.Tx,
	tenant, grantID, executionID string,
	now time.Time,
) (storedGrant, error) {
	query := `SELECT g.grant_id::text, g.tenant_id, g.authority_kind, g.workspace_id::text,
		g.execution_id::text, g.request_hash, g.expires_at, g.cleanup_owner_id::text
	FROM ` + s.schema + `.storage_execution_grants g
	WHERE g.tenant_id = $1 AND g.grant_id = $2 AND g.execution_id = $3
	  AND g.revoked_at IS NULL AND g.expires_at > $4
	FOR UPDATE OF g`
	var grant storedGrant
	var kind StorageScopeKind
	var workspace sql.NullString
	var cleanupOwner sql.NullString
	err := tx.QueryRowContext(ctx, query, tenant, grantID, executionID, now.UTC()).Scan(
		&grant.ID, &grant.Tenant, &kind, &workspace, &grant.ExecutionID, &grant.requestHash, &grant.ExpiresAt,
		&cleanupOwner,
	)
	if err != nil {
		return storedGrant{}, err
	}
	grant.Scope = storageScopeFromRow(kind, workspace)
	grant.CleanupOwnerID = cleanupOwner.String
	// Associations are left unloaded: output reservation needs only the output
	// classes and activation needs none, so neither reads up to 256 readable
	// object rows per request.
	return grant, nil
}

func (s *PostgresGovernedStore) loadGrantAssociations(
	ctx context.Context,
	tx *sql.Tx,
	grant *storedGrant,
) error {
	objectRows, err := tx.QueryContext(ctx, `SELECT object_id::text FROM `+s.schema+
		`.storage_execution_grant_objects WHERE grant_id = $1 ORDER BY object_id`, grant.ID)
	if err != nil {
		return err
	}
	for objectRows.Next() {
		var objectID string
		if err := objectRows.Scan(&objectID); err != nil {
			_ = objectRows.Close()
			return err
		}
		grant.ReadableObjectIDs = append(grant.ReadableObjectIDs, objectID)
	}
	if err := objectRows.Err(); err != nil {
		_ = objectRows.Close()
		return err
	}
	if err := objectRows.Close(); err != nil {
		return err
	}
	return s.loadGrantOutputClasses(ctx, tx, grant)
}

func (s *PostgresGovernedStore) loadGrantOutputClasses(
	ctx context.Context,
	tx *sql.Tx,
	grant *storedGrant,
) error {
	classRows, err := tx.QueryContext(ctx, `SELECT artifact_class FROM `+s.schema+
		`.storage_execution_grant_output_classes WHERE grant_id = $1 ORDER BY artifact_class`, grant.ID)
	if err != nil {
		return err
	}
	defer classRows.Close()
	for classRows.Next() {
		var artifactClass ArtifactClass
		if err := classRows.Scan(&artifactClass); err != nil {
			return err
		}
		grant.WritableArtifactClasses = append(grant.WritableArtifactClasses, artifactClass)
	}
	if err := classRows.Err(); err != nil {
		return err
	}
	return nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanGovernedObject(row rowScanner) (GovernedObject, error) {
	var object GovernedObject
	var kind StorageScopeKind
	var workspace sql.NullString
	err := row.Scan(&object.ID, &object.Tenant, &kind, &workspace, &object.ArtifactClass,
		&object.ContentHash, &object.ByteSize, &object.MediaType, &object.State, &object.Bucket, &object.Key)
	if err != nil {
		return GovernedObject{}, err
	}
	object.Scope = storageScopeFromRow(kind, workspace)
	return object, nil
}

func scanGovernedObjectDeletion(row rowScanner) (GovernedObjectDeletion, error) {
	var deletion GovernedObjectDeletion
	var kind StorageScopeKind
	var workspace sql.NullString
	var sweptAt, attemptedAt, confirmedAt sql.NullTime
	err := row.Scan(&deletion.Object.ID, &deletion.Object.Tenant, &kind, &workspace,
		&deletion.Object.ArtifactClass, &deletion.Object.ContentHash, &deletion.Object.ByteSize,
		&deletion.Object.MediaType, &deletion.Object.State, &deletion.Object.Bucket,
		&deletion.Object.Key, &deletion.StagingKey, &deletion.AccessRevokedAt, &sweptAt, &attemptedAt,
		&confirmedAt)
	if err != nil {
		return GovernedObjectDeletion{}, err
	}
	deletion.Object.Scope = storageScopeFromRow(kind, workspace)
	if sweptAt.Valid {
		swept := sweptAt.Time
		deletion.LegacyStagingSweptAt = &swept
	}
	if attemptedAt.Valid {
		attempted := attemptedAt.Time
		deletion.ReservationCleanupAttemptedAt = &attempted
	}
	if confirmedAt.Valid {
		confirmed := confirmedAt.Time
		deletion.PhysicalRemovalConfirmedAt = &confirmed
	}
	return deletion, nil
}

func scanStoredGrant(row rowScanner) (storedGrant, error) {
	var grant storedGrant
	var kind StorageScopeKind
	var workspace sql.NullString
	var cleanupOwner sql.NullString
	if err := row.Scan(&grant.ID, &grant.Tenant, &kind, &workspace, &grant.ExecutionID, &grant.requestHash,
		&grant.ExpiresAt, &cleanupOwner); err != nil {
		return storedGrant{}, err
	}
	grant.Scope = storageScopeFromRow(kind, workspace)
	grant.SchemaVersion = 1
	grant.CleanupOwnerID = cleanupOwner.String
	return grant, nil
}

func storageScopeFromRow(kind StorageScopeKind, workspace sql.NullString) StorageScope {
	if kind == StorageScopeWorkspace {
		return WorkspaceStorageScope(workspace.String)
	}
	return TenantStorageScope()
}

func nullableWorkspace(scope StorageScope) any {
	if scope.Kind == StorageScopeWorkspace {
		return scope.WorkspaceID
	}
	return nil
}

func nullableCleanupOwner(ownerID string) any {
	if ownerID == "" {
		return nil
	}
	return ownerID
}

// exactScopePredicate renders exact tenant/authority equality as conditions a
// btree can bound, numbering its placeholders from $1. workspace_id IS NOT
// DISTINCT FROM $n is never index-bounded, so cleanup queries written that way
// walked the tenant's whole authority range or the whole table. The scope check
// constraint makes "tenant authority" and "workspace_id IS NULL" equivalent.
func exactScopePredicate(tenant string, scope StorageScope) (string, []any) {
	if scope.Kind == StorageScopeWorkspace {
		return "tenant_id = $1 AND authority_kind = $2 AND workspace_id = $3::uuid",
			[]any{tenant, scope.Kind, scope.WorkspaceID}
	}
	return "tenant_id = $1 AND authority_kind = $2 AND workspace_id IS NULL", []any{tenant, scope.Kind}
}

// ordinaryCleanupPredicate adds the exact cleanup owner unless the selector is
// whole-workspace. A "$n::boolean OR cleanup_owner_id = $m" form cannot bound
// the owner index in a cached generic plan.
func ordinaryCleanupPredicate(tenant string, scope StorageScope, ownerID string, allOwners bool) (string, []any) {
	predicate, args := exactScopePredicate(tenant, scope)
	if allOwners {
		return predicate, args
	}
	args = append(args, ownerID)
	return predicate + " AND cleanup_owner_id = " + placeholder(args) + "::uuid", args
}

// placeholder names the last argument in args.
func placeholder(args []any) string {
	return fmt.Sprintf("$%d", len(args))
}

func validateObjectRegistration(input RegisterActiveObjectInput) error {
	if input.Tenant == "" || !kubernetesName.MatchString(input.Tenant) {
		return fmt.Errorf("object tenant is invalid")
	}
	if err := input.Scope.Validate(); err != nil {
		return err
	}
	if !validArtifactClass(input.ArtifactClass) {
		return fmt.Errorf("object artifact class is invalid")
	}
	if !contentHashPattern.MatchString(input.ContentHash) {
		return fmt.Errorf("object content hash is invalid")
	}
	if input.ByteSize < 0 {
		return fmt.Errorf("object byte size is invalid")
	}
	if input.MediaType == "" || len(input.MediaType) > 255 {
		return fmt.Errorf("object media type is invalid")
	}
	if input.Bucket == "" || len(input.Bucket) > 255 {
		return fmt.Errorf("object bucket is invalid")
	}
	if err := validKey(input.Key); err != nil {
		return fmt.Errorf("object key is invalid: %w", err)
	}
	return nil
}

func validateIssueGrantInput(input IssueGrantInput, now time.Time) error {
	if input.Tenant == "" || !kubernetesName.MatchString(input.Tenant) {
		return fmt.Errorf("grant tenant is invalid")
	}
	if err := input.Scope.Validate(); err != nil {
		return err
	}
	if !uuidPattern.MatchString(input.ExecutionID) {
		return fmt.Errorf("grant execution ID is invalid")
	}
	if !uuidPattern.MatchString(input.CleanupOwnerID) {
		return fmt.Errorf("grant cleanup owner ID is invalid")
	}
	if len(input.ReadableObjectIDs) == 0 || len(input.ReadableObjectIDs) > maxGrantReadableObjects {
		return fmt.Errorf("grant readable object count is invalid")
	}
	seenObjects := make(map[string]struct{}, len(input.ReadableObjectIDs))
	for _, objectID := range input.ReadableObjectIDs {
		if !uuidPattern.MatchString(objectID) {
			return fmt.Errorf("grant readable object ID is invalid")
		}
		if _, duplicate := seenObjects[objectID]; duplicate {
			return fmt.Errorf("grant readable object ID is duplicated")
		}
		seenObjects[objectID] = struct{}{}
	}
	if len(input.WritableArtifactClasses) > 8 {
		return fmt.Errorf("grant writable artifact class count is invalid")
	}
	seenClasses := make(map[ArtifactClass]struct{}, len(input.WritableArtifactClasses))
	for _, artifactClass := range input.WritableArtifactClasses {
		if !validGrantOutputClass(artifactClass) {
			return fmt.Errorf("grant output artifact class is invalid")
		}
		if _, duplicate := seenClasses[artifactClass]; duplicate {
			return fmt.Errorf("grant output artifact class is duplicated")
		}
		seenClasses[artifactClass] = struct{}{}
	}
	if !kubernetesName.MatchString(input.IssuerServiceAccount) {
		return fmt.Errorf("grant issuer service account is invalid")
	}
	if !input.ExpiresAt.After(now) || input.ExpiresAt.After(now.Add(maxExecutionGrantLifetime)) {
		return fmt.Errorf("grant expiry is invalid")
	}
	return nil
}

func validArtifactClass(artifactClass ArtifactClass) bool {
	switch artifactClass {
	case ArtifactNormalizedSource, ArtifactTransformationSQL, ArtifactWorkBundle,
		ArtifactCheckpoint, ArtifactRuleBundle, ArtifactTrace, ArtifactResult,
		ArtifactValidation, ArtifactEvidence, ArtifactAnalysisManifest, ArtifactAnalysisResult,
		ArtifactSandboxRelation, ArtifactSandboxDiagnostic,
		ArtifactDuckLakeData:
		return true
	default:
		return false
	}
}

func validGrantOutputClass(artifactClass ArtifactClass) bool {
	return artifactClass == ArtifactSandboxRelation || artifactClass == ArtifactSandboxDiagnostic
}

func governedObjectKey(
	scope StorageScope,
	artifactClass ArtifactClass,
	contentHash, objectID string,
) string {
	scopePath := "tenant"
	if scope.Kind == StorageScopeWorkspace {
		scopePath = "workspace/" + scope.WorkspaceID
	}
	return fmt.Sprintf("governed/%s/%s/sha256/%s/%s", scopePath, artifactClass, contentHash, objectID)
}

func governedReservationKey(
	scope StorageScope,
	artifactClass ArtifactClass,
	reservationID string,
) string {
	scopePath := "tenant"
	if scope.Kind == StorageScopeWorkspace {
		scopePath = "workspace/" + scope.WorkspaceID
	}
	return fmt.Sprintf("governed-staging/%s/%s/%s", scopePath, artifactClass, reservationID)
}

func governedStagingPrefix(scope StorageScope, artifactClass ArtifactClass) string {
	scopePath := "tenant"
	if scope.Kind == StorageScopeWorkspace {
		scopePath = "workspace/" + scope.WorkspaceID
	}
	return fmt.Sprintf("governed-staging/%s/%s/", scopePath, artifactClass)
}

func grantRequestHash(input IssueGrantInput) (string, error) {
	objectIDs := append([]string(nil), input.ReadableObjectIDs...)
	sort.Strings(objectIDs)
	classes := append([]ArtifactClass(nil), input.WritableArtifactClasses...)
	sort.Slice(classes, func(i, j int) bool { return classes[i] < classes[j] })
	payload := struct {
		Tenant         string
		Scope          StorageScope
		ExecutionID    string
		Objects        []string
		Classes        []ArtifactClass
		Issuer         string
		ExpiresAt      string
		CleanupOwnerID string
	}{input.Tenant, input.Scope, input.ExecutionID, objectIDs, classes,
		input.IssuerServiceAccount, input.ExpiresAt.UTC().Format(time.RFC3339Nano), input.CleanupOwnerID}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

const nilUUID = "00000000-0000-0000-0000-000000000000"

func newUUIDv4() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		value[0:4], value[4:6], value[6:8], value[8:10], value[10:16]), nil
}
