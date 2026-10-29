package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"
)

func TestPositiveDecisionCacheCachesOnlySuccessUntilExpiry(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	cache := newPositiveDecisionCache(30*time.Second, func() time.Time { return now })
	calls := 0
	decision := errors.New("transient")
	decide := func() error {
		calls++
		return decision
	}

	if err := cache.verify("acme\nacme-files", decide); !errors.Is(err, decision) || calls != 1 {
		t.Fatalf("first failing decision = %v after %d calls", err, calls)
	}
	if err := cache.verify("acme\nacme-files", decide); !errors.Is(err, decision) || calls != 2 {
		t.Fatalf("failure was cached: %v after %d calls", err, calls)
	}
	decision = nil
	if err := cache.verify("acme\nacme-files", decide); err != nil || calls != 3 {
		t.Fatalf("successful decision = %v after %d calls", err, calls)
	}
	now = now.Add(29 * time.Second)
	if err := cache.verify("acme\nacme-files", decide); err != nil || calls != 3 {
		t.Fatalf("cached success = %v after %d calls", err, calls)
	}
	if err := cache.verify("atlas\nacme-files", decide); err != nil || calls != 4 {
		t.Fatalf("decision leaked across keys: %v after %d calls", err, calls)
	}
	now = now.Add(time.Second)
	decision = ErrGovernedConflict
	if err := cache.verify("acme\nacme-files", decide); !errors.Is(err, ErrGovernedConflict) || calls != 5 {
		t.Fatalf("expired success was not re-evaluated: %v after %d calls", err, calls)
	}
}

func TestPostgresGovernedStoreReservationsDoNotExcludeEachOther(t *testing.T) {
	ctx := context.Background()
	db, store := newConcurrencyTestStore(t, "storage_broker_parallel_reserve")
	if err := store.ClaimBucketOwnership(ctx, "acme", "acme-files"); err != nil {
		t.Fatal(err)
	}

	// One reservation stays open in the middle of its transaction, holding the
	// shared bucket barrier. Every other reservation, ordinary and grant output,
	// must still complete without waiting for it and without serialization
	// failures, even though they all write the same workspace's locator range.
	readable := registerStoreObject(t, ctx, store, "acme", WorkspaceStorageScope(workspaceA), "grant-input")
	releaseHeld, heldDone := holdReservationOpen(t, ctx, store, "acme", "acme-files")
	defer releaseHeld()

	const ordinaryReservations = 24
	const grantOutputs = 12
	grants := make([]ExecutionGrant, grantOutputs)
	for index := range grants {
		grant, err := store.IssueGrant(ctx, IssueGrantInput{
			Tenant: "acme", Scope: WorkspaceStorageScope(workspaceA),
			ExecutionID:             mustUUID(t),
			ReadableObjectIDs:       []string{readable.ID},
			WritableArtifactClasses: []ArtifactClass{ArtifactSandboxRelation},
			IssuerServiceAccount:    "transformation-storage-authority",
			ExpiresAt:               time.Now().Add(time.Hour), CleanupOwnerID: mustUUID(t),
		})
		if err != nil {
			t.Fatal(err)
		}
		grants[index] = grant
	}

	start := make(chan struct{})
	errs := make(chan error, ordinaryReservations+grantOutputs)
	var wg sync.WaitGroup
	for index := 0; index < ordinaryReservations; index++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := store.ReserveObject(ctx, testReservation(t, "acme", "acme-files"),
				func(GovernedObject) error { return nil })
			errs <- err
		}()
	}
	for _, grant := range grants {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := store.ReserveGrantOutput(ctx, ReserveGrantOutputInput{
				Tenant: "acme", GrantID: grant.ID, ExecutionID: grant.ExecutionID,
				ArtifactClass: ArtifactSandboxRelation, ContentHash: testContentHash,
				ByteSize: 12, MediaType: "application/parquet", Bucket: "acme-files", Now: time.Now(),
			}, func(GovernedObject) error { return nil })
			errs <- err
		}()
	}
	close(start)
	finished := make(chan struct{})
	go func() {
		wg.Wait()
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("concurrent reservations waited on an open reservation")
	}
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent reservation failed: %v", err)
		}
	}
	releaseHeld()
	if err := <-heldDone; err != nil {
		t.Fatalf("held reservation failed: %v", err)
	}
	var reserved int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM "`+store.schema[1:len(store.schema)-1]+
		`".storage_objects WHERE state = 'reserved'`).Scan(&reserved); err != nil {
		t.Fatal(err)
	}
	if reserved != ordinaryReservations+grantOutputs+1 {
		t.Fatalf("reserved rows = %d, want %d", reserved, ordinaryReservations+grantOutputs+1)
	}
}

func TestPostgresGovernedStoreReservationIssuerFailureRollsBack(t *testing.T) {
	ctx := context.Background()
	db, store := newConcurrencyTestStore(t, "storage_broker_issuer_rollback")
	input := testReservation(t, "acme", "acme-files")
	issueErr := errors.New("signing failed")
	if _, err := store.ReserveObject(ctx, input, func(GovernedObject) error { return issueErr }); !errors.Is(err, issueErr) {
		t.Fatalf("reservation error = %v, want issuer error", err)
	}
	var rows int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM "`+store.schema[1:len(store.schema)-1]+
		`".storage_objects WHERE physical_key = $1`, input.Key).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("reservation without a ticket was committed: %d rows", rows)
	}
}

func TestPostgresGovernedStoreCleanupBarrierOrdersReservationTickets(t *testing.T) {
	ctx := context.Background()
	db, store := newConcurrencyTestStore(t, "storage_broker_cleanup_order")
	scope := WorkspaceStorageScope(workspaceA)

	// Cleanup first: the exclusive session barrier (the same lock an older
	// replica takes for its reservations) keeps a new reservation from writing
	// or signing anything until cleanup releases it.
	releaseCleanup, err := store.AcquireRegistrationBarrier(ctx, "acme", "acme-files")
	if err != nil {
		t.Fatal(err)
	}
	var signed sync.Mutex
	signedCalls := 0
	reservationDone := make(chan error, 1)
	input := testReservation(t, "acme", "acme-files")
	go func() {
		_, err := store.ReserveObject(ctx, input, func(GovernedObject) error {
			signed.Lock()
			signedCalls++
			signed.Unlock()
			return nil
		})
		reservationDone <- err
	}()
	select {
	case err := <-reservationDone:
		releaseCleanup()
		t.Fatalf("reservation did not wait for cleanup barrier: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	signed.Lock()
	signedBeforeRelease := signedCalls
	signed.Unlock()
	if signedBeforeRelease != 0 {
		releaseCleanup()
		t.Fatal("upload ticket was signed while cleanup held the barrier")
	}
	releaseCleanup()
	if err := <-reservationDone; err != nil || signedCalls != 1 {
		t.Fatalf("reservation after cleanup = %v, signed %d", err, signedCalls)
	}

	// Reservation first: cleanup cannot tombstone until the reservation (and so
	// its ticket signature) has committed, so the recorded revocation time is
	// never earlier than the ticket's signing time.
	var signedAt time.Time
	held := make(chan struct{})
	proceed := make(chan struct{})
	reservationDone = make(chan error, 1)
	go func() {
		_, err := store.ReserveObject(ctx, testReservation(t, "acme", "acme-files"),
			func(GovernedObject) error {
				signedAt = time.Now()
				close(held)
				<-proceed
				return nil
			})
		reservationDone <- err
	}()
	<-held
	type cleanupResult struct {
		revokedAt time.Time
		err       error
	}
	cleanupDone := make(chan cleanupResult, 1)
	go func() {
		release, err := store.AcquireRegistrationBarrier(ctx, "acme", "acme-files")
		if err != nil {
			cleanupDone <- cleanupResult{err: err}
			return
		}
		defer release()
		revokedAt := time.Now()
		cleanupDone <- cleanupResult{revokedAt: revokedAt,
			err: store.TombstoneReservations(ctx, "acme", scope, revokedAt)}
	}()
	select {
	case result := <-cleanupDone:
		close(proceed)
		t.Fatalf("cleanup did not wait for in-flight reservation: %v", result.err)
	case <-time.After(150 * time.Millisecond):
	}
	close(proceed)
	if err := <-reservationDone; err != nil {
		t.Fatal(err)
	}
	result := <-cleanupDone
	if result.err != nil {
		t.Fatal(result.err)
	}
	if result.revokedAt.Before(signedAt) {
		t.Fatalf("revocation %s precedes ticket signing %s", result.revokedAt, signedAt)
	}
	var live int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM "`+store.schema[1:len(store.schema)-1]+
		`".storage_objects WHERE state = 'reserved'`).Scan(&live); err != nil {
		t.Fatal(err)
	}
	if live != 0 {
		t.Fatalf("cleanup missed %d committed reservations", live)
	}
}

func TestPostgresGovernedStoreCachesOnlyPositiveBucketDecisions(t *testing.T) {
	ctx := context.Background()
	db, store := newConcurrencyTestStore(t, "storage_broker_ownership_cache")
	schema := store.schema[1 : len(store.schema)-1]
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	store.bucketClaims = newPositiveDecisionCache(bucketOwnershipCacheTTL, clock)
	store.bucketExclusivity = newPositiveDecisionCache(bucketOwnershipCacheTTL, clock)

	if err := store.VerifyBucketRegistration(ctx, "acme", "acme-files"); err != nil {
		t.Fatal(err)
	}
	if err := store.VerifyBucketRegistration(ctx, "atlas", "acme-files"); !errors.Is(err, ErrGovernedConflict) {
		t.Fatalf("foreign registration = %v, want conflict", err)
	}
	// Tamper with the insert-only owner row outside the broker. Within the TTL a
	// replica keeps its positive answer without querying; after it, the change is
	// observed. Denials are never cached, so the new owner is accepted at once.
	if _, err := db.ExecContext(ctx, `UPDATE "`+schema+`".storage_bucket_owners
		SET tenant_id = 'atlas' WHERE physical_bucket = 'acme-files'`); err != nil {
		t.Fatal(err)
	}
	if err := store.VerifyBucketRegistration(ctx, "acme", "acme-files"); err != nil {
		t.Fatalf("cached registration decision = %v", err)
	}
	if err := store.ClaimBucketOwnership(ctx, "acme", "acme-files"); err != nil {
		t.Fatalf("cached ownership claim = %v", err)
	}
	if err := store.VerifyBucketRegistration(ctx, "atlas", "acme-files"); err != nil {
		t.Fatalf("denial was cached: %v", err)
	}
	now = now.Add(bucketOwnershipCacheTTL)
	if err := store.VerifyBucketRegistration(ctx, "acme", "acme-files"); !errors.Is(err, ErrGovernedConflict) {
		t.Fatalf("expired registration decision = %v, want conflict", err)
	}
	if err := store.ClaimBucketOwnership(ctx, "acme", "acme-files"); !errors.Is(err, ErrGovernedConflict) {
		t.Fatalf("expired ownership claim = %v, want conflict", err)
	}
}

func TestPostgresGovernedStoreExclusivityFindsForeignRowsOnBothSides(t *testing.T) {
	ctx := context.Background()
	_, store := newConcurrencyTestStore(t, "storage_broker_exclusivity")
	if err := store.ClaimBucketOwnership(ctx, "acme", "acme-files"); err != nil {
		t.Fatal(err)
	}
	registerStoreObjectInBucket(t, ctx, store, "acme", "acme-files", "own")
	if exclusive, err := store.IsBucketExclusiveToTenant(ctx, "acme", "acme-files"); err != nil || !exclusive {
		t.Fatalf("own rows only = %t, %v", exclusive, err)
	}
	for _, foreign := range []string{"aaa", "zzz"} {
		t.Run(foreign, func(t *testing.T) {
			bucket := "shared-" + foreign
			if err := store.ClaimBucketOwnership(ctx, "acme", bucket); err != nil {
				t.Fatal(err)
			}
			registerStoreObjectInBucket(t, ctx, store, "acme", bucket, "own")
			registerStoreObjectInBucket(t, ctx, store, foreign, bucket, "foreign")
			if exclusive, err := store.IsBucketExclusiveToTenant(ctx, "acme", bucket); err != nil || exclusive {
				t.Fatalf("foreign tenant %q sorting around owner = %t, %v", foreign, exclusive, err)
			}
			if err := store.VerifyBucketRegistration(ctx, "acme", bucket); !errors.Is(err, ErrGovernedConflict) {
				t.Fatalf("registration into shared history = %v, want conflict", err)
			}
		})
	}
}

func newConcurrencyTestStore(t *testing.T, prefix string) (*sql.DB, *PostgresGovernedStore) {
	t.Helper()
	databaseURL := os.Getenv("STORAGE_BROKER_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("STORAGE_BROKER_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(64)
	db.SetMaxIdleConns(64)
	schema := fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
	if _, err := db.ExecContext(ctx, `CREATE SCHEMA "`+schema+`"`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := db.ExecContext(context.Background(), `DROP SCHEMA "`+schema+`" CASCADE`); err != nil {
			t.Errorf("drop test schema: %v", err)
		}
		_ = db.Close()
	})
	store, err := NewPostgresGovernedStore(db, schema)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return db, store
}

func testReservation(t *testing.T, tenant, bucket string) RegisterActiveObjectInput {
	t.Helper()
	scope := WorkspaceStorageScope(workspaceA)
	return RegisterActiveObjectInput{
		Tenant: tenant, Scope: scope, ArtifactClass: ArtifactTransformationSQL,
		ContentHash: testContentHash, ByteSize: 12, MediaType: "text/sql", Bucket: bucket,
		Key:            governedReservationKey(scope, ArtifactTransformationSQL, mustUUID(t)),
		CleanupOwnerID: "22222222-2222-4222-8222-222222222222",
	}
}

var errAbandonHeldReservation = errors.New("abandon held reservation")

// holdReservationOpen starts a reservation and parks it inside its transaction,
// after its row is written and while it holds the shared registration barrier.
// Releasing it commits the reservation.
func holdReservationOpen(
	t *testing.T,
	ctx context.Context,
	store *PostgresGovernedStore,
	tenant, bucket string,
) (release func(), done <-chan error) {
	return holdReservation(t, ctx, store, tenant, bucket, nil)
}

// holdReservationThenRollBack is holdReservationOpen whose release rolls the
// reservation back, so the row never becomes durable.
func holdReservationThenRollBack(
	t *testing.T,
	ctx context.Context,
	store *PostgresGovernedStore,
	tenant, bucket string,
) (release func(), done <-chan error) {
	return holdReservation(t, ctx, store, tenant, bucket, errAbandonHeldReservation)
}

func holdReservation(
	t *testing.T,
	ctx context.Context,
	store *PostgresGovernedStore,
	tenant, bucket string,
	issueErr error,
) (release func(), done <-chan error) {
	t.Helper()
	held := make(chan struct{})
	proceed := make(chan struct{})
	result := make(chan error, 1)
	input := testReservation(t, tenant, bucket)
	go func() {
		_, err := store.ReserveObject(ctx, input, func(GovernedObject) error {
			close(held)
			<-proceed
			return issueErr
		})
		if issueErr != nil && errors.Is(err, issueErr) {
			err = nil
		}
		result <- err
	}()
	select {
	case <-held:
	case err := <-result:
		t.Fatalf("reservation did not reach its open transaction: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("reservation did not reach its open transaction")
	}
	var once sync.Once
	return func() { once.Do(func() { close(proceed) }) }, result
}

// tryExclusiveAdvisoryLock reports whether cleanup could take key right now.
func tryExclusiveAdvisoryLock(t *testing.T, ctx context.Context, db *sql.DB, key string) bool {
	t.Helper()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var acquired bool
	if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock(hashtextextended($1, 0))`,
		key).Scan(&acquired); err != nil {
		t.Fatal(err)
	}
	if acquired {
		if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_unlock(hashtextextended($1, 0))`, key); err != nil {
			t.Fatal(err)
		}
	}
	return acquired
}

func registerStoreObjectInBucket(
	t *testing.T,
	ctx context.Context,
	store *PostgresGovernedStore,
	tenant, bucket, key string,
) {
	t.Helper()
	if _, err := store.RegisterActiveObject(ctx, RegisterActiveObjectInput{
		Tenant: tenant, Scope: TenantStorageScope(), ArtifactClass: ArtifactWorkBundle,
		ContentHash: testContentHash, ByteSize: 12, MediaType: "application/vnd.apache.parquet",
		Bucket: bucket, Key: "governed/test/" + key,
	}); err != nil {
		t.Fatal(err)
	}
}

func mustUUID(t *testing.T) string {
	t.Helper()
	id, err := newUUIDv4()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// productionPoolTestStore uses the broker's production pool size, so pool
// exhaustion behaves exactly as it would on a replica.
func productionPoolTestStore(t *testing.T, prefix string) (*sql.DB, *PostgresGovernedStore) {
	t.Helper()
	db, store := newConcurrencyTestStore(t, prefix)
	db.SetMaxOpenConns(16)
	db.SetMaxIdleConns(16)
	if err := store.ClaimBucketOwnership(context.Background(), "acme", "acme-files"); err != nil {
		t.Fatal(err)
	}
	return db, store
}

func startReservations(
	t *testing.T,
	ctx context.Context,
	store *PostgresGovernedStore,
	count int,
) <-chan error {
	t.Helper()
	results := make(chan error, count)
	for index := 0; index < count; index++ {
		input := testReservation(t, "acme", "acme-files")
		go func() {
			_, err := store.ReserveObject(ctx, input, func(GovernedObject) error { return nil })
			results <- err
		}()
	}
	return results
}

func awaitReservations(t *testing.T, results <-chan error, count int) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for index := 0; index < count; index++ {
		select {
		case err := <-results:
			if err != nil {
				t.Errorf("reservation after barrier release failed: %v", err)
			}
		case <-deadline:
			t.Fatalf("only %d of %d reservations finished after the barrier was released", index, count)
		}
	}
}

// Reservations arriving while cleanup holds the exclusive barrier must not
// occupy the pool connections cleanup needs for its own statements.
func TestPostgresGovernedStoreWaitingReservationsDoNotStarveCleanup(t *testing.T) {
	db, store := productionPoolTestStore(t, "storage_broker_starve_cleanup")
	ctx := context.Background()
	releaseCleanup, err := store.AcquireRegistrationBarrier(ctx, "acme", "acme-files")
	if err != nil {
		t.Fatal(err)
	}
	reserveCtx, cancelReservations := context.WithCancel(ctx)
	defer cancelReservations()
	results := startReservations(t, reserveCtx, store, 15)
	// The invariant is that a waiting reservation never blocks while holding a
	// connection: each attempt holds one only for BEGIN, a non-blocking try and
	// ROLLBACK. Pool occupancy is a noisy proxy (coinciding attempts can briefly
	// use many connections, none of them waiting), so assert the invariant
	// itself: no session ever waits on an advisory lock while cleanup holds the
	// barrier, and cleanup's own statements keep getting a connection promptly.
	// Tests in this package run sequentially, so every waiting session would be
	// one of these reservations.
	for attempt := 0; attempt < 5; attempt++ {
		if waiting := maxAdvisoryLockWaiters(t, db, 200*time.Millisecond); waiting != 0 {
			cancelReservations()
			releaseCleanup()
			t.Fatalf("%d reservations blocked on the barrier while holding a connection", waiting)
		}
		stepCtx, cancelStep := context.WithTimeout(ctx, time.Second)
		err = store.TombstoneReservations(stepCtx, "acme", WorkspaceStorageScope(workspaceA), time.Now())
		cancelStep()
		if err != nil {
			cancelReservations()
			releaseCleanup()
			t.Fatalf("cleanup statement %d under its barrier failed with pool %+v: %v", attempt, db.Stats(), err)
		}
	}
	releaseCleanup()
	awaitReservations(t, results, 15)
}

// register-existing (and Lake PUT) hold the shared barrier on a session
// connection and register on a second one. A cleanup queued for the exclusive
// barrier makes later reservations wait; they must not take that second
// connection, or no participant can ever finish.
func TestPostgresGovernedStoreQueuedCleanupDoesNotDeadlockRegisterExisting(t *testing.T) {
	db, store := productionPoolTestStore(t, "storage_broker_register_deadlock")
	ctx := context.Background()
	key := "governed/test/register-existing-behind-cleanup"
	releaseExisting, err := store.AcquireRegisterExistingLock(ctx, "acme", "acme-files", key)
	if err != nil {
		t.Fatal(err)
	}
	cleanupCtx, cancelCleanup := context.WithCancel(ctx)
	defer cancelCleanup()
	cleanupAcquired := make(chan func(), 1)
	cleanupErr := make(chan error, 1)
	go func() {
		release, err := store.AcquireRegistrationBarrier(cleanupCtx, "acme", "acme-files")
		if err != nil {
			cleanupErr <- err
			return
		}
		cleanupAcquired <- release
	}()
	time.Sleep(300 * time.Millisecond)
	reserveCtx, cancelReservations := context.WithCancel(ctx)
	defer cancelReservations()
	results := startReservations(t, reserveCtx, store, 14)
	time.Sleep(time.Second)

	stats := db.Stats()
	stepCtx, cancelStep := context.WithTimeout(ctx, 3*time.Second)
	_, err = store.RegisterActiveObject(stepCtx, RegisterActiveObjectInput{
		Tenant: "acme", Scope: TenantStorageScope(), ArtifactClass: ArtifactWorkBundle,
		ContentHash: testContentHash, ByteSize: 12, MediaType: "application/vnd.apache.parquet",
		Bucket: "acme-files", Key: key,
	})
	cancelStep()
	if err != nil {
		cancelReservations()
		cancelCleanup()
		releaseExisting()
		t.Fatalf("register-existing could not register under its shared barrier with pool %+v: %v", stats, err)
	}
	releaseExisting()
	select {
	case release := <-cleanupAcquired:
		release()
	case err := <-cleanupErr:
		t.Fatalf("queued cleanup failed: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("queued cleanup never acquired the barrier")
	}
	awaitReservations(t, results, 14)
}

func TestPostgresGovernedStoreMigrationRejectsForeignHistoryOnBothSides(t *testing.T) {
	for _, foreign := range []string{"aaa", "zzz"} {
		t.Run(foreign, func(t *testing.T) {
			ctx := context.Background()
			_, store := newConcurrencyTestStore(t, "storage_broker_migrate_history")
			if err := store.ClaimBucketOwnership(ctx, "acme", "acme-files"); err != nil {
				t.Fatal(err)
			}
			registerStoreObjectInBucket(t, ctx, store, "acme", "acme-files", "own")
			if err := store.Migrate(ctx); err != nil {
				t.Fatalf("migration over exclusive history: %v", err)
			}
			registerStoreObjectInBucket(t, ctx, store, foreign, "acme-files", "foreign")
			if err := store.Migrate(ctx); err == nil {
				t.Fatalf("migration accepted tenant %q's row in a bucket owned by acme", foreign)
			}
		})
	}
}

// maxAdvisoryLockWaiters samples, over duration, how many sessions in this
// database are blocked waiting for an advisory lock.
func maxAdvisoryLockWaiters(t *testing.T, db *sql.DB, duration time.Duration) int {
	t.Helper()
	peak := 0
	for deadline := time.Now().Add(duration); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		var waiting int
		err := db.QueryRowContext(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock' AND wait_event = 'advisory'`).Scan(&waiting)
		cancel()
		if err != nil {
			// A starved pool cannot run the probe either; report it as waiting.
			return db.Stats().InUse
		}
		peak = max(peak, waiting)
	}
	return peak
}

func TestPostgresGovernedStoreReservationGivesUpOnLongCleanup(t *testing.T) {
	db, store := productionPoolTestStore(t, "storage_broker_barrier_wait")
	ctx := context.Background()
	store.registrationBarrierWait = 300 * time.Millisecond
	readable := registerStoreObject(t, ctx, store, "acme", WorkspaceStorageScope(workspaceA), "grant-input")
	grant, err := store.IssueGrant(ctx, IssueGrantInput{
		Tenant: "acme", Scope: WorkspaceStorageScope(workspaceA), ExecutionID: mustUUID(t),
		ReadableObjectIDs: []string{readable.ID}, WritableArtifactClasses: []ArtifactClass{ArtifactSandboxRelation},
		IssuerServiceAccount: "transformation-storage-authority", ExpiresAt: time.Now().Add(time.Hour),
		CleanupOwnerID: mustUUID(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	cleanupRelease, err := store.AcquireRegistrationBarrier(ctx, "acme", "acme-files")
	if err != nil {
		t.Fatal(err)
	}
	var releaseOnce sync.Once
	releaseCleanup := func() { releaseOnce.Do(cleanupRelease) }
	defer releaseCleanup()

	// The request context never ends; only the server-side bound stops the wait.
	started := time.Now()
	_, reserveErr := store.ReserveObject(ctx, testReservation(t, "acme", "acme-files"),
		func(GovernedObject) error { return nil })
	_, outputErr := store.ReserveGrantOutput(ctx, ReserveGrantOutputInput{
		Tenant: "acme", GrantID: grant.ID, ExecutionID: grant.ExecutionID,
		ArtifactClass: ArtifactSandboxRelation, ContentHash: testContentHash, ByteSize: 12,
		MediaType: "application/parquet", Bucket: "acme-files", Now: time.Now(),
	}, func(GovernedObject) error { return nil })
	elapsed := time.Since(started)
	if !errors.Is(reserveErr, ErrRegistrationBusy) || !errors.Is(outputErr, ErrRegistrationBusy) {
		t.Fatalf("reservation errors = %v, %v; want ErrRegistrationBusy", reserveErr, outputErr)
	}
	if elapsed < 600*time.Millisecond || elapsed > 3*time.Second {
		t.Fatalf("two bounded waits took %s, want about 600ms", elapsed)
	}
	var rows int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM "`+store.schema[1:len(store.schema)-1]+
		`".storage_objects WHERE state = 'reserved'`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("timed-out reservations wrote %d rows", rows)
	}
	releaseCleanup()
	if _, err := store.ReserveObject(ctx, testReservation(t, "acme", "acme-files"),
		func(GovernedObject) error { return nil }); err != nil {
		t.Fatalf("reservation after cleanup: %v", err)
	}
}
