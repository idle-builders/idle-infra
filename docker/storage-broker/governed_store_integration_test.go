package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestPostgresGovernedStoreEnforcesGrantRows(t *testing.T) {
	databaseURL := os.Getenv("STORAGE_BROKER_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("STORAGE_BROKER_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	schema := fmt.Sprintf("storage_broker_test_%d", time.Now().UnixNano())
	if _, err := db.ExecContext(ctx, `CREATE SCHEMA "`+schema+`"`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := db.ExecContext(ctx, `DROP SCHEMA "`+schema+`" CASCADE`); err != nil {
			t.Errorf("drop test schema: %v", err)
		}
	}()

	store, err := NewPostgresGovernedStore(db, schema)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.ClaimBucketOwnership(ctx, "acme", "acme-files"); err != nil {
		t.Fatal(err)
	}

	publicObject := registerStoreObject(t, ctx, store, "acme", TenantStorageScope(), "public.parquet")
	workspaceObject := registerStoreObject(t, ctx, store, "acme", WorkspaceStorageScope(workspaceA), "a.parquet")
	otherWorkspaceObject := registerStoreObject(t, ctx, store, "acme", WorkspaceStorageScope(workspaceB), "b.parquet")
	otherTenantObject := registerStoreObject(t, ctx, store, "atlas", TenantStorageScope(), "other-tenant.parquet")
	reservedObject, err := store.ReserveObject(ctx, RegisterActiveObjectInput{
		Tenant: "acme", Scope: WorkspaceStorageScope(workspaceA),
		ArtifactClass: ArtifactTransformationSQL, ContentHash: testContentHash,
		ByteSize: 12, MediaType: "text/sql", Bucket: "acme-files",
		Key: governedReservationKey(WorkspaceStorageScope(workspaceA), ArtifactTransformationSQL,
			"44444444-4444-4444-8444-444444444444"),
		CleanupOwnerID: "22222222-2222-4222-8222-222222222222",
	}, nil)
	if err != nil || reservedObject.State != ObjectStateReserved {
		t.Fatalf("reserve governed object = %#v, %v", reservedObject, err)
	}
	if _, err := store.AuthorizeObjectReservation(ctx, "atlas", reservedObject.ID); !errors.Is(err, ErrGovernedDenied) {
		t.Fatalf("other-tenant reservation authorization error = %v", err)
	}
	finalKey := governedObjectKey(reservedObject.Scope, reservedObject.ArtifactClass,
		reservedObject.ContentHash, reservedObject.ID)
	activatedObject, err := store.ActivateObject(ctx, "acme", reservedObject.ID,
		reservedObject.Bucket, finalKey)
	if err != nil || activatedObject.State != ObjectStateActive {
		t.Fatalf("activate governed object = %#v, %v", activatedObject, err)
	}
	if activatedObject.Key != finalKey {
		t.Fatalf("activated object key = %q, want %q", activatedObject.Key, finalKey)
	}

	grantInput := IssueGrantInput{
		Tenant:                  "acme",
		Scope:                   WorkspaceStorageScope(workspaceA),
		ExecutionID:             "33333333-3333-4333-8333-333333333333",
		ReadableObjectIDs:       []string{publicObject.ID, workspaceObject.ID},
		WritableArtifactClasses: []ArtifactClass{ArtifactSandboxRelation},
		IssuerServiceAccount:    "transformation-storage-authority",
		ExpiresAt:               time.Now().Add(time.Hour).Truncate(time.Millisecond), // client precision; Postgres keeps µs
		CleanupOwnerID:          "22222222-2222-4222-8222-222222222222",
	}
	grant, err := store.IssueGrant(ctx, grantInput)
	if err != nil {
		t.Fatal(err)
	}
	replayedGrant, err := store.IssueGrant(ctx, grantInput)
	if err != nil {
		t.Fatalf("idempotent grant replay: %v", err)
	}
	if replayedGrant.ID != grant.ID {
		t.Fatalf("replayed grant ID = %q, want %q", replayedGrant.ID, grant.ID)
	}
	if replayedGrant.SchemaVersion != 1 || !replayedGrant.ExpiresAt.Equal(grantInput.ExpiresAt.UTC()) {
		t.Fatalf("replayed grant contract = %#v", replayedGrant)
	}
	ownerConflict := grantInput
	ownerConflict.CleanupOwnerID = "33333333-3333-4333-8333-333333333333"
	if _, err := store.IssueGrant(ctx, ownerConflict); !errors.Is(err, ErrGovernedConflict) {
		t.Fatalf("changed cleanup owner replay error = %v", err)
	}
	conflictingInput := grantInput
	conflictingInput.ReadableObjectIDs = []string{workspaceObject.ID}
	if _, err := store.IssueGrant(ctx, conflictingInput); !errors.Is(err, ErrGovernedConflict) {
		t.Fatalf("conflicting execution identity error = %v", err)
	}
	for _, test := range []struct {
		name        string
		scope       StorageScope
		object      GovernedObject
		executionID string
	}{
		{"workspace cannot include another workspace", WorkspaceStorageScope(workspaceA), otherWorkspaceObject,
			"66666666-6666-4666-8666-666666666666"},
		{"workspace cannot include another tenant", WorkspaceStorageScope(workspaceA), otherTenantObject,
			"77777777-7777-4777-8777-777777777777"},
		{"tenant-public cannot include workspace", TenantStorageScope(), workspaceObject,
			"88888888-8888-4888-8888-888888888888"},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := grantInput
			input.Scope = test.scope
			input.ReadableObjectIDs = []string{test.object.ID}
			input.ExecutionID = test.executionID
			if _, err := store.IssueGrant(ctx, input); !errors.Is(err, ErrGovernedDenied) {
				t.Fatalf("IssueGrant() error = %v", err)
			}
		})
	}
	tenantGrantInput := grantInput
	tenantGrantInput.Scope = TenantStorageScope()
	tenantGrantInput.ReadableObjectIDs = []string{publicObject.ID}
	tenantGrantInput.ExecutionID = "99999999-9999-4999-8999-999999999999"
	if _, err := store.IssueGrant(ctx, tenantGrantInput); err != nil {
		t.Fatalf("issue tenant-public grant: %v", err)
	}

	if _, err := store.ConsumeGrantObject(ctx, ConsumeGrantObjectInput{
		Tenant: "acme", GrantID: grant.ID, ExecutionID: grant.ExecutionID,
		ObjectID: workspaceObject.ID, Now: time.Now(),
	}); err != nil {
		t.Fatalf("consume same-workspace object: %v", err)
	}
	if _, err := store.ConsumeGrantObject(ctx, ConsumeGrantObjectInput{
		Tenant: "acme", GrantID: grant.ID, ExecutionID: grant.ExecutionID,
		ObjectID: otherWorkspaceObject.ID, Now: time.Now(),
	}); !errors.Is(err, ErrGovernedDenied) {
		t.Fatalf("consume undeclared other-workspace object error = %v", err)
	}
	if _, err := store.ConsumeGrantObject(ctx, ConsumeGrantObjectInput{
		Tenant: "atlas", GrantID: grant.ID, ExecutionID: grant.ExecutionID,
		ObjectID: workspaceObject.ID, Now: time.Now(),
	}); !errors.Is(err, ErrGovernedDenied) {
		t.Fatalf("consume other-tenant grant error = %v", err)
	}
	if _, err := store.ConsumeGrantObject(ctx, ConsumeGrantObjectInput{
		Tenant: "acme", GrantID: grant.ID, ExecutionID: grant.ExecutionID,
		ObjectID: publicObject.ID, Now: grantInput.ExpiresAt.Add(time.Second),
	}); !errors.Is(err, ErrGovernedDenied) {
		t.Fatalf("consume expired grant error = %v", err)
	}

	output, err := store.ReserveGrantOutput(ctx, ReserveGrantOutputInput{
		Tenant: "acme", GrantID: grant.ID, ExecutionID: grant.ExecutionID,
		ArtifactClass: ArtifactSandboxRelation, ContentHash: testContentHash,
		ByteSize: 12, MediaType: "application/vnd.apache.parquet",
		Bucket: "acme-files", Now: time.Now(),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if output.Scope != WorkspaceStorageScope(workspaceA) {
		t.Fatalf("output scope = %#v, want workspace A", output.Scope)
	}
	if output.State != ObjectStateReserved {
		t.Fatalf("reserved output state = %q, want reserved", output.State)
	}
	reserved, err := store.AuthorizeGrantOutputReservation(ctx, GrantOutputReservationInput{
		Tenant: "acme", GrantID: grant.ID, ExecutionID: grant.ExecutionID,
		ObjectID: output.ID, Now: time.Now(),
	})
	if err != nil || reserved.ID != output.ID {
		t.Fatalf("authorize output reservation = %#v, %v", reserved, err)
	}
	finalOutputKey := governedObjectKey(output.Scope, output.ArtifactClass, output.ContentHash, output.ID)
	activated, err := store.ActivateGrantOutput(ctx, GrantOutputReservationInput{
		Tenant: "acme", GrantID: grant.ID, ExecutionID: grant.ExecutionID,
		ObjectID: output.ID, Now: time.Now(),
	}, output.Bucket, finalOutputKey)
	if err != nil || activated.State != ObjectStateActive {
		t.Fatalf("activate output = %#v, %v", activated, err)
	}
	if _, err := store.ReserveGrantOutput(ctx, ReserveGrantOutputInput{
		Tenant: "acme", GrantID: grant.ID, ExecutionID: grant.ExecutionID,
		ArtifactClass: ArtifactSandboxDiagnostic, ContentHash: testContentHash,
		ByteSize: 12, MediaType: "application/json", Bucket: "acme-files",
		Now: time.Now(),
	}, nil); !errors.Is(err, ErrGovernedDenied) {
		t.Fatalf("undeclared output class error = %v", err)
	}

	if err := store.RevokeGrant(ctx, "acme", grant.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ConsumeGrantObject(ctx, ConsumeGrantObjectInput{
		Tenant: "acme", GrantID: grant.ID, ExecutionID: grant.ExecutionID,
		ObjectID: publicObject.ID, Now: time.Now(),
	}); !errors.Is(err, ErrGovernedDenied) {
		t.Fatalf("consume revoked grant error = %v", err)
	}

	unlockLakePath, err := store.AcquireLakePathLock(ctx, "acme", "acme-files",
		"ducklake/main/public/ducklake-public.parquet")
	if err != nil {
		t.Fatalf("acquire lake path lock: %v", err)
	}
	unlockLakePath()

	lakePublic, err := store.ReserveLakeObject(ctx, RegisterActiveObjectInput{
		Tenant: "acme", Scope: TenantStorageScope(), ArtifactClass: ArtifactDuckLakeData,
		ContentHash: testContentHash, ByteSize: 12, MediaType: "application/vnd.apache.parquet",
		Bucket: "acme-files", Key: "ducklake/main/public/ducklake-public.parquet",
	})
	if err != nil || lakePublic.State != ObjectStateReserved {
		t.Fatalf("reserve lake object = %#v, %v", lakePublic, err)
	}
	if _, err := store.ReadLakeObject(ctx, "acme", TenantStorageScope(), lakePublic.Bucket, lakePublic.Key); !errors.Is(err, ErrGovernedDenied) {
		t.Fatalf("reserved lake read error = %v", err)
	}
	lakePublic, err = store.ActivateLakeObject(ctx, "acme", lakePublic.ID)
	if err != nil || lakePublic.State != ObjectStateActive || lakePublic.Key != "ducklake/main/public/ducklake-public.parquet" {
		t.Fatalf("activate exact-path lake object = %#v, %v", lakePublic, err)
	}
	if replay, err := store.ReserveLakeObject(ctx, RegisterActiveObjectInput{
		Tenant: "acme", Scope: TenantStorageScope(), ArtifactClass: ArtifactDuckLakeData,
		ContentHash: testContentHash, ByteSize: 12, MediaType: "application/vnd.apache.parquet",
		Bucket: lakePublic.Bucket, Key: lakePublic.Key,
	}); err != nil || replay.ID != lakePublic.ID || replay.State != ObjectStateActive {
		t.Fatalf("active lake retry = %#v, %v", replay, err)
	}
	if _, err := store.ReserveLakeObject(ctx, RegisterActiveObjectInput{
		Tenant: "acme", Scope: WorkspaceStorageScope(workspaceA), ArtifactClass: ArtifactDuckLakeData,
		ContentHash: testContentHash, ByteSize: 12, MediaType: "application/vnd.apache.parquet",
		Bucket: lakePublic.Bucket, Key: lakePublic.Key,
	}); !errors.Is(err, ErrGovernedConflict) {
		t.Fatalf("cross-scope lake write conflict = %v", err)
	}

	lakePrivate, err := store.ReserveLakeObject(ctx, RegisterActiveObjectInput{
		Tenant: "acme", Scope: WorkspaceStorageScope(workspaceA), ArtifactClass: ArtifactDuckLakeData,
		ContentHash: testContentHash, ByteSize: 12, MediaType: "application/vnd.apache.parquet",
		Bucket: "acme-files", Key: "ducklake/main/private/ducklake-private.parquet",
	})
	if err != nil {
		t.Fatal(err)
	}
	lakePrivate, err = store.ActivateLakeObject(ctx, "acme", lakePrivate.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, read := range []struct {
		name   string
		scope  StorageScope
		object GovernedObject
		allow  bool
	}{
		{"tenant public", TenantStorageScope(), lakePublic, true},
		{"workspace public overlay", WorkspaceStorageScope(workspaceA), lakePublic, true},
		{"same workspace private", WorkspaceStorageScope(workspaceA), lakePrivate, true},
		{"tenant private denied", TenantStorageScope(), lakePrivate, false},
		{"cross workspace denied", WorkspaceStorageScope(workspaceB), lakePrivate, false},
	} {
		t.Run("lake read "+read.name, func(t *testing.T) {
			_, err := store.ReadLakeObject(ctx, "acme", read.scope, read.object.Bucket, read.object.Key)
			if read.allow && err != nil {
				t.Fatalf("ReadLakeObject() error = %v", err)
			}
			if !read.allow && !errors.Is(err, ErrGovernedDenied) {
				t.Fatalf("ReadLakeObject() error = %v", err)
			}
		})
	}
	if _, err := store.DeleteLakeObject(ctx, "acme", WorkspaceStorageScope(workspaceA),
		lakePublic.Bucket, lakePublic.Key, time.Now()); !errors.Is(err, ErrGovernedDenied) {
		t.Fatalf("workspace delete of public lake object error = %v", err)
	}
	deleted, err := store.DeleteLakeObject(ctx, "acme", WorkspaceStorageScope(workspaceA),
		lakePrivate.Bucket, lakePrivate.Key, time.Now())
	if err != nil || deleted.State != ObjectStateDeleted {
		t.Fatalf("delete lake object = %#v, %v", deleted, err)
	}
	if retry, err := store.DeleteLakeObject(ctx, "acme", WorkspaceStorageScope(workspaceA),
		lakePrivate.Bucket, lakePrivate.Key, time.Now()); err != nil || retry.State != ObjectStateDeleted {
		t.Fatalf("retry lake delete = %#v, %v", retry, err)
	}
	if _, err := store.ReadLakeObject(ctx, "acme", WorkspaceStorageScope(workspaceA),
		lakePrivate.Bucket, lakePrivate.Key); !errors.Is(err, ErrGovernedDenied) {
		t.Fatalf("deleted lake read error = %v", err)
	}

	createLakeObject := func(scope StorageScope, key string) GovernedObject {
		t.Helper()
		object, reserveErr := store.ReserveLakeObject(ctx, RegisterActiveObjectInput{
			Tenant: "acme", Scope: scope, ArtifactClass: ArtifactDuckLakeData,
			ContentHash: testContentHash, ByteSize: 12, MediaType: "application/vnd.apache.parquet",
			Bucket: "acme-files", Key: key,
		})
		if reserveErr != nil {
			t.Fatalf("reserve batch lake object %q: %v", key, reserveErr)
		}
		object, activateErr := store.ActivateLakeObject(ctx, "acme", object.ID)
		if activateErr != nil {
			t.Fatalf("activate batch lake object %q: %v", key, activateErr)
		}
		return object
	}
	publicBatchObject := createLakeObject(TenantStorageScope(), "ducklake/main/public/batch-public.parquet")
	workspaceBatchA := createLakeObject(WorkspaceStorageScope(workspaceA), "ducklake/main/private/batch-a.parquet")
	workspaceBatchB := createLakeObject(WorkspaceStorageScope(workspaceB), "ducklake/main/private/batch-b.parquet")

	if _, err := store.DeleteLakeObjects(ctx, "acme", TenantStorageScope(), "acme-files",
		[]string{lakePublic.Key, workspaceBatchA.Key}, time.Now()); !errors.Is(err, ErrGovernedDenied) {
		t.Fatalf("mixed tenant/workspace batch delete error = %v", err)
	}
	if _, err := store.ReadLakeObject(ctx, "acme", TenantStorageScope(), lakePublic.Bucket,
		lakePublic.Key); err != nil {
		t.Fatalf("rejected mixed batch changed public row: %v", err)
	}
	if _, err := store.DeleteLakeObjects(ctx, "acme", WorkspaceStorageScope(workspaceA), "acme-files",
		[]string{workspaceBatchA.Key, workspaceBatchB.Key}, time.Now()); !errors.Is(err, ErrGovernedDenied) {
		t.Fatalf("mixed workspace batch delete error = %v", err)
	}
	if _, err := store.ReadLakeObject(ctx, "acme", WorkspaceStorageScope(workspaceA), workspaceBatchA.Bucket,
		workspaceBatchA.Key); err != nil {
		t.Fatalf("rejected cross-workspace batch changed workspace A row: %v", err)
	}
	deletedBatch, err := store.DeleteLakeObjects(ctx, "acme", TenantStorageScope(), "acme-files",
		[]string{lakePublic.Key, publicBatchObject.Key}, time.Now())
	if err != nil || len(deletedBatch) != 2 || deletedBatch[0].State != ObjectStateDeleted ||
		deletedBatch[1].State != ObjectStateDeleted {
		t.Fatalf("tenant-public batch delete = %#v, %v", deletedBatch, err)
	}
	if retryBatch, err := store.DeleteLakeObjects(ctx, "acme", TenantStorageScope(), "acme-files",
		[]string{lakePublic.Key, publicBatchObject.Key}, time.Now()); err != nil || len(retryBatch) != 2 {
		t.Fatalf("tenant-public batch retry = %#v, %v", retryBatch, err)
	}
}

func TestPostgresGovernedStoreReplaysReadOnlyGrantWithEmptyOutputClasses(t *testing.T) {
	databaseURL := os.Getenv("STORAGE_BROKER_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("STORAGE_BROKER_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	schema := fmt.Sprintf("storage_broker_grant_json_test_%d", time.Now().UnixNano())
	if _, err := db.ExecContext(ctx, `CREATE SCHEMA "`+schema+`"`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := db.ExecContext(ctx, `DROP SCHEMA "`+schema+`" CASCADE`); err != nil {
			t.Errorf("drop test schema: %v", err)
		}
	}()
	store, err := NewPostgresGovernedStore(db, schema)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.ClaimBucketOwnership(ctx, "acme", "acme-files"); err != nil {
		t.Fatal(err)
	}
	object := registerStoreObject(t, ctx, store, "acme", WorkspaceStorageScope(workspaceA), "read-only.parquet")

	clientExpiry := time.Now().Add(time.Hour).Truncate(time.Second).UTC().
		Add(120 * time.Millisecond).Format("2006-01-02T15:04:05.000Z07:00")
	expiresAt, err := time.Parse(time.RFC3339Nano, clientExpiry)
	if err != nil {
		t.Fatal(err)
	}
	grantInput := IssueGrantInput{
		Tenant: "acme", Scope: WorkspaceStorageScope(workspaceA),
		ExecutionID:       "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
		ReadableObjectIDs: []string{object.ID}, IssuerServiceAccount: "transformation-storage-authority",
		ExpiresAt: expiresAt, CleanupOwnerID: "22222222-2222-4222-8222-222222222222",
	}
	issued, err := store.IssueGrant(ctx, grantInput)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := store.IssueGrant(ctx, grantInput)
	if err != nil {
		t.Fatalf("idempotent grant replay: %v", err)
	}
	if replayed.ID != issued.ID {
		t.Fatalf("replayed grant ID = %q, want %q", replayed.ID, issued.ID)
	}
	for name, grant := range map[string]ExecutionGrant{"issued": issued, "replayed": replayed} {
		encoded, err := json.Marshal(grant)
		if err != nil {
			t.Fatal(err)
		}
		body := string(encoded)
		for _, want := range []string{
			`"writableArtifactClasses":[]`,
			`"readableObjectIds":["` + object.ID + `"]`,
			`"expiresAt":"` + clientExpiry + `"`,
		} {
			if !strings.Contains(body, want) {
				t.Fatalf("%s grant JSON %s does not contain %s", name, body, want)
			}
		}
	}
}

func TestPostgresGovernedStoreDeletesExactOrdinaryObjectScope(t *testing.T) {
	databaseURL := os.Getenv("STORAGE_BROKER_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("STORAGE_BROKER_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	schema := fmt.Sprintf("storage_broker_delete_test_%d", time.Now().UnixNano())
	if _, err := db.ExecContext(ctx, `CREATE SCHEMA "`+schema+`"`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := db.ExecContext(ctx, `DROP SCHEMA "`+schema+`" CASCADE`); err != nil {
			t.Errorf("drop test schema: %v", err)
		}
	}()
	store, err := NewPostgresGovernedStore(db, schema)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.ClaimBucketOwnership(ctx, "acme", "acme-files"); err != nil {
		t.Fatal(err)
	}

	active := registerStoreObject(t, ctx, store, "acme", WorkspaceStorageScope(workspaceA), "delete-active")
	public := registerStoreObject(t, ctx, store, "acme", TenantStorageScope(), "delete-public")
	reserved, err := store.ReserveObject(ctx, RegisterActiveObjectInput{
		Tenant: "acme", Scope: WorkspaceStorageScope(workspaceA),
		ArtifactClass: ArtifactTransformationSQL, ContentHash: testContentHash,
		ByteSize: 12, MediaType: "text/sql", Bucket: "acme-files",
		Key: governedReservationKey(WorkspaceStorageScope(workspaceA), ArtifactTransformationSQL,
			"44444444-4444-4444-8444-444444444444"),
		CleanupOwnerID: "22222222-2222-4222-8222-222222222222",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE "`+schema+`".storage_objects SET staging_key = NULL WHERE object_id = $1`,
		reserved.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE "`+schema+`".storage_objects SET staging_key = NULL WHERE object_id = $1`,
		active.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("backfill legacy reserved staging key: %v", err)
	}
	if referenced, err := store.IsLiveObjectKeyReferenced(ctx, "acme", reserved.Bucket, reserved.Key); err != nil || !referenced {
		t.Fatalf("reserved staging key reference = %t, %v", referenced, err)
	}
	workspaceStagingPrefix := governedStagingPrefix(WorkspaceStorageScope(workspaceA), ArtifactTransformationSQL)
	if referenced, err := store.HasLiveObjectKeyUnderPrefix(ctx, "acme", reserved.Bucket,
		workspaceStagingPrefix); err != nil || !referenced {
		t.Fatalf("live staging prefix reference = %t, %v", referenced, err)
	}

	grantInput := IssueGrantInput{
		Tenant: "acme", Scope: WorkspaceStorageScope(workspaceA),
		ExecutionID:       "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
		ReadableObjectIDs: []string{active.ID}, IssuerServiceAccount: "transformation-storage-authority",
		ExpiresAt:      time.Now().Add(time.Hour),
		CleanupOwnerID: "22222222-2222-4222-8222-222222222222",
	}
	grant, err := store.IssueGrant(ctx, grantInput)
	if err != nil {
		t.Fatal(err)
	}

	for _, denied := range []struct {
		name     string
		tenant   string
		scope    StorageScope
		objectID string
	}{
		{"other tenant", "atlas", WorkspaceStorageScope(workspaceA), active.ID},
		{"other workspace", "acme", WorkspaceStorageScope(workspaceB), active.ID},
		{"workspace cannot delete public", "acme", WorkspaceStorageScope(workspaceA), public.ID},
		{"tenant cannot delete workspace", "acme", TenantStorageScope(), active.ID},
	} {
		t.Run(denied.name, func(t *testing.T) {
			if _, err := store.TombstoneObject(ctx, denied.tenant, denied.scope, denied.objectID,
				time.Now()); !errors.Is(err, ErrGovernedDenied) {
				t.Fatalf("TombstoneObject() error = %v", err)
			}
		})
	}

	firstRevocation := time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)
	deleted, err := store.TombstoneObject(ctx, "acme", WorkspaceStorageScope(workspaceA), active.ID,
		firstRevocation)
	if err != nil || deleted.Object.State != ObjectStateDeleted || !deleted.AccessRevokedAt.Equal(firstRevocation) {
		t.Fatalf("active deletion = %#v, %v", deleted, err)
	}
	if retry, err := store.TombstoneObject(ctx, "acme", WorkspaceStorageScope(workspaceA), active.ID,
		firstRevocation.Add(time.Hour)); err != nil || !retry.AccessRevokedAt.Equal(firstRevocation) {
		t.Fatalf("idempotent deletion = %#v, %v", retry, err)
	}
	if _, err := store.ActivateObject(ctx, "acme", active.ID, active.Bucket, active.Key); !errors.Is(err, ErrGovernedDenied) {
		t.Fatalf("reactivate deleted object error = %v", err)
	}
	if _, err := store.AuthorizeObjectReservation(ctx, "acme", active.ID); !errors.Is(err, ErrGovernedDenied) {
		t.Fatalf("authorize deleted object error = %v", err)
	}
	sweptAt := firstRevocation.Add(10 * time.Minute)
	swept, err := store.RecordLegacyStagingSweep(ctx, "acme", WorkspaceStorageScope(workspaceA),
		active.ID, sweptAt)
	if err != nil || swept.LegacyStagingSweptAt == nil || !swept.LegacyStagingSweptAt.Equal(sweptAt) {
		t.Fatalf("record legacy staging sweep = %#v, %v", swept, err)
	}
	if retry, err := store.RecordLegacyStagingSweep(ctx, "acme", WorkspaceStorageScope(workspaceA),
		active.ID, sweptAt.Add(time.Hour)); err != nil || retry.LegacyStagingSweptAt == nil ||
		!retry.LegacyStagingSweptAt.Equal(sweptAt) {
		t.Fatalf("idempotent legacy staging sweep = %#v, %v", retry, err)
	}
	if _, err := store.ConsumeGrantObject(ctx, ConsumeGrantObjectInput{
		Tenant: "acme", GrantID: grant.ID, ExecutionID: grant.ExecutionID,
		ObjectID: active.ID, Now: time.Now(),
	}); !errors.Is(err, ErrGovernedDenied) {
		t.Fatalf("consume tombstoned object error = %v", err)
	}
	grantInput.ExecutionID = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	if _, err := store.IssueGrant(ctx, grantInput); !errors.Is(err, ErrGovernedDenied) {
		t.Fatalf("issue grant for tombstoned object error = %v", err)
	}

	reservedDeletion, err := store.TombstoneObject(ctx, "acme", WorkspaceStorageScope(workspaceA),
		reserved.ID, firstRevocation)
	if err != nil || reservedDeletion.StagingKey != reserved.Key {
		t.Fatalf("reserved deletion = %#v, %v", reservedDeletion, err)
	}
	if referenced, err := store.IsLiveObjectKeyReferenced(ctx, "acme", reserved.Bucket, reserved.Key); err != nil || referenced {
		t.Fatalf("deleted staging key reference = %t, %v", referenced, err)
	}
	if referenced, err := store.HasLiveObjectKeyUnderPrefix(ctx, "acme", reserved.Bucket,
		workspaceStagingPrefix); err != nil || referenced {
		t.Fatalf("deleted staging prefix reference = %t, %v", referenced, err)
	}
	if referenced, err := store.IsLiveObjectKeyReferenced(ctx, "atlas", public.Bucket, public.Key); err != nil || referenced {
		t.Fatalf("cross-tenant live-key reference = %t, %v", referenced, err)
	}
	if _, err := store.ActivateObject(ctx, "acme", reserved.ID, reserved.Bucket,
		governedObjectKey(reserved.Scope, reserved.ArtifactClass, reserved.ContentHash, reserved.ID)); !errors.Is(err, ErrGovernedDenied) {
		t.Fatalf("activate tombstoned reservation error = %v", err)
	}

	confirmedAt := firstRevocation.Add(time.Hour)
	if _, err := store.ConfirmObjectRemoval(ctx, "acme", WorkspaceStorageScope(workspaceA),
		active.ID, confirmedAt); !errors.Is(err, ErrGovernedDenied) {
		t.Fatalf("legacy object with unknown staging locator was confirmed: %v", err)
	}
	confirmed, err := store.ConfirmObjectRemoval(ctx, "acme", WorkspaceStorageScope(workspaceA),
		reserved.ID, confirmedAt)
	if err != nil || confirmed.PhysicalRemovalConfirmedAt == nil ||
		!confirmed.PhysicalRemovalConfirmedAt.Equal(confirmedAt) {
		t.Fatalf("confirm deletion = %#v, %v", confirmed, err)
	}
	if retry, err := store.ConfirmObjectRemoval(ctx, "acme", WorkspaceStorageScope(workspaceA),
		reserved.ID, confirmedAt.Add(time.Hour)); err != nil || retry.PhysicalRemovalConfirmedAt == nil ||
		!retry.PhysicalRemovalConfirmedAt.Equal(confirmedAt) {
		t.Fatalf("idempotent confirmation = %#v, %v", retry, err)
	}

	lake, err := store.ReserveLakeObject(ctx, RegisterActiveObjectInput{
		Tenant: "acme", Scope: WorkspaceStorageScope(workspaceA), ArtifactClass: ArtifactDuckLakeData,
		ContentHash: testContentHash, ByteSize: 12, MediaType: "application/vnd.apache.parquet",
		Bucket: "acme-files", Key: "ducklake/delete-contract.parquet",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.TombstoneObject(ctx, "acme", WorkspaceStorageScope(workspaceA), lake.ID,
		firstRevocation); !errors.Is(err, ErrGovernedDenied) {
		t.Fatalf("ordinary delete accepted DuckLake object: %v", err)
	}

	deployedLockKey := "acme\nacme-files\nducklake/delete-contract.parquet"
	for name, lockKey := range map[string]string{
		"deployed":     deployedLockKey,
		"intermediate": "lake\n" + deployedLockKey,
	} {
		t.Run(name+" Lake lock compatibility", func(t *testing.T) {
			legacyLockConn, connErr := db.Conn(ctx)
			if connErr != nil {
				t.Fatal(connErr)
			}
			if _, lockErr := legacyLockConn.ExecContext(ctx,
				`SELECT pg_advisory_lock(hashtextextended($1, 0))`, lockKey); lockErr != nil {
				_ = legacyLockConn.Close()
				t.Fatal(lockErr)
			}
			lakeLockAcquired := make(chan func(), 1)
			lakeLockErr := make(chan error, 1)
			go func() {
				release, lockError := store.AcquireLakePathLock(ctx, "acme", "acme-files",
					"ducklake/delete-contract.parquet")
				if lockError != nil {
					lakeLockErr <- lockError
					return
				}
				lakeLockAcquired <- release
			}()
			select {
			case release := <-lakeLockAcquired:
				release()
				t.Fatalf("new broker did not honor the %s Lake lock key", name)
			case lockErr := <-lakeLockErr:
				t.Fatalf("acquire compatible Lake lock: %v", lockErr)
			case <-time.After(100 * time.Millisecond):
			}
			var unlocked bool
			if unlockErr := legacyLockConn.QueryRowContext(ctx,
				`SELECT pg_advisory_unlock(hashtextextended($1, 0))`, lockKey).Scan(&unlocked); unlockErr != nil || !unlocked {
				_ = legacyLockConn.Close()
				t.Fatalf("release %s Lake lock = %t, %v", name, unlocked, unlockErr)
			}
			_ = legacyLockConn.Close()
			select {
			case release := <-lakeLockAcquired:
				release()
			case lockErr := <-lakeLockErr:
				t.Fatalf("compatible Lake lock failed after release: %v", lockErr)
			case <-time.After(2 * time.Second):
				t.Fatal("compatible Lake lock was not released")
			}
		})
	}

	// The deployed broker takes "object\n..." and then this internal key. A new
	// replica takes only the internal key, so it must still wait for an older
	// replica that holds both.
	deployedObjectLockConn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, deployedObjectLockKey := range []string{"object\nacme\n" + public.ID,
		objectLockKey("acme", public.ID)} {
		if _, err := deployedObjectLockConn.ExecContext(ctx,
			`SELECT pg_advisory_lock(hashtextextended($1, 0))`, deployedObjectLockKey); err != nil {
			_ = deployedObjectLockConn.Close()
			t.Fatal(err)
		}
	}
	objectLockAcquired := make(chan func(), 1)
	objectLockErr := make(chan error, 1)
	go func() {
		release, lockError := store.AcquireObjectLock(ctx, "acme", public.ID)
		if lockError != nil {
			objectLockErr <- lockError
			return
		}
		objectLockAcquired <- release
	}()
	select {
	case release := <-objectLockAcquired:
		release()
		t.Fatal("new broker did not honor the deployed object lock key")
	case lockErr := <-objectLockErr:
		t.Fatalf("acquire compatible object lock: %v", lockErr)
	case <-time.After(100 * time.Millisecond):
	}
	if _, err := deployedObjectLockConn.ExecContext(ctx, `SELECT pg_advisory_unlock_all()`); err != nil {
		_ = deployedObjectLockConn.Close()
		t.Fatalf("release deployed object locks: %v", err)
	}
	_ = deployedObjectLockConn.Close()
	select {
	case release := <-objectLockAcquired:
		release()
	case lockErr := <-objectLockErr:
		t.Fatalf("compatible object lock failed after release: %v", lockErr)
	case <-time.After(2 * time.Second):
		t.Fatal("compatible object lock was not released")
	}

	physicalBarrierCollision, err := store.AcquirePhysicalKeyLock(ctx, "registration", "acme", "acme-files")
	if err != nil {
		t.Fatal(err)
	}
	barrierCtx, cancelBarrier := context.WithTimeout(ctx, time.Second)
	nonCollidingBarrier, err := store.AcquireRegistrationBarrier(barrierCtx, "acme", "acme-files")
	cancelBarrier()
	physicalBarrierCollision()
	if err != nil {
		t.Fatalf("registration barrier collided with another tenant's physical key: %v", err)
	}
	nonCollidingBarrier()

	// Physical paths must not collide with the internal object-lock key.
	physicalObjectCollision, err := store.AcquirePhysicalKeyLock(ctx, "object-other", "acme", public.ID)
	if err != nil {
		t.Fatal(err)
	}
	objectCtx, cancelObject := context.WithTimeout(ctx, time.Second)
	nonCollidingObject, err := store.AcquireObjectLock(objectCtx, "acme", public.ID)
	cancelObject()
	physicalObjectCollision()
	if err != nil {
		t.Fatalf("object lock collided with an unrelated physical key: %v", err)
	}
	nonCollidingObject()

	registrationBarrier, err := store.AcquireRegistrationBarrier(ctx, "acme", "acme-files")
	if err != nil {
		t.Fatal(err)
	}
	registrationKey := "governed-staging/workspace/" + workspaceA + "/transformation_sql/concurrent"
	if release, err := store.AcquireRegisterExistingLock(ctx, "acme", "acme-files",
		registrationKey); !errors.Is(err, ErrGovernedConflict) {
		if release != nil {
			release()
		}
		t.Fatalf("register-existing during cleanup error = %v, want conflict", err)
	}
	if release, err := store.AcquireRegisterExistingLock(ctx, "atlas", "acme-files",
		registrationKey+"-other-tenant"); !errors.Is(err, ErrGovernedConflict) {
		if release != nil {
			release()
		}
		t.Fatalf("cross-tenant register-existing during bucket cleanup error = %v, want conflict", err)
	}
	lakeRegistrationAcquired := make(chan func(), 1)
	lakeRegistrationErr := make(chan error, 1)
	go func() {
		release, lockError := store.AcquireLakeRegistrationLock(ctx, "acme", "acme-files",
			"ducklake/concurrent-registration.parquet")
		if lockError != nil {
			lakeRegistrationErr <- lockError
			return
		}
		lakeRegistrationAcquired <- release
	}()
	select {
	case release := <-lakeRegistrationAcquired:
		release()
		registrationBarrier()
		t.Fatal("Lake PUT registration did not wait for cleanup barrier")
	case err := <-lakeRegistrationErr:
		registrationBarrier()
		t.Fatalf("Lake PUT registration lock failed while cleanup barrier was active: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	registrationBarrier()
	select {
	case release := <-lakeRegistrationAcquired:
		release()
	case err := <-lakeRegistrationErr:
		t.Fatalf("Lake PUT registration lock failed after cleanup barrier release: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("Lake PUT registration lock was not released after cleanup barrier")
	}
	oldAtlasLakeLock, err := store.AcquireLakePathLock(ctx, "atlas", "acme-files",
		"ducklake/lock-order.parquet")
	if err != nil {
		t.Fatal(err)
	}
	atlasLakeRegistrationAcquired := make(chan func(), 1)
	atlasLakeRegistrationErr := make(chan error, 1)
	go func() {
		release, lockError := store.AcquireLakeRegistrationLock(ctx, "atlas", "acme-files",
			"ducklake/lock-order.parquet")
		if lockError != nil {
			atlasLakeRegistrationErr <- lockError
			return
		}
		atlasLakeRegistrationAcquired <- release
	}()
	// The new Lake registration must acquire the (shared) bucket barrier before
	// waiting on the deployed path lock, regardless of how the tenant name sorts.
	time.Sleep(100 * time.Millisecond)
	if cleanupCouldStart := tryExclusiveAdvisoryLock(t, ctx, db,
		registrationBarrierKey("acme-files")); cleanupCouldStart {
		oldAtlasLakeLock()
		t.Fatal("registration/path lock order is inconsistent: barrier not held while waiting on path")
	}
	oldAtlasLakeLock()
	select {
	case release := <-atlasLakeRegistrationAcquired:
		release()
	case err := <-atlasLakeRegistrationErr:
		t.Fatalf("ordered Lake registration lock failed after path release: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("ordered Lake registration lock did not finish after path release")
	}
	releaseRegistration, err := store.AcquireRegisterExistingLock(ctx, "acme", "acme-files",
		registrationKey)
	if err != nil {
		t.Fatalf("register-existing lock failed after barrier release: %v", err)
	}
	// Registrations share the bucket barrier: the immutable owner row, not
	// mutual exclusion, decides ownership, so same-bucket registrations of
	// different keys proceed concurrently. Only cleanup excludes them.
	releaseSameBucketRegistration, err := store.AcquireRegisterExistingLock(ctx, "acme", "acme-files",
		registrationKey+"-other")
	if err != nil {
		releaseRegistration()
		t.Fatalf("same-bucket concurrent registration lock failed: %v", err)
	}
	sameKeyCtx, cancelSameKey := context.WithTimeout(ctx, 150*time.Millisecond)
	release, err := store.AcquireRegisterExistingLock(sameKeyCtx, "acme", "acme-files", registrationKey)
	cancelSameKey()
	if !errors.Is(err, context.DeadlineExceeded) {
		if release != nil {
			release()
		}
		releaseSameBucketRegistration()
		releaseRegistration()
		t.Fatalf("same-key concurrent registration did not wait on the physical key: %v", err)
	}
	releaseOtherBucketRegistration, err := store.AcquireRegisterExistingLock(ctx, "acme", "other-files",
		registrationKey+"-other-bucket")
	if err != nil {
		releaseSameBucketRegistration()
		releaseRegistration()
		t.Fatalf("different-bucket concurrent registration lock failed: %v", err)
	}
	releaseOtherRegistration := func() {
		releaseOtherBucketRegistration()
		releaseSameBucketRegistration()
	}
	cleanupBarrierAcquired := make(chan func(), 1)
	cleanupBarrierErr := make(chan error, 1)
	go func() {
		release, lockError := store.AcquireRegistrationBarrier(ctx, "acme", "acme-files")
		if lockError != nil {
			cleanupBarrierErr <- lockError
			return
		}
		cleanupBarrierAcquired <- release
	}()
	select {
	case release := <-cleanupBarrierAcquired:
		release()
		releaseOtherRegistration()
		releaseRegistration()
		t.Fatal("cleanup barrier did not wait for registration lock")
	case err := <-cleanupBarrierErr:
		releaseOtherRegistration()
		releaseRegistration()
		t.Fatalf("cleanup barrier failed while registrations were active: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	releaseOtherRegistration()
	releaseRegistration()
	select {
	case release := <-cleanupBarrierAcquired:
		release()
	case err := <-cleanupBarrierErr:
		t.Fatalf("cleanup barrier failed after registrations completed: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("cleanup barrier was not released after registrations completed")
	}

	// A real reservation by another tenant in this bucket holds the shared barrier
	// for its transaction; bucket cleanup must wait for it to finish. It rolls
	// back so the bucket's persisted history stays exclusive for later checks.
	releaseReservation, reservationDone := holdReservationThenRollBack(t, ctx, store, "atlas", "acme-files")
	reservationCleanupAcquired := make(chan func(), 1)
	go func() {
		release, lockError := store.AcquireRegistrationBarrier(ctx, "acme", "acme-files")
		if lockError != nil {
			cleanupBarrierErr <- lockError
			return
		}
		reservationCleanupAcquired <- release
	}()
	select {
	case release := <-reservationCleanupAcquired:
		release()
		releaseReservation()
		t.Fatal("cleanup barrier did not wait for reservation transaction")
	case err := <-cleanupBarrierErr:
		releaseReservation()
		t.Fatalf("cleanup barrier failed while reservation was active: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	releaseReservation()
	if err := <-reservationDone; err != nil {
		t.Fatalf("in-flight reservation failed: %v", err)
	}
	select {
	case release := <-reservationCleanupAcquired:
		release()
	case err := <-cleanupBarrierErr:
		t.Fatalf("cleanup barrier failed after reservation completed: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("cleanup barrier was not released after reservation completed")
	}

	unlock, err := store.AcquireObjectLock(ctx, "acme", public.ID)
	if err != nil {
		t.Fatal(err)
	}
	acquired := make(chan func(), 1)
	lockErr := make(chan error, 1)
	go func() {
		release, lockError := store.AcquireObjectLock(ctx, "acme", public.ID)
		if lockError != nil {
			lockErr <- lockError
			return
		}
		acquired <- release
	}()
	select {
	case release := <-acquired:
		release()
		unlock()
		t.Fatal("concurrent object mutation lock did not block")
	case err := <-lockErr:
		unlock()
		t.Fatalf("concurrent object mutation lock failed: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	unlock()
	select {
	case release := <-acquired:
		release()
	case err := <-lockErr:
		t.Fatalf("concurrent object mutation lock failed after release: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("concurrent object mutation lock was not released")
	}

	limitedDB, err := sql.Open("pgx", databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer limitedDB.Close()
	limitedDB.SetMaxOpenConns(4)
	limitedStore, err := NewPostgresGovernedStore(limitedDB, schema)
	if err != nil {
		t.Fatal(err)
	}
	releases := make([]func(), 0, 3)
	defer func() {
		for _, release := range releases {
			release()
		}
	}()
	for _, objectID := range []string{active.ID, public.ID, reserved.ID} {
		release, lockError := limitedStore.AcquireObjectLock(ctx, "acme", objectID)
		if lockError != nil {
			t.Fatalf("acquire distinct object lock: %v", lockError)
		}
		releases = append(releases, release)
	}
	pingCtx, cancelPing := context.WithTimeout(ctx, time.Second)
	defer cancelPing()
	if err := limitedDB.PingContext(pingCtx); err != nil {
		t.Fatalf("mutation locks exhausted database pool: %v", err)
	}
	fourthAcquired := make(chan func(), 1)
	go func() {
		release, lockError := limitedStore.AcquireObjectLock(ctx, "acme", lake.ID)
		if lockError != nil {
			lockErr <- lockError
			return
		}
		fourthAcquired <- release
	}()
	select {
	case release := <-fourthAcquired:
		release()
		t.Fatal("mutation lock concurrency limit was not enforced")
	case err := <-lockErr:
		t.Fatalf("fourth distinct mutation lock failed: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	releases[0]()
	releases = releases[1:]
	select {
	case release := <-fourthAcquired:
		release()
	case err := <-lockErr:
		t.Fatalf("fourth distinct mutation lock failed after release: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("mutation lock concurrency slot was not released")
	}
	manyBuckets := make([]string, 0, defaultConcurrentMutationLockLimit+4)
	for index := 0; index < defaultConcurrentMutationLockLimit+4; index++ {
		manyBuckets = append(manyBuckets, fmt.Sprintf("historical-bucket-%02d", index))
	}
	manyBarrierCtx, cancelManyBarriers := context.WithTimeout(ctx, time.Second)
	manyBarriersRelease, err := limitedStore.AcquireRegistrationBarriers(manyBarrierCtx, "acme", manyBuckets)
	cancelManyBarriers()
	if err != nil {
		t.Fatalf("acquire many registration barriers in one mutation slot: %v", err)
	}
	manyBarriersRelease()

	if exclusive, err := store.IsBucketExclusiveToTenant(ctx, "acme", "acme-files"); err != nil || !exclusive {
		t.Fatalf("tenant-only persisted bucket history = %t, %v", exclusive, err)
	}
	var ownerVersionBefore string
	if err := db.QueryRowContext(ctx, `SELECT xmin::text FROM "`+schema+`".storage_bucket_owners
		WHERE physical_bucket = 'acme-files'`).Scan(&ownerVersionBefore); err != nil {
		t.Fatal(err)
	}
	if err := store.ClaimBucketOwnership(ctx, "acme", "acme-files"); err != nil {
		t.Fatalf("idempotent bucket ownership claim: %v", err)
	}
	var ownerVersionAfter string
	if err := db.QueryRowContext(ctx, `SELECT xmin::text FROM "`+schema+`".storage_bucket_owners
		WHERE physical_bucket = 'acme-files'`).Scan(&ownerVersionAfter); err != nil {
		t.Fatal(err)
	}
	if ownerVersionAfter != ownerVersionBefore {
		t.Fatalf("idempotent bucket ownership claim rewrote the owner row: %s -> %s",
			ownerVersionBefore, ownerVersionAfter)
	}
	if err := store.ClaimBucketOwnership(ctx, "atlas", "acme-files"); !errors.Is(err, ErrGovernedConflict) {
		t.Fatalf("conflicting bucket ownership claim = %v, want conflict", err)
	}
	historical, err := store.RegisterActiveObject(ctx, RegisterActiveObjectInput{
		Tenant: "atlas", Scope: TenantStorageScope(), ArtifactClass: ArtifactWorkBundle,
		ContentHash: testContentHash, ByteSize: 12, MediaType: "application/vnd.apache.parquet",
		Bucket: "acme-files", Key: "governed/test/historical-other-tenant",
	})
	if err != nil {
		t.Fatalf("seed historically shared bucket: %v", err)
	}
	var registeredStaging sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT staging_key FROM "`+schema+`".storage_objects WHERE object_id = $1`,
		historical.ID).Scan(&registeredStaging); err != nil || !registeredStaging.Valid ||
		registeredStaging.String != historical.Key {
		t.Fatalf("register-existing staging locator = %#v, %v", registeredStaging, err)
	}
	if exclusive, err := store.IsBucketExclusiveToTenant(ctx, "acme", "acme-files"); err != nil || exclusive {
		t.Fatalf("historically shared persisted bucket history = %t, %v", exclusive, err)
	}
}

func TestPostgresGovernedStoreCleansExactReservationScope(t *testing.T) {
	databaseURL := os.Getenv("STORAGE_BROKER_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("STORAGE_BROKER_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	schema := fmt.Sprintf("storage_broker_reservation_cleanup_test_%d", time.Now().UnixNano())
	if _, err := db.ExecContext(ctx, `CREATE SCHEMA "`+schema+`"`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := db.ExecContext(ctx, `DROP SCHEMA "`+schema+`" CASCADE`); err != nil {
			t.Errorf("drop test schema: %v", err)
		}
	}()
	store, err := NewPostgresGovernedStore(db, schema)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.ClaimBucketOwnership(ctx, "acme", "acme-files"); err != nil {
		t.Fatal(err)
	}

	reserve := func(scope StorageScope, artifactClass ArtifactClass, suffix string) GovernedObject {
		t.Helper()
		object, err := store.ReserveObject(ctx, RegisterActiveObjectInput{
			Tenant: "acme", Scope: scope, ArtifactClass: artifactClass,
			ContentHash: testContentHash, ByteSize: 12, MediaType: "application/json",
			Bucket: "acme-files", Key: governedReservationKey(scope, artifactClass, suffix),
			CleanupOwnerID: "22222222-2222-4222-8222-222222222222",
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return object
	}
	workspaceAReservation := reserve(WorkspaceStorageScope(workspaceA), ArtifactAnalysisManifest,
		"44444444-4444-4444-8444-444444444444")
	workspaceAReservation2 := reserve(WorkspaceStorageScope(workspaceA), ArtifactAnalysisManifest,
		"44444444-4444-4444-8444-444444444445")
	workspaceBReservation := reserve(WorkspaceStorageScope(workspaceB), ArtifactAnalysisResult,
		"55555555-5555-4555-8555-555555555555")
	activeAnalysis := reserve(WorkspaceStorageScope(workspaceA), ArtifactAnalysisResult,
		"66666666-6666-4666-8666-666666666666")
	activeAnalysis, err = store.ActivateObject(ctx, "acme", activeAnalysis.ID,
		activeAnalysis.Bucket, governedObjectKey(activeAnalysis.Scope, activeAnalysis.ArtifactClass,
			activeAnalysis.ContentHash, activeAnalysis.ID))
	if err != nil {
		t.Fatalf("commit analysis result: %v", err)
	}
	analysisGrant, err := store.IssueGrant(ctx, IssueGrantInput{
		Tenant: "acme", Scope: WorkspaceStorageScope(workspaceA),
		ExecutionID:          "77777777-7777-4777-8777-777777777777",
		ReadableObjectIDs:    []string{activeAnalysis.ID},
		IssuerServiceAccount: "transformation-storage-authority",
		ExpiresAt:            time.Now().Add(time.Hour),
		CleanupOwnerID:       "22222222-2222-4222-8222-222222222222",
	})
	if err != nil {
		t.Fatalf("grant analysis result read: %v", err)
	}
	readAnalysis, err := store.ConsumeGrantObject(ctx, ConsumeGrantObjectInput{
		Tenant: "acme", GrantID: analysisGrant.ID, ExecutionID: analysisGrant.ExecutionID,
		ObjectID: activeAnalysis.ID, Now: time.Now(),
	})
	if err != nil || readAnalysis.ID != activeAnalysis.ID {
		t.Fatalf("read analysis result = %#v, %v", readAnalysis, err)
	}
	active := registerStoreObject(t, ctx, store, "acme", WorkspaceStorageScope(workspaceA),
		"reservation-cleanup-active")
	revokedAt := time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)

	if err := store.TombstoneReservations(ctx, "acme", WorkspaceStorageScope(workspaceB), revokedAt); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AuthorizeObjectReservation(ctx, "acme", workspaceAReservation.ID); err != nil {
		t.Fatalf("other-workspace cleanup changed workspace A reservation: %v", err)
	}
	workspaceBPending, err := store.PendingReservationDeletions(ctx, "acme",
		WorkspaceStorageScope(workspaceB), 64)
	if err != nil || len(workspaceBPending) != 1 || workspaceBPending[0].Object.ID != workspaceBReservation.ID {
		t.Fatalf("workspace B pending cleanup = %#v, %v", workspaceBPending, err)
	}

	if err := store.TombstoneReservations(ctx, "acme", WorkspaceStorageScope(workspaceA), revokedAt); err != nil {
		t.Fatal(err)
	}
	pending, err := store.PendingReservationDeletions(ctx, "acme", WorkspaceStorageScope(workspaceA), 64)
	if err != nil || len(pending) != 2 || !pending[0].AccessRevokedAt.Equal(revokedAt) {
		t.Fatalf("workspace A pending cleanup = %#v, %v", pending, err)
	}
	firstPending := pending[0].Object.ID
	if err := store.RecordReservationCleanupAttempt(ctx, "acme", WorkspaceStorageScope(workspaceA),
		firstPending, revokedAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	rotated, err := store.PendingReservationDeletions(ctx, "acme", WorkspaceStorageScope(workspaceA), 1)
	if err != nil || len(rotated) != 1 || rotated[0].Object.ID == firstPending {
		t.Fatalf("reservation cleanup did not rotate past attempted row: %#v, %v", rotated, err)
	}
	if _, err := store.ActivateObject(ctx, "acme", workspaceAReservation.ID,
		workspaceAReservation.Bucket, governedObjectKey(workspaceAReservation.Scope,
			workspaceAReservation.ArtifactClass, workspaceAReservation.ContentHash,
			workspaceAReservation.ID)); !errors.Is(err, ErrGovernedDenied) {
		t.Fatalf("late commit after reservation cleanup = %v, want denied", err)
	}
	if _, err := store.TombstoneObject(ctx, "acme", WorkspaceStorageScope(workspaceA), active.ID,
		revokedAt); err != nil {
		t.Fatal(err)
	}
	if _, err := store.TombstoneObject(ctx, "acme", WorkspaceStorageScope(workspaceA),
		activeAnalysis.ID, revokedAt); err != nil {
		t.Fatalf("delete analysis result: %v", err)
	}
	if _, err := store.ConsumeGrantObject(ctx, ConsumeGrantObjectInput{
		Tenant: "acme", GrantID: analysisGrant.ID, ExecutionID: analysisGrant.ExecutionID,
		ObjectID: activeAnalysis.ID, Now: time.Now(),
	}); !errors.Is(err, ErrGovernedDenied) {
		t.Fatalf("read deleted analysis result = %v, want denied", err)
	}
	pending, err = store.PendingReservationDeletions(ctx, "acme", WorkspaceStorageScope(workspaceA), 64)
	if err != nil || len(pending) != 2 {
		t.Fatalf("active deletion leaked into reservation cleanup = %#v, %v", pending, err)
	}
	confirmedAt := revokedAt.Add(10 * time.Minute)
	for _, reservation := range []GovernedObject{workspaceAReservation, workspaceAReservation2} {
		if _, err := store.ConfirmObjectRemoval(ctx, "acme", WorkspaceStorageScope(workspaceA),
			reservation.ID, confirmedAt); err != nil {
			t.Fatal(err)
		}
	}
	count, oldest, err := store.ReservationCleanupStatus(ctx, "acme",
		WorkspaceStorageScope(workspaceA))
	if err != nil || count != 0 || oldest != nil {
		t.Fatalf("completed cleanup status = %d, %v, %v", count, oldest, err)
	}
	if err := store.TombstoneReservations(ctx, "acme", WorkspaceStorageScope(workspaceA),
		revokedAt.Add(time.Hour)); err != nil {
		t.Fatalf("idempotent cleanup retry: %v", err)
	}
	if object, err := store.AuthorizeObjectReservation(ctx, "acme", workspaceBReservation.ID); !errors.Is(err, ErrGovernedDenied) || object.ID != "" {
		t.Fatalf("workspace B tombstone was not durable: %#v, %v", object, err)
	}
}

func TestPostgresGovernedStoreCleanupOwnerInheritanceAndGrantRevocation(t *testing.T) {
	databaseURL := os.Getenv("STORAGE_BROKER_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("STORAGE_BROKER_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	schema := fmt.Sprintf("storage_broker_test_%d", time.Now().UnixNano())
	if _, err := db.ExecContext(ctx, `CREATE SCHEMA "`+schema+`"`); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = db.ExecContext(ctx, `DROP SCHEMA "`+schema+`" CASCADE`) }()
	store, err := NewPostgresGovernedStore(db, schema)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.ClaimBucketOwnership(ctx, "acme", "acme-files"); err != nil {
		t.Fatal(err)
	}
	ownerA := "22222222-2222-4222-8222-222222222222"
	ownerB := "33333333-3333-4333-8333-333333333333"
	reserve := func(owner, suffix string) GovernedObject {
		object, err := store.ReserveObject(ctx, RegisterActiveObjectInput{
			Tenant: "acme", Scope: WorkspaceStorageScope(workspaceA),
			ArtifactClass: ArtifactAnalysisResult, ContentHash: testContentHash,
			ByteSize: 12, MediaType: "application/json", Bucket: "acme-files",
			Key:            governedReservationKey(WorkspaceStorageScope(workspaceA), ArtifactAnalysisResult, suffix),
			CleanupOwnerID: owner,
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return object
	}
	ownedActive := reserve(ownerA, "44444444-4444-4444-8444-444444444444")
	if _, err := store.ActivateObject(ctx, "acme", ownedActive.ID, ownedActive.Bucket,
		governedObjectKey(ownedActive.Scope, ownedActive.ArtifactClass, ownedActive.ContentHash,
			ownedActive.ID)); err != nil {
		t.Fatal(err)
	}
	otherOwner := reserve(ownerB, "55555555-5555-4555-8555-555555555555")
	legacy := registerStoreObject(t, ctx, store, "acme", WorkspaceStorageScope(workspaceA), "legacy-owner-null")
	reserveLake := func(scope StorageScope, key string) GovernedObject {
		object, err := store.ReserveLakeObject(ctx, RegisterActiveObjectInput{
			Tenant: "acme", Scope: scope, ArtifactClass: ArtifactDuckLakeData,
			ContentHash: testContentHash, ByteSize: 12, MediaType: "application/parquet",
			Bucket: "acme-files", Key: key,
		})
		if err != nil {
			t.Fatal(err)
		}
		return object
	}
	lakeReserved := reserveLake(WorkspaceStorageScope(workspaceA), "ducklake/owner/reserved.parquet")
	lakeActive := reserveLake(WorkspaceStorageScope(workspaceA), "ducklake/owner/active.parquet")
	if _, err := store.ActivateLakeObject(ctx, "acme", lakeActive.ID); err != nil {
		t.Fatal(err)
	}
	lakeOtherWorkspace := reserveLake(WorkspaceStorageScope(workspaceB), "ducklake/owner/other.parquet")
	if _, err := store.ActivateLakeObject(ctx, "acme", lakeOtherWorkspace.ID); err != nil {
		t.Fatal(err)
	}
	pageOne, more, err := store.InventoryLakeObjects(ctx, "acme", WorkspaceStorageScope(workspaceA), "", 1)
	if err != nil || len(pageOne) != 1 || !more {
		t.Fatalf("lake inventory first page = %#v, %t, %v", pageOne, more, err)
	}
	pageTwo, more, err := store.InventoryLakeObjects(ctx, "acme", WorkspaceStorageScope(workspaceA),
		pageOne[0].ID, 1)
	if err != nil || len(pageTwo) != 1 || more {
		t.Fatalf("lake inventory second page = %#v, %t, %v", pageTwo, more, err)
	}
	grant, err := store.IssueGrant(ctx, IssueGrantInput{
		Tenant: "acme", Scope: WorkspaceStorageScope(workspaceA),
		ExecutionID:       "66666666-6666-4666-8666-666666666666",
		ReadableObjectIDs: []string{legacy.ID}, WritableArtifactClasses: []ArtifactClass{ArtifactSandboxRelation},
		IssuerServiceAccount: "transformation-storage-authority", ExpiresAt: time.Now().Add(time.Hour),
		CleanupOwnerID: ownerA,
	})
	if err != nil {
		t.Fatal(err)
	}
	output, err := store.ReserveGrantOutput(ctx, ReserveGrantOutputInput{
		Tenant: "acme", GrantID: grant.ID, ExecutionID: grant.ExecutionID,
		ArtifactClass: ArtifactSandboxRelation, ContentHash: testContentHash, ByteSize: 12,
		MediaType: "application/parquet", Bucket: "acme-files", Now: time.Now(),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var outputOwner string
	if err := db.QueryRowContext(ctx, `SELECT cleanup_owner_id::text FROM "`+schema+
		`".storage_objects WHERE object_id = $1`, output.ID).Scan(&outputOwner); err != nil || outputOwner != ownerA {
		t.Fatalf("output cleanup owner = %q, %v", outputOwner, err)
	}
	revokedAt := time.Now().UTC()
	if err := store.TombstoneOrdinaryObjects(ctx, "acme", WorkspaceStorageScope(workspaceA),
		ownerA, false, revokedAt); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReserveGrantOutput(ctx, ReserveGrantOutputInput{
		Tenant: "acme", GrantID: grant.ID, ExecutionID: grant.ExecutionID,
		ArtifactClass: ArtifactSandboxRelation, ContentHash: testContentHash, ByteSize: 12,
		MediaType: "application/parquet", Bucket: "acme-files", Now: time.Now(),
	}, nil); !errors.Is(err, ErrGovernedDenied) {
		t.Fatalf("revoked grant output reservation error = %v", err)
	}
	deletions, err := store.PendingOrdinaryObjectDeletions(ctx, "acme",
		WorkspaceStorageScope(workspaceA), ownerA, false, 10)
	if err != nil || len(deletions) != 2 {
		t.Fatalf("owner pending deletions = %d, %v", len(deletions), err)
	}
	if object, err := store.AuthorizeObjectReservation(ctx, "acme", otherOwner.ID); err != nil ||
		object.State != ObjectStateReserved {
		t.Fatalf("other owner changed = %#v, %v", object, err)
	}
	if err := store.TombstoneOrdinaryObjects(ctx, "acme", WorkspaceStorageScope(workspaceA),
		"", true, revokedAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	for _, objectID := range []string{otherOwner.ID, legacy.ID} {
		var state ObjectState
		if err := db.QueryRowContext(ctx, `SELECT state FROM "`+schema+`".storage_objects WHERE object_id = $1`,
			objectID).Scan(&state); err != nil || state != ObjectStateDeleted {
			t.Fatalf("workspace object %s state = %q, %v", objectID, state, err)
		}
	}
	for _, objectID := range []string{lakeReserved.ID, lakeActive.ID, lakeOtherWorkspace.ID} {
		var state ObjectState
		if err := db.QueryRowContext(ctx, `SELECT state FROM "`+schema+`".storage_objects WHERE object_id = $1`,
			objectID).Scan(&state); err != nil || state == ObjectStateDeleted {
			t.Fatalf("workspace ordinary cleanup changed lake object %s to %q, %v", objectID, state, err)
		}
	}
}

// Cleanup selectors are rendered as index-bounded equality per scope kind. The
// tenant form (workspace_id IS NULL) and the workspace form must each select
// exactly their own rows, and a retried cleanup must keep the first revocation.
func TestPostgresGovernedStoreCleanupSelectorsMatchExactScope(t *testing.T) {
	databaseURL := os.Getenv("STORAGE_BROKER_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("STORAGE_BROKER_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	schema := fmt.Sprintf("storage_broker_test_%d", time.Now().UnixNano())
	if _, err := db.ExecContext(ctx, `CREATE SCHEMA "`+schema+`"`); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = db.ExecContext(ctx, `DROP SCHEMA "`+schema+`" CASCADE`) }()
	store, err := NewPostgresGovernedStore(db, schema)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.ClaimBucketOwnership(ctx, "acme", "acme-files"); err != nil {
		t.Fatal(err)
	}
	owner := "22222222-2222-4222-8222-222222222222"
	tenantScope := TenantStorageScope()
	workspaceScope := WorkspaceStorageScope(workspaceA)
	reserve := func(scope StorageScope, suffix string) GovernedObject {
		object, err := store.ReserveObject(ctx, RegisterActiveObjectInput{
			Tenant: "acme", Scope: scope, ArtifactClass: ArtifactAnalysisResult,
			ContentHash: testContentHash, ByteSize: 12, MediaType: "application/json",
			Bucket: "acme-files", Key: governedReservationKey(scope, ArtifactAnalysisResult, suffix),
			CleanupOwnerID: owner,
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return object
	}
	stateOf := func(objectID string) ObjectState {
		var state ObjectState
		if err := db.QueryRowContext(ctx, `SELECT state FROM "`+schema+`".storage_objects
			WHERE object_id = $1`, objectID).Scan(&state); err != nil {
			t.Fatal(err)
		}
		return state
	}
	revokedAtOf := func(grantID string) sql.NullTime {
		var revokedAt sql.NullTime
		if err := db.QueryRowContext(ctx, `SELECT revoked_at FROM "`+schema+`".storage_execution_grants
			WHERE grant_id = $1`, grantID).Scan(&revokedAt); err != nil {
			t.Fatal(err)
		}
		return revokedAt
	}
	issue := func(scope StorageScope, executionID, readableID string) ExecutionGrant {
		grant, err := store.IssueGrant(ctx, IssueGrantInput{
			Tenant: "acme", Scope: scope, ExecutionID: executionID, ReadableObjectIDs: []string{readableID},
			WritableArtifactClasses: []ArtifactClass{ArtifactSandboxRelation},
			IssuerServiceAccount:    "transformation-storage-authority", ExpiresAt: time.Now().Add(time.Hour),
			CleanupOwnerID: owner,
		})
		if err != nil {
			t.Fatal(err)
		}
		return grant
	}

	tenantReserved := reserve(tenantScope, "44444444-4444-4444-8444-444444444444")
	if err := store.ClaimBucketOwnership(ctx, "beacon", "beacon-files"); err != nil {
		t.Fatal(err)
	}
	otherTenant, err := store.ReserveObject(ctx, RegisterActiveObjectInput{
		Tenant: "beacon", Scope: tenantScope, ArtifactClass: ArtifactAnalysisResult,
		ContentHash: testContentHash, ByteSize: 12, MediaType: "application/json",
		Bucket: "beacon-files", Key: governedReservationKey(tenantScope, ArtifactAnalysisResult,
			"88888888-8888-4888-8888-888888888888"),
		CleanupOwnerID: owner,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	workspaceReserved := reserve(workspaceScope, "55555555-5555-4555-8555-555555555555")
	tenantGrant := issue(tenantScope, "66666666-6666-4666-8666-666666666666",
		registerStoreObject(t, ctx, store, "acme", tenantScope, "tenant-readable").ID)
	workspaceGrant := issue(workspaceScope, "77777777-7777-4777-8777-777777777777",
		registerStoreObject(t, ctx, store, "acme", workspaceScope, "workspace-readable").ID)

	if buckets, err := store.OrdinaryCleanupBuckets(ctx, "acme", tenantScope, owner, false); err != nil ||
		!slices.Equal(buckets, []string{"acme-files"}) {
		t.Fatalf("tenant owner cleanup buckets = %v, %v", buckets, err)
	}
	firstRevocation := time.Now().UTC().Truncate(time.Microsecond)
	if err := store.TombstoneOrdinaryObjects(ctx, "acme", tenantScope, owner, false, firstRevocation); err != nil {
		t.Fatal(err)
	}
	if stateOf(tenantReserved.ID) != ObjectStateDeleted || stateOf(workspaceReserved.ID) != ObjectStateReserved ||
		stateOf(otherTenant.ID) != ObjectStateReserved {
		t.Fatalf("tenant owner cleanup states = %q, %q, other tenant %q", stateOf(tenantReserved.ID),
			stateOf(workspaceReserved.ID), stateOf(otherTenant.ID))
	}
	if revoked := revokedAtOf(tenantGrant.ID); !revoked.Valid || !revoked.Time.Equal(firstRevocation) {
		t.Fatalf("tenant grant revoked_at = %v", revoked)
	}
	if revoked := revokedAtOf(workspaceGrant.ID); revoked.Valid {
		t.Fatalf("tenant owner cleanup revoked a workspace grant at %v", revoked.Time)
	}
	if err := store.TombstoneOrdinaryObjects(ctx, "acme", tenantScope, owner, false,
		firstRevocation.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if revoked := revokedAtOf(tenantGrant.ID); !revoked.Time.Equal(firstRevocation) {
		t.Fatalf("retried cleanup moved tenant grant revocation to %v", revoked.Time)
	}
	pending, err := store.PendingOrdinaryObjectDeletions(ctx, "acme", tenantScope, owner, false, 10)
	if err != nil || len(pending) != 1 || pending[0].Object.ID != tenantReserved.ID {
		t.Fatalf("tenant owner pending deletions = %#v, %v", pending, err)
	}
	if count, _, err := store.OrdinaryCleanupStatus(ctx, "acme", tenantScope, owner, false); err != nil || count != 1 {
		t.Fatalf("tenant owner cleanup status = %d, %v", count, err)
	}
	if count, _, err := store.ReservationCleanupStatus(ctx, "acme", tenantScope); err != nil || count != 1 {
		t.Fatalf("tenant reservation cleanup status = %d, %v", count, err)
	}

	if err := store.TombstoneReservations(ctx, "acme", workspaceScope, firstRevocation); err != nil {
		t.Fatal(err)
	}
	pending, err = store.PendingReservationDeletions(ctx, "acme", workspaceScope, 10)
	if err != nil || len(pending) != 1 || pending[0].Object.ID != workspaceReserved.ID {
		t.Fatalf("workspace reservation pending deletions = %#v, %v", pending, err)
	}
	if count, _, err := store.OrdinaryCleanupStatus(ctx, "acme", workspaceScope, "", true); err != nil || count != 1 {
		t.Fatalf("workspace cleanup status = %d, %v", count, err)
	}
	if revoked := revokedAtOf(workspaceGrant.ID); revoked.Valid {
		t.Fatalf("reservation cleanup revoked a workspace grant at %v", revoked.Time)
	}
}

const testContentHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func registerStoreObject(
	t *testing.T,
	ctx context.Context,
	store *PostgresGovernedStore,
	tenant string,
	scope StorageScope,
	key string,
) GovernedObject {
	t.Helper()
	object, err := store.RegisterActiveObject(ctx, RegisterActiveObjectInput{
		Tenant: tenant, Scope: scope, ArtifactClass: ArtifactWorkBundle,
		ContentHash: testContentHash, ByteSize: 12, MediaType: "application/vnd.apache.parquet",
		Bucket: tenant + "-files", Key: "governed/test/" + key,
	})
	if err != nil {
		t.Fatal(err)
	}
	return object
}
