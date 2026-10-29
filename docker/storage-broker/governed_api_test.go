package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

type fixedReviewer struct {
	namespace string
	account   string
}

func (r fixedReviewer) Review(context.Context, string) (string, string, error) {
	return r.namespace, r.account, nil
}

type fakeGovernedStore struct {
	object                     GovernedObject
	grant                      ExecutionGrant
	deletion                   GovernedObjectDeletion
	issueCalls                 int
	issued                     IssueGrantInput
	consumeCalls               int
	reserveCalls               int
	tombstoneCalls             int
	confirmRemovalCalls        int
	recordSweepCalls           int
	acquireObjectLockCalls     int
	objectLockHeld             bool
	registrationBarrier        bool
	registrationLocks          []string
	physicalKeyLockCalls       []string
	physicalKeyLockHeld        bool
	registerSawKeyLock         bool
	registerSawBarrier         bool
	reserveErr                 error
	reservationOpen            bool
	ticketsIssuedInReservation int
	reserved                   RegisterActiveObjectInput
	tombstoneErr               error
	referencedKeys             map[string]bool
	livePrefixReferenced       bool
	nonExclusiveBucket         bool
	bucketOwnerConflict        bool
	bucketOwnerErr             error
	claimedBuckets             []string
	reservationDeletions       []GovernedObjectDeletion
	tombstoneReservations      int
	cleanupOwners              map[string]string
	lakeInventory              []GovernedObject
	confirmedLakeKeys          []string
}

func (s *fakeGovernedStore) RegisterActiveObject(context.Context, RegisterActiveObjectInput) (GovernedObject, error) {
	s.registerSawKeyLock = s.physicalKeyLockHeld
	s.registerSawBarrier = s.registrationBarrier
	return s.object, nil
}

func (s *fakeGovernedStore) ReserveObject(
	_ context.Context,
	input RegisterActiveObjectInput,
	issue ReservationIssuer,
) (GovernedObject, error) {
	s.reserveCalls++
	s.reserved = input
	if s.reserveErr != nil {
		return GovernedObject{}, s.reserveErr
	}
	return s.object, s.issueInsideReservation(issue)
}

// issueInsideReservation models the store contract: the ticket is signed after
// the row exists and before it commits, under the shared registration barrier.
func (s *fakeGovernedStore) issueInsideReservation(issue ReservationIssuer) error {
	if issue == nil {
		return nil
	}
	s.reservationOpen = true
	defer func() { s.reservationOpen = false }()
	s.ticketsIssuedInReservation++
	return issue(s.object)
}
func (s *fakeGovernedStore) AuthorizeObjectReservation(context.Context, string, string) (GovernedObject, error) {
	return s.object, nil
}
func (s *fakeGovernedStore) ActivateObject(context.Context, string, string, string, string) (GovernedObject, error) {
	return s.object, nil
}
func (s *fakeGovernedStore) IssueGrant(_ context.Context, input IssueGrantInput) (ExecutionGrant, error) {
	s.issueCalls++
	s.issued = input
	return s.grant, nil
}
func (s *fakeGovernedStore) ConsumeGrantObject(context.Context, ConsumeGrantObjectInput) (GovernedObject, error) {
	s.consumeCalls++
	return s.object, nil
}
func (s *fakeGovernedStore) ReserveGrantOutput(
	_ context.Context,
	_ ReserveGrantOutputInput,
	issue ReservationIssuer,
) (GovernedObject, error) {
	if s.reserveErr != nil {
		return GovernedObject{}, s.reserveErr
	}
	if err := s.issueInsideReservation(issue); err != nil {
		return GovernedObject{}, err
	}
	return s.object, nil
}
func (s *fakeGovernedStore) AuthorizeGrantOutputReservation(context.Context, GrantOutputReservationInput) (GovernedObject, error) {
	return s.object, nil
}
func (s *fakeGovernedStore) ActivateGrantOutput(context.Context, GrantOutputReservationInput, string, string) (GovernedObject, error) {
	return s.object, nil
}
func (s *fakeGovernedStore) RevokeGrant(context.Context, string, string, time.Time) error { return nil }
func (s *fakeGovernedStore) AcquireObjectLock(context.Context, string, string) (func(), error) {
	s.acquireObjectLockCalls++
	s.objectLockHeld = true
	return func() { s.objectLockHeld = false }, nil
}
func (s *fakeGovernedStore) AcquireRegistrationBarrier(_ context.Context, tenant, bucket string) (func(), error) {
	s.registrationLocks = append(s.registrationLocks, tenant+"/"+bucket)
	s.registrationBarrier = true
	return func() { s.registrationBarrier = false }, nil
}
func (s *fakeGovernedStore) AcquireRegistrationBarriers(
	_ context.Context,
	tenant string,
	buckets []string,
) (func(), error) {
	for _, bucket := range buckets {
		s.registrationLocks = append(s.registrationLocks, tenant+"/"+bucket)
	}
	s.registrationBarrier = true
	return func() { s.registrationBarrier = false }, nil
}
func (s *fakeGovernedStore) AcquireRegisterExistingLock(_ context.Context, tenant, bucket, key string) (func(), error) {
	s.registrationLocks = append(s.registrationLocks, tenant+"/"+bucket)
	s.physicalKeyLockCalls = append(s.physicalKeyLockCalls, tenant+"/"+bucket+"/"+key)
	s.registrationBarrier = true
	s.physicalKeyLockHeld = true
	return func() {
		s.physicalKeyLockHeld = false
		s.registrationBarrier = false
	}, nil
}
func (s *fakeGovernedStore) AcquireLakeRegistrationLock(context.Context, string, string, string) (func(), error) {
	s.registrationBarrier = true
	return func() { s.registrationBarrier = false }, nil
}
func (s *fakeGovernedStore) ClaimBucketOwnership(_ context.Context, tenant, bucket string) error {
	s.claimedBuckets = append(s.claimedBuckets, tenant+"/"+bucket)
	if s.bucketOwnerErr != nil {
		return s.bucketOwnerErr
	}
	if s.bucketOwnerConflict {
		return ErrGovernedConflict
	}
	return nil
}
func (s *fakeGovernedStore) VerifyBucketRegistration(ctx context.Context, tenant, bucket string) error {
	if err := s.ClaimBucketOwnership(ctx, tenant, bucket); err != nil {
		return err
	}
	if s.nonExclusiveBucket {
		return ErrGovernedConflict
	}
	return nil
}
func (s *fakeGovernedStore) ReservationCleanupBuckets(
	_ context.Context,
	tenant string,
	scope StorageScope,
) ([]string, error) {
	buckets := make([]string, 0)
	for _, deletion := range s.reservationDeletions {
		if deletion.Object.Tenant == tenant && deletion.Object.Scope == scope &&
			!slices.Contains(buckets, deletion.Object.Bucket) {
			buckets = append(buckets, deletion.Object.Bucket)
		}
	}
	return buckets, nil
}
func (s *fakeGovernedStore) TombstoneReservations(
	_ context.Context,
	tenant string,
	scope StorageScope,
	now time.Time,
) error {
	s.tombstoneReservations++
	for index := range s.reservationDeletions {
		deletion := &s.reservationDeletions[index]
		if deletion.Object.Tenant != tenant || deletion.Object.Scope != scope ||
			deletion.Object.State != ObjectStateReserved {
			continue
		}
		deletion.Object.State = ObjectStateDeleted
		deletion.AccessRevokedAt = now
	}
	return nil
}
func (s *fakeGovernedStore) PendingReservationDeletions(
	_ context.Context,
	tenant string,
	scope StorageScope,
	limit int,
) ([]GovernedObjectDeletion, error) {
	deletions := make([]GovernedObjectDeletion, 0, limit)
	for _, attempted := range []bool{false, true} {
		for _, deletion := range s.reservationDeletions {
			if (deletion.ReservationCleanupAttemptedAt != nil) != attempted {
				continue
			}
			if deletion.Object.Tenant == tenant && deletion.Object.Scope == scope &&
				deletion.Object.State == ObjectStateDeleted && deletion.PhysicalRemovalConfirmedAt == nil {
				deletions = append(deletions, deletion)
				if len(deletions) == limit {
					return deletions, nil
				}
			}
		}
	}
	return deletions, nil
}
func (s *fakeGovernedStore) RecordReservationCleanupAttempt(
	_ context.Context,
	tenant string,
	scope StorageScope,
	objectID string,
	now time.Time,
) error {
	for index := range s.reservationDeletions {
		deletion := &s.reservationDeletions[index]
		if deletion.Object.Tenant == tenant && deletion.Object.Scope == scope &&
			deletion.Object.ID == objectID && deletion.PhysicalRemovalConfirmedAt == nil {
			deletion.ReservationCleanupAttemptedAt = &now
			return nil
		}
	}
	return ErrGovernedDenied
}
func (s *fakeGovernedStore) ReservationCleanupStatus(
	_ context.Context,
	tenant string,
	scope StorageScope,
) (int, *time.Time, error) {
	pending := 0
	var oldest *time.Time
	for _, deletion := range s.reservationDeletions {
		if deletion.Object.Tenant != tenant || deletion.Object.Scope != scope ||
			deletion.Object.State != ObjectStateDeleted || deletion.PhysicalRemovalConfirmedAt != nil {
			continue
		}
		pending++
		if oldest == nil || deletion.AccessRevokedAt.Before(*oldest) {
			value := deletion.AccessRevokedAt
			oldest = &value
		}
	}
	return pending, oldest, nil
}
func (s *fakeGovernedStore) matchesOrdinaryCleanup(
	deletion GovernedObjectDeletion, tenant string, scope StorageScope, ownerID string, allOwners bool,
) bool {
	return deletion.Object.Tenant == tenant && deletion.Object.Scope == scope &&
		deletion.Object.ArtifactClass != ArtifactDuckLakeData &&
		(allOwners || s.cleanupOwners[deletion.Object.ID] == ownerID)
}
func (s *fakeGovernedStore) OrdinaryCleanupBuckets(
	_ context.Context, tenant string, scope StorageScope, ownerID string, allOwners bool,
) ([]string, error) {
	buckets := make([]string, 0)
	for _, deletion := range s.reservationDeletions {
		if s.matchesOrdinaryCleanup(deletion, tenant, scope, ownerID, allOwners) &&
			deletion.PhysicalRemovalConfirmedAt == nil && !slices.Contains(buckets, deletion.Object.Bucket) {
			buckets = append(buckets, deletion.Object.Bucket)
		}
	}
	return buckets, nil
}
func (s *fakeGovernedStore) TombstoneOrdinaryObjects(
	_ context.Context, tenant string, scope StorageScope, ownerID string, allOwners bool, now time.Time,
) error {
	for index := range s.reservationDeletions {
		deletion := &s.reservationDeletions[index]
		if s.matchesOrdinaryCleanup(*deletion, tenant, scope, ownerID, allOwners) &&
			(deletion.Object.State == ObjectStateReserved || deletion.Object.State == ObjectStateActive) {
			deletion.Object.State = ObjectStateDeleted
			deletion.AccessRevokedAt = now
		}
	}
	return nil
}
func (s *fakeGovernedStore) PendingOrdinaryObjectDeletions(
	_ context.Context, tenant string, scope StorageScope, ownerID string, allOwners bool, limit int,
) ([]GovernedObjectDeletion, error) {
	deletions := make([]GovernedObjectDeletion, 0, limit)
	for _, attempted := range []bool{false, true} {
		for _, deletion := range s.reservationDeletions {
			if (deletion.ReservationCleanupAttemptedAt != nil) != attempted ||
				!s.matchesOrdinaryCleanup(deletion, tenant, scope, ownerID, allOwners) {
				continue
			}
			if deletion.Object.State == ObjectStateDeleted && deletion.PhysicalRemovalConfirmedAt == nil {
				deletions = append(deletions, deletion)
				if len(deletions) == limit {
					return deletions, nil
				}
			}
		}
	}
	return deletions, nil
}
func (s *fakeGovernedStore) OrdinaryCleanupStatus(
	_ context.Context, tenant string, scope StorageScope, ownerID string, allOwners bool,
) (int, *time.Time, error) {
	pending := 0
	var oldest *time.Time
	for _, deletion := range s.reservationDeletions {
		if !s.matchesOrdinaryCleanup(deletion, tenant, scope, ownerID, allOwners) ||
			deletion.Object.State != ObjectStateDeleted || deletion.PhysicalRemovalConfirmedAt != nil {
			continue
		}
		pending++
		if oldest == nil || deletion.AccessRevokedAt.Before(*oldest) {
			value := deletion.AccessRevokedAt
			oldest = &value
		}
	}
	return pending, oldest, nil
}
func (s *fakeGovernedStore) RecordLegacyStagingSweep(
	_ context.Context,
	tenant string,
	scope StorageScope,
	objectID string,
	now time.Time,
) (GovernedObjectDeletion, error) {
	s.recordSweepCalls++
	if tenant != s.deletion.Object.Tenant || scope != s.deletion.Object.Scope || objectID != s.deletion.Object.ID {
		return GovernedObjectDeletion{}, ErrGovernedDenied
	}
	if s.deletion.LegacyStagingSweptAt == nil {
		s.deletion.LegacyStagingSweptAt = &now
	}
	return s.deletion, nil
}
func (s *fakeGovernedStore) TombstoneObject(
	_ context.Context,
	tenant string,
	scope StorageScope,
	objectID string,
	_ time.Time,
) (GovernedObjectDeletion, error) {
	s.tombstoneCalls++
	if s.tombstoneErr != nil {
		return GovernedObjectDeletion{}, s.tombstoneErr
	}
	if tenant != s.deletion.Object.Tenant || scope != s.deletion.Object.Scope || objectID != s.deletion.Object.ID {
		return GovernedObjectDeletion{}, ErrGovernedDenied
	}
	return s.deletion, nil
}
func (s *fakeGovernedStore) ConfirmObjectRemoval(
	_ context.Context,
	tenant string,
	scope StorageScope,
	objectID string,
	now time.Time,
) (GovernedObjectDeletion, error) {
	if !s.registrationBarrier {
		panic("physical-removal confirmation ran without the registration barrier")
	}
	s.confirmRemovalCalls++
	for index := range s.reservationDeletions {
		deletion := &s.reservationDeletions[index]
		if tenant == deletion.Object.Tenant && scope == deletion.Object.Scope && objectID == deletion.Object.ID {
			deletion.PhysicalRemovalConfirmedAt = &now
			return *deletion, nil
		}
	}
	if tenant != s.deletion.Object.Tenant || scope != s.deletion.Object.Scope || objectID != s.deletion.Object.ID {
		return GovernedObjectDeletion{}, ErrGovernedDenied
	}
	s.deletion.PhysicalRemovalConfirmedAt = &now
	return s.deletion, nil
}
func (s *fakeGovernedStore) IsBucketExclusiveToTenant(context.Context, string, string) (bool, error) {
	return !s.nonExclusiveBucket, nil
}
func (s *fakeGovernedStore) IsLiveObjectKeyReferenced(_ context.Context, tenant, bucket, key string) (bool, error) {
	return s.referencedKeys[tenant+"/"+bucket+"/"+key], nil
}
func (s *fakeGovernedStore) HasLiveObjectKeyUnderPrefix(context.Context, string, string, string) (bool, error) {
	return s.livePrefixReferenced, nil
}
func (s *fakeGovernedStore) AcquirePhysicalKeyLock(_ context.Context, tenant, bucket, key string) (func(), error) {
	if s.objectLockHeld {
		panic("physical-key lock acquired while object lock is held")
	}
	s.physicalKeyLockCalls = append(s.physicalKeyLockCalls, tenant+"/"+bucket+"/"+key)
	s.physicalKeyLockHeld = true
	return func() { s.physicalKeyLockHeld = false }, nil
}
func (s *fakeGovernedStore) AcquireLakePathLock(context.Context, string, string, string) (func(), error) {
	return func() {}, nil
}
func (s *fakeGovernedStore) AcquireLakePathLocks(context.Context, string, string, []string) (func(), error) {
	return func() {}, nil
}
func (s *fakeGovernedStore) ReserveLakeObject(context.Context, RegisterActiveObjectInput) (GovernedObject, error) {
	return s.object, nil
}
func (s *fakeGovernedStore) ActivateLakeObject(context.Context, string, string) (GovernedObject, error) {
	return s.object, nil
}
func (s *fakeGovernedStore) ReadLakeObject(context.Context, string, StorageScope, string, string) (GovernedObject, error) {
	return s.object, nil
}
func (s *fakeGovernedStore) DeleteLakeObject(context.Context, string, StorageScope, string, string, time.Time) (GovernedObject, error) {
	return s.object, nil
}
func (s *fakeGovernedStore) DeleteLakeObjects(context.Context, string, StorageScope, string, []string, time.Time) ([]GovernedObject, error) {
	return []GovernedObject{s.object}, nil
}
func (s *fakeGovernedStore) ConfirmLakeObjectRemoval(
	_ context.Context, _ string, _ StorageScope, _ string, key string, _ time.Time,
) error {
	s.confirmedLakeKeys = append(s.confirmedLakeKeys, key)
	return nil
}
func (s *fakeGovernedStore) InventoryLakeObjects(
	_ context.Context, tenant string, scope StorageScope, afterObjectID string, limit int,
) ([]GovernedObject, bool, error) {
	objects := make([]GovernedObject, 0, limit+1)
	for _, object := range s.lakeInventory {
		if object.Tenant == tenant && object.Scope == scope && object.ID > afterObjectID {
			objects = append(objects, object)
		}
	}
	hasMore := len(objects) > limit
	if hasMore {
		objects = objects[:limit]
	}
	return objects, hasMore, nil
}

type fakeObjectCleaner struct {
	deleted      []string
	failKey      string
	failKeys     map[string]bool
	listed       []string
	walks        int
	barrierStore *fakeGovernedStore
}

func (c *fakeObjectCleaner) DeleteAndConfirmAbsent(_ context.Context, bucket, key string) error {
	if c.barrierStore != nil && !c.barrierStore.registrationBarrier {
		panic("physical deletion ran without the registration barrier")
	}
	c.deleted = append(c.deleted, bucket+"/"+key)
	if key == c.failKey || c.failKeys[key] {
		return errors.New("storage unavailable")
	}
	return nil
}

func (c *fakeObjectCleaner) WalkKeys(_ context.Context, _, _ string, visit func(string) error) error {
	if c.barrierStore != nil && !c.barrierStore.registrationBarrier {
		panic("staging listing ran without the registration barrier")
	}
	c.walks++
	for _, key := range c.listed {
		if !slices.Contains(c.deleted, "nimmly-files/"+key) {
			if err := visit(key); err != nil {
				return err
			}
		}
	}
	return nil
}

func TestGovernedObjectDeleteWaitsForWriteTicketsBeforePhysicalRemoval(t *testing.T) {
	revokedAt := time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)
	store := deletionStore(revokedAt)
	cleaner := &fakeObjectCleaner{barrierStore: store}
	server := governedServerForTest("transformation-storage-authority", store)
	server.objectCleaner = cleaner
	server.now = func() time.Time { return revokedAt.Add(2 * time.Minute) }
	request := governedDeleteRequest(store.deletion.Object.ID, WorkspaceStorageScope(workspaceA))
	response := httptest.NewRecorder()

	server.handleDeleteGovernedObject(response, request)

	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if len(cleaner.deleted) != 0 || store.confirmRemovalCalls != 0 {
		t.Fatalf("premature physical cleanup = %#v, confirmations = %d", cleaner.deleted, store.confirmRemovalCalls)
	}
	if response.Header().Get("Retry-After") != "180" {
		t.Fatalf("Retry-After = %q, want 180", response.Header().Get("Retry-After"))
	}
	assertDeletionResponse(t, response.Body.Bytes(), "revoked", "pending")
}

func TestGovernedObjectDeleteConfirmsFinalAndStagingRemoval(t *testing.T) {
	revokedAt := time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)
	store := deletionStore(revokedAt)
	cleaner := &fakeObjectCleaner{barrierStore: store}
	server := governedServerForTest("transformation-storage-authority", store)
	server.objectCleaner = cleaner
	nowCalls := 0
	server.now = func() time.Time {
		nowCalls++
		return revokedAt.Add(time.Duration(5+nowCalls) * time.Minute)
	}
	response := httptest.NewRecorder()

	server.handleDeleteGovernedObject(response,
		governedDeleteRequest(store.deletion.Object.ID, WorkspaceStorageScope(workspaceA)))

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	want := []string{
		"nimmly-files/" + store.deletion.Object.Key,
		"nimmly-files/governed-staging/workspace/" + workspaceA + "/transformation_sql/staged",
	}
	if !slices.Equal(cleaner.deleted, want) {
		t.Fatalf("deleted locators = %#v, want %#v", cleaner.deleted, want)
	}
	if store.confirmRemovalCalls != 1 {
		t.Fatalf("confirm removal calls = %d, want 1", store.confirmRemovalCalls)
	}
	if store.registrationBarrier {
		t.Fatal("registration barrier remained held after confirmed deletion")
	}
	wantConfirmedAt := revokedAt.Add(7 * time.Minute)
	if store.deletion.PhysicalRemovalConfirmedAt == nil ||
		!store.deletion.PhysicalRemovalConfirmedAt.Equal(wantConfirmedAt) {
		t.Fatalf("physical removal confirmed at = %v, want %v",
			store.deletion.PhysicalRemovalConfirmedAt, wantConfirmedAt)
	}
	assertDeletionResponse(t, response.Body.Bytes(), "revoked", "confirmed")
}

func TestGovernedObjectDeleteIsRetryableAfterStorageFailure(t *testing.T) {
	revokedAt := time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)
	store := deletionStore(revokedAt)
	cleaner := &fakeObjectCleaner{failKey: store.deletion.StagingKey}
	server := governedServerForTest("transformation-storage-authority", store)
	server.objectCleaner = cleaner
	server.now = func() time.Time { return revokedAt.Add(6 * time.Minute) }
	first := httptest.NewRecorder()

	server.handleDeleteGovernedObject(first,
		governedDeleteRequest(store.deletion.Object.ID, WorkspaceStorageScope(workspaceA)))

	if first.Code != http.StatusServiceUnavailable {
		t.Fatalf("first status = %d, body = %s", first.Code, first.Body.String())
	}
	if store.confirmRemovalCalls != 0 {
		t.Fatalf("failed cleanup was confirmed")
	}
	assertDeletionResponse(t, first.Body.Bytes(), "revoked", "pending")

	cleaner.failKey = ""
	second := httptest.NewRecorder()
	server.handleDeleteGovernedObject(second,
		governedDeleteRequest(store.deletion.Object.ID, WorkspaceStorageScope(workspaceA)))
	if second.Code != http.StatusOK || store.confirmRemovalCalls != 1 {
		t.Fatalf("retry status = %d, confirmations = %d, body = %s",
			second.Code, store.confirmRemovalCalls, second.Body.String())
	}
}

func TestReservationCleanupRevokesLostReservationAndWaitsForUploadTicket(t *testing.T) {
	now := time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)
	store := deletionStore(now)
	reservation := store.deletion
	reservation.Object.State = ObjectStateReserved
	reservation.AccessRevokedAt = time.Time{}
	store.reservationDeletions = []GovernedObjectDeletion{reservation}
	cleaner := &fakeObjectCleaner{barrierStore: store}
	server := governedServerForTest("transformation-storage-authority", store)
	server.objectCleaner = cleaner
	server.now = func() time.Time { return now }
	response := httptest.NewRecorder()

	server.handleCleanupGovernedReservations(response,
		governedReservationCleanupRequest(WorkspaceStorageScope(workspaceA)))

	if response.Code != http.StatusAccepted || store.tombstoneReservations != 1 ||
		len(cleaner.deleted) != 0 || store.confirmRemovalCalls != 0 {
		t.Fatalf("status = %d, tombstones = %d, deleted = %#v, confirmations = %d, body = %s",
			response.Code, store.tombstoneReservations, cleaner.deleted, store.confirmRemovalCalls,
			response.Body.String())
	}
	if response.Header().Get("Retry-After") != "300" {
		t.Fatalf("Retry-After = %q, want 300", response.Header().Get("Retry-After"))
	}
	assertReservationCleanupResponse(t, response.Body.Bytes(), "revoked", "pending", 1)
}

func TestReservationCleanupRecoversFromStorageFailureAndIsIdempotent(t *testing.T) {
	revokedAt := time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)
	store := deletionStore(revokedAt)
	store.reservationDeletions = []GovernedObjectDeletion{store.deletion}
	cleaner := &fakeObjectCleaner{barrierStore: store, failKey: store.deletion.StagingKey}
	server := governedServerForTest("transformation-storage-authority", store)
	server.objectCleaner = cleaner
	server.now = func() time.Time { return revokedAt.Add(6 * time.Minute) }
	request := func() *http.Request {
		return governedReservationCleanupRequest(WorkspaceStorageScope(workspaceA))
	}

	failed := httptest.NewRecorder()
	server.handleCleanupGovernedReservations(failed, request())
	if failed.Code != http.StatusServiceUnavailable || store.confirmRemovalCalls != 0 {
		t.Fatalf("failed status = %d, confirmations = %d, body = %s",
			failed.Code, store.confirmRemovalCalls, failed.Body.String())
	}
	assertReservationCleanupResponse(t, failed.Body.Bytes(), "revoked", "pending", 1)

	cleaner.failKey = ""
	recovered := httptest.NewRecorder()
	server.handleCleanupGovernedReservations(recovered, request())
	if recovered.Code != http.StatusOK || store.confirmRemovalCalls != 1 {
		t.Fatalf("recovered status = %d, confirmations = %d, body = %s",
			recovered.Code, store.confirmRemovalCalls, recovered.Body.String())
	}
	assertReservationCleanupResponse(t, recovered.Body.Bytes(), "revoked", "confirmed", 0)

	retry := httptest.NewRecorder()
	server.handleCleanupGovernedReservations(retry, request())
	if retry.Code != http.StatusOK || store.confirmRemovalCalls != 1 {
		t.Fatalf("retry status = %d, confirmations = %d, body = %s",
			retry.Code, store.confirmRemovalCalls, retry.Body.String())
	}
}

func TestReservationCleanupRotatesPastFailedBatch(t *testing.T) {
	revokedAt := time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)
	store := deletionStore(revokedAt)
	store.reservationDeletions = make([]GovernedObjectDeletion, 0, maxReservationCleanupBatch+1)
	failKeys := make(map[string]bool, maxReservationCleanupBatch)
	for index := 0; index <= maxReservationCleanupBatch; index++ {
		deletion := store.deletion
		deletion.Object.ID = fmt.Sprintf("00000000-0000-4000-8000-%012d", index)
		deletion.Object.Key = fmt.Sprintf("governed/staging/%d", index)
		deletion.StagingKey = deletion.Object.Key
		deletion.PhysicalRemovalConfirmedAt = nil
		store.reservationDeletions = append(store.reservationDeletions, deletion)
		if index < maxReservationCleanupBatch {
			failKeys[deletion.StagingKey] = true
		}
	}
	cleaner := &fakeObjectCleaner{barrierStore: store, failKeys: failKeys}
	server := governedServerForTest("transformation-storage-authority", store)
	server.objectCleaner = cleaner
	now := revokedAt.Add(6 * time.Minute)
	server.now = func() time.Time {
		now = now.Add(time.Second)
		return now
	}
	request := func() *http.Request {
		return governedReservationCleanupRequest(WorkspaceStorageScope(workspaceA))
	}

	first := httptest.NewRecorder()
	server.handleCleanupGovernedReservations(first, request())
	if first.Code != http.StatusServiceUnavailable || store.confirmRemovalCalls != 0 {
		t.Fatalf("first cleanup status = %d, confirmations = %d", first.Code, store.confirmRemovalCalls)
	}

	second := httptest.NewRecorder()
	server.handleCleanupGovernedReservations(second, request())
	last := store.reservationDeletions[maxReservationCleanupBatch]
	if second.Code != http.StatusServiceUnavailable || store.confirmRemovalCalls != 1 ||
		last.PhysicalRemovalConfirmedAt == nil {
		t.Fatalf("second cleanup status = %d, confirmations = %d, last confirmation = %v",
			second.Code, store.confirmRemovalCalls, last.PhysicalRemovalConfirmedAt)
	}
}

func TestReservationCleanupIsExactWorkspaceScoped(t *testing.T) {
	revokedAt := time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)
	store := deletionStore(revokedAt)
	store.reservationDeletions = []GovernedObjectDeletion{store.deletion}
	cleaner := &fakeObjectCleaner{barrierStore: store}
	server := governedServerForTest("transformation-storage-authority", store)
	server.objectCleaner = cleaner
	server.now = func() time.Time { return revokedAt.Add(6 * time.Minute) }
	response := httptest.NewRecorder()

	server.handleCleanupGovernedReservations(response,
		governedReservationCleanupRequest(WorkspaceStorageScope(workspaceB)))

	if response.Code != http.StatusOK || len(cleaner.deleted) != 0 ||
		store.reservationDeletions[0].PhysicalRemovalConfirmedAt != nil {
		t.Fatalf("status = %d, deleted = %#v, other workspace confirmation = %v, body = %s",
			response.Code, cleaner.deleted, store.reservationDeletions[0].PhysicalRemovalConfirmedAt,
			response.Body.String())
	}
}

func TestReservationCleanupRejectsControllerBeforeStore(t *testing.T) {
	store := deletionStore(time.Now())
	server := governedServerForTest("transformation-sandbox-controller", store)
	response := httptest.NewRecorder()

	server.handleCleanupGovernedReservations(response,
		governedReservationCleanupRequest(WorkspaceStorageScope(workspaceA)))

	if response.Code != http.StatusForbidden || store.tombstoneReservations != 0 {
		t.Fatalf("status = %d, tombstone reservations = %d",
			response.Code, store.tombstoneReservations)
	}
}

func TestAnalysisArtifactsAreAcceptedOnlyAsOrdinaryObjects(t *testing.T) {
	for _, artifactClass := range []ArtifactClass{"analysis_manifest", "analysis_result"} {
		if !validArtifactClass(artifactClass) {
			t.Errorf("ordinary artifact class %q was rejected", artifactClass)
		}
		if validGrantOutputClass(artifactClass) {
			t.Errorf("analysis artifact class %q was accepted as a grant output", artifactClass)
		}
	}
}

func TestGovernedObjectDeleteCleansUnreferencedLegacyStaging(t *testing.T) {
	revokedAt := time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)
	store := deletionStore(revokedAt)
	store.deletion.StagingKey = ""
	prefix := governedStagingPrefix(store.deletion.Object.Scope, store.deletion.Object.ArtifactClass)
	legacyKey := prefix + "legacy-reservation"
	cleaner := &fakeObjectCleaner{listed: []string{legacyKey}, barrierStore: store}
	server := governedServerForTest("transformation-storage-authority", store)
	server.objectCleaner = cleaner
	now := revokedAt.Add(6 * time.Minute)
	server.now = func() time.Time { return now }
	first := httptest.NewRecorder()

	server.handleDeleteGovernedObject(first,
		governedDeleteRequest(store.deletion.Object.ID, WorkspaceStorageScope(workspaceA)))

	if first.Code != http.StatusAccepted || store.recordSweepCalls != 1 || store.confirmRemovalCalls != 0 ||
		!slices.Contains(cleaner.deleted, "nimmly-files/"+legacyKey) {
		t.Fatalf("first status = %d, sweeps = %d, confirmations = %d, deleted = %#v, body = %s",
			first.Code, store.recordSweepCalls, store.confirmRemovalCalls, cleaner.deleted, first.Body.String())
	}
	var firstBody governedObjectDeletionResponse
	if err := json.Unmarshal(first.Body.Bytes(), &firstBody); err != nil {
		t.Fatal(err)
	}
	if first.Header().Get("Retry-After") != "300" || firstBody.RetryAfterSeconds != 300 {
		t.Fatalf("legacy settling retry = header %q, body %d; want 300",
			first.Header().Get("Retry-After"), firstBody.RetryAfterSeconds)
	}
	if !slices.Contains(store.registrationLocks, "nimmly/nimmly-files") || store.registrationBarrier {
		t.Fatalf("legacy staging deletion registration barrier = %#v, held after response = %t",
			store.registrationLocks, store.registrationBarrier)
	}
	now = now.Add(2 * time.Minute)
	middle := httptest.NewRecorder()
	server.handleDeleteGovernedObject(middle,
		governedDeleteRequest(store.deletion.Object.ID, WorkspaceStorageScope(workspaceA)))
	if middle.Code != http.StatusAccepted || cleaner.walks != 1 || store.confirmRemovalCalls != 0 {
		t.Fatalf("settling retry status = %d, walks = %d, confirmations = %d, body = %s",
			middle.Code, cleaner.walks, store.confirmRemovalCalls, middle.Body.String())
	}
	now = now.Add(3 * time.Minute)
	second := httptest.NewRecorder()
	server.handleDeleteGovernedObject(second,
		governedDeleteRequest(store.deletion.Object.ID, WorkspaceStorageScope(workspaceA)))
	if second.Code != http.StatusServiceUnavailable || store.confirmRemovalCalls != 0 || cleaner.walks != 2 {
		t.Fatalf("second status = %d, confirmations = %d, body = %s",
			second.Code, store.confirmRemovalCalls, second.Body.String())
	}
}

func TestGovernedObjectDeleteKeepsLegacyRemovalPendingWhileStagingIsReferenced(t *testing.T) {
	revokedAt := time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)
	store := deletionStore(revokedAt)
	store.deletion.StagingKey = ""
	prefix := governedStagingPrefix(store.deletion.Object.Scope, store.deletion.Object.ArtifactClass)
	liveKey := prefix + "live-reservation"
	orphanedKey := prefix + "orphaned-upload"
	store.referencedKeys = map[string]bool{"nimmly/nimmly-files/" + liveKey: true}
	store.livePrefixReferenced = true
	cleaner := &fakeObjectCleaner{listed: []string{liveKey, orphanedKey}}
	server := governedServerForTest("transformation-storage-authority", store)
	server.objectCleaner = cleaner
	server.now = func() time.Time { return revokedAt.Add(6 * time.Minute) }
	response := httptest.NewRecorder()

	server.handleDeleteGovernedObject(response,
		governedDeleteRequest(store.deletion.Object.ID, WorkspaceStorageScope(workspaceA)))

	if response.Code != http.StatusServiceUnavailable || store.confirmRemovalCalls != 0 ||
		slices.Contains(cleaner.deleted, "nimmly-files/"+liveKey) ||
		!slices.Contains(cleaner.deleted, "nimmly-files/"+orphanedKey) {
		t.Fatalf("status = %d, confirmations = %d, deleted = %#v, body = %s",
			response.Code, store.confirmRemovalCalls, cleaner.deleted, response.Body.String())
	}
	assertDeletionResponse(t, response.Body.Bytes(), "revoked", "pending")
}

func TestGovernedObjectDeleteKeepsLegacyRemovalPendingWhenListingOmitsLiveKey(t *testing.T) {
	revokedAt := time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)
	store := deletionStore(revokedAt)
	store.deletion.StagingKey = ""
	store.livePrefixReferenced = true
	cleaner := &fakeObjectCleaner{barrierStore: store}
	server := governedServerForTest("transformation-storage-authority", store)
	server.objectCleaner = cleaner
	server.now = func() time.Time { return revokedAt.Add(6 * time.Minute) }
	response := httptest.NewRecorder()

	server.handleDeleteGovernedObject(response,
		governedDeleteRequest(store.deletion.Object.ID, WorkspaceStorageScope(workspaceA)))

	if response.Code != http.StatusServiceUnavailable || store.recordSweepCalls != 0 ||
		store.confirmRemovalCalls != 0 {
		t.Fatalf("status = %d, sweeps = %d, confirmations = %d, body = %s",
			response.Code, store.recordSweepCalls, store.confirmRemovalCalls, response.Body.String())
	}
	assertDeletionResponse(t, response.Body.Bytes(), "revoked", "pending")
}

func TestGovernedObjectDeleteRefusesCleanupForHistoricallySharedBucket(t *testing.T) {
	revokedAt := time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)
	store := deletionStore(revokedAt)
	store.nonExclusiveBucket = true
	cleaner := &fakeObjectCleaner{barrierStore: store}
	server := governedServerForTest("transformation-storage-authority", store)
	server.objectCleaner = cleaner
	server.now = func() time.Time { return revokedAt.Add(6 * time.Minute) }
	response := httptest.NewRecorder()

	server.handleDeleteGovernedObject(response,
		governedDeleteRequest(store.deletion.Object.ID, WorkspaceStorageScope(workspaceA)))

	if response.Code != http.StatusServiceUnavailable || len(cleaner.deleted) != 0 ||
		store.confirmRemovalCalls != 0 || store.registrationBarrier {
		t.Fatalf("status = %d, deleted = %#v, confirmations = %d, barrier held = %t, body = %s",
			response.Code, cleaner.deleted, store.confirmRemovalCalls, store.registrationBarrier,
			response.Body.String())
	}
	assertDeletionResponse(t, response.Body.Bytes(), "revoked", "pending")
}

func TestRegisterExistingHoldsPhysicalKeyLockThroughRegistration(t *testing.T) {
	payload := []byte("legacy governed bytes")
	digest := sha256.Sum256(payload)
	contentHash := hex.EncodeToString(digest[:])
	key := "governed-staging/workspace/" + workspaceA + "/transformation_sql/legacy"
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/nimmly-files/"+key {
			t.Errorf("verification request = %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write(payload)
	}))
	defer backend.Close()
	store := &fakeGovernedStore{object: GovernedObject{
		ID: "44444444-4444-4444-8444-444444444444", Tenant: "nimmly",
		Scope: WorkspaceStorageScope(workspaceA), ArtifactClass: ArtifactTransformationSQL,
		ContentHash: contentHash, ByteSize: int64(len(payload)), MediaType: "text/sql",
		State: ObjectStateActive, Bucket: "nimmly-files", Key: key,
	}}
	server := governedServerForTest("transformation-storage-authority", store)
	server.presigner = NewPresigner(&Config{
		S3Endpoint: backend.URL, S3Region: "us-east-1",
		S3AccessKey: "broker", S3SecretKey: "secret",
	})
	body, _ := json.Marshal(map[string]any{
		"authority": WorkspaceStorageScope(workspaceA), "artifactClass": ArtifactTransformationSQL,
		"contentHash": contentHash, "byteSize": len(payload), "mediaType": "text/sql", "physicalKey": key,
	})
	request := httptest.NewRequest(http.MethodPost, "/v2/objects/register-existing", strings.NewReader(string(body)))
	request.Header.Set("Authorization", "Bearer test-token")
	response := httptest.NewRecorder()

	server.handleRegisterGovernedObject(response, request)

	if response.Code != http.StatusCreated || !store.registerSawKeyLock || !store.registerSawBarrier ||
		store.physicalKeyLockHeld || store.registrationBarrier {
		t.Fatalf("status = %d, registered under key/barrier locks = %t/%t, locks held after request = %t/%t, body = %s",
			response.Code, store.registerSawKeyLock, store.registerSawBarrier,
			store.physicalKeyLockHeld, store.registrationBarrier, response.Body.String())
	}
	wantLock := "nimmly/nimmly-files/" + key
	if !slices.Contains(store.physicalKeyLockCalls, wantLock) {
		t.Fatalf("physical key lock calls = %#v, want %q", store.physicalKeyLockCalls, wantLock)
	}
}

func TestRegisterExistingRejectsHistoricallyForeignBucketBeforeVerification(t *testing.T) {
	store := &fakeGovernedStore{nonExclusiveBucket: true}
	server := governedServerForTest("transformation-storage-authority", store)
	request := httptest.NewRequest(http.MethodPost, "/v2/objects/register-existing", strings.NewReader(`{
		"authority":{"kind":"tenant"},
		"artifactClass":"transformation_work_bundle",
		"contentHash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"byteSize":12,
		"mediaType":"application/octet-stream",
		"physicalKey":"governed/test/foreign"
	}`))
	request.Header.Set("Authorization", "Bearer test-token")
	response := httptest.NewRecorder()

	server.handleRegisterGovernedObject(response, request)

	if response.Code != http.StatusConflict || store.registrationBarrier || store.registerSawKeyLock {
		t.Fatalf("status = %d, barrier held = %t, registered = %t, body = %s",
			response.Code, store.registrationBarrier, store.registerSawKeyLock, response.Body.String())
	}
}

func TestLegacyPresignRejectsConflictingPersistedBucketOwner(t *testing.T) {
	store := &fakeGovernedStore{bucketOwnerConflict: true}
	server := &server{
		cfg: &Config{MaxBodyBytes: 4096},
		policy: &Policy{
			DefaultTTLSeconds: TTLConfig{Put: 300},
			Namespaces: map[string]NamespacePolicy{
				"nimmly": {Bucket: "nimmly-files", Ops: []string{"put"}, Prefixes: []string{"corpus/"}},
			},
		},
		appAuth:       fixedReviewer{namespace: "nimmly", account: "crawler"},
		governedStore: store,
		log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/presign",
		strings.NewReader(`{"op":"put","key":"corpus/new.txt","contentType":"text/plain"}`))
	request.Header.Set("Authorization", "Bearer test-token")
	response := httptest.NewRecorder()

	server.handlePresign(response, request)

	if response.Code != http.StatusConflict ||
		!slices.Contains(store.claimedBuckets, "nimmly/nimmly-files") {
		t.Fatalf("status = %d, claims = %#v, body = %s",
			response.Code, store.claimedBuckets, response.Body.String())
	}
}

func TestLegacyPresignReportsTransientBucketOwnershipFailure(t *testing.T) {
	store := &fakeGovernedStore{bucketOwnerErr: errors.New("database unavailable")}
	server := &server{
		cfg: &Config{MaxBodyBytes: 4096},
		policy: &Policy{
			DefaultTTLSeconds: TTLConfig{Put: 300},
			Namespaces: map[string]NamespacePolicy{
				"nimmly": {Bucket: "nimmly-files", Ops: []string{"put"}, Prefixes: []string{"corpus/"}},
			},
		},
		appAuth:       fixedReviewer{namespace: "nimmly", account: "crawler"},
		governedStore: store,
		log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/presign",
		strings.NewReader(`{"op":"put","key":"corpus/new.txt","contentType":"text/plain"}`))
	request.Header.Set("Authorization", "Bearer test-token")
	response := httptest.NewRecorder()

	server.handlePresign(response, request)

	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body = %s", response.Code, response.Body.String())
	}
}

func TestMultipartReportsTransientBucketOwnershipFailure(t *testing.T) {
	store := &fakeGovernedStore{bucketOwnerErr: errors.New("database unavailable")}
	server := &server{
		cfg: &Config{MaxBodyBytes: 4096},
		policy: &Policy{
			DefaultTTLSeconds: TTLConfig{Multipart: 300},
			Namespaces: map[string]NamespacePolicy{
				"nimmly": {Bucket: "nimmly-files", Ops: []string{"multipart"}, Prefixes: []string{"corpus/"}},
			},
		},
		appAuth:       fixedReviewer{namespace: "nimmly", account: "crawler"},
		governedStore: store,
		log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/multipart/create", strings.NewReader(`{}`))
	request.Header.Set("Authorization", "Bearer test-token")
	response := httptest.NewRecorder()

	if _, ok := server.authorizeMultipart(response, request, "corpus/new.txt"); ok ||
		response.Code != http.StatusInternalServerError {
		t.Fatalf("ok = %t, status = %d, want false/500; body = %s",
			ok, response.Code, response.Body.String())
	}
}

func TestGovernedObjectDeleteRequiresExactWorkspaceAuthority(t *testing.T) {
	revokedAt := time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)
	store := deletionStore(revokedAt)
	server := governedServerForTest("transformation-storage-authority", store)
	server.objectCleaner = &fakeObjectCleaner{}
	server.now = func() time.Time { return revokedAt }
	for _, authority := range []StorageScope{TenantStorageScope(), WorkspaceStorageScope(workspaceB)} {
		response := httptest.NewRecorder()
		server.handleDeleteGovernedObject(response, governedDeleteRequest(store.deletion.Object.ID, authority))
		if response.Code != http.StatusForbidden {
			t.Fatalf("authority %#v status = %d, want 403", authority, response.Code)
		}
	}
}

func TestGovernedObjectDeleteRejectsControllerBeforeStore(t *testing.T) {
	store := deletionStore(time.Now())
	server := governedServerForTest("transformation-sandbox-controller", store)
	response := httptest.NewRecorder()

	server.handleDeleteGovernedObject(response,
		governedDeleteRequest(store.deletion.Object.ID, WorkspaceStorageScope(workspaceA)))

	if response.Code != http.StatusForbidden || store.acquireObjectLockCalls != 0 || store.tombstoneCalls != 0 {
		t.Fatalf("status = %d, locks = %d, tombstones = %d",
			response.Code, store.acquireObjectLockCalls, store.tombstoneCalls)
	}
}

func deletionStore(revokedAt time.Time) *fakeGovernedStore {
	object := GovernedObject{
		ID: "44444444-4444-4444-8444-444444444444", Tenant: "nimmly",
		Scope: WorkspaceStorageScope(workspaceA), ArtifactClass: ArtifactTransformationSQL,
		ContentHash: testContentHash, ByteSize: 12, MediaType: "text/sql", State: ObjectStateDeleted,
		Bucket: "nimmly-files",
	}
	object.Key = governedObjectKey(object.Scope, object.ArtifactClass, object.ContentHash, object.ID)
	return &fakeGovernedStore{object: object, deletion: GovernedObjectDeletion{
		Object: object, AccessRevokedAt: revokedAt,
		StagingKey: "governed-staging/workspace/" + workspaceA + "/transformation_sql/staged",
	}}
}

func governedDeleteRequest(objectID string, authority StorageScope) *http.Request {
	body, _ := json.Marshal(map[string]any{"authority": authority})
	request := httptest.NewRequest(http.MethodDelete, "/v2/objects/"+objectID, strings.NewReader(string(body)))
	request.Header.Set("Authorization", "Bearer test-token")
	request.SetPathValue("objectId", objectID)
	return request
}

func governedReservationCleanupRequest(authority StorageScope) *http.Request {
	body, _ := json.Marshal(map[string]any{"authority": authority})
	request := httptest.NewRequest(http.MethodDelete, "/v2/reservations", strings.NewReader(string(body)))
	request.Header.Set("Authorization", "Bearer test-token")
	return request
}

func assertDeletionResponse(t *testing.T, encoded []byte, accessState, physicalState string) {
	t.Helper()
	var body struct {
		ObjectID             string `json:"objectId"`
		AccessState          string `json:"accessState"`
		PhysicalRemovalState string `json:"physicalRemovalState"`
	}
	if err := json.Unmarshal(encoded, &body); err != nil {
		t.Fatal(err)
	}
	if body.ObjectID == "" || body.AccessState != accessState || body.PhysicalRemovalState != physicalState {
		t.Fatalf("unexpected deletion response: %#v", body)
	}
}

func assertReservationCleanupResponse(
	t *testing.T,
	encoded []byte,
	accessState string,
	physicalState string,
	pending int,
) {
	t.Helper()
	var body struct {
		AccessState             string `json:"accessState"`
		PhysicalRemovalState    string `json:"physicalRemovalState"`
		PendingReservationCount int    `json:"pendingReservationCount"`
	}
	if err := json.Unmarshal(encoded, &body); err != nil {
		t.Fatal(err)
	}
	if body.AccessState != accessState || body.PhysicalRemovalState != physicalState ||
		body.PendingReservationCount != pending {
		t.Fatalf("unexpected reservation cleanup response: %#v", body)
	}
}

func TestGovernedObjectTicketReturnsNoPhysicalLocator(t *testing.T) {
	store := &fakeGovernedStore{object: GovernedObject{
		ID: "44444444-4444-4444-8444-444444444444", Tenant: "nimmly",
		Scope: WorkspaceStorageScope(workspaceA), ArtifactClass: ArtifactWorkBundle,
		ContentHash: testContentHash, ByteSize: 12, MediaType: "application/vnd.apache.parquet",
		State: ObjectStateActive, Bucket: "nimmly-files", Key: "secret/physical/key.parquet",
	}}
	server := governedServerForTest("transformation-sandbox-controller", store)
	request := httptest.NewRequest(http.MethodPost, "/v2/grants/55555555-5555-4555-8555-555555555555/objects/44444444-4444-4444-8444-444444444444/ticket",
		strings.NewReader(`{"executionId":"33333333-3333-4333-8333-333333333333"}`))
	request.Header.Set("Authorization", "Bearer test-token")
	request.SetPathValue("grantId", "55555555-5555-4555-8555-555555555555")
	request.SetPathValue("objectId", store.object.ID)
	response := httptest.NewRecorder()

	server.handleGovernedObjectTicket(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if store.consumeCalls != 1 {
		t.Fatalf("consume calls = %d, want 1", store.consumeCalls)
	}
	var raw struct {
		Object map[string]any `json:"object"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if _, exposed := raw.Object["key"]; exposed {
		t.Fatalf("object reference exposed physical key: %#v", raw.Object)
	}
	if _, exposed := raw.Object["bucket"]; exposed {
		t.Fatalf("object reference exposed physical bucket: %#v", raw.Object)
	}
	var body struct {
		Object struct {
			SchemaVersion int          `json:"schemaVersion"`
			ObjectID      string       `json:"objectId"`
			Authority     StorageScope `json:"authority"`
		} `json:"object"`
		Transfer struct {
			Method string `json:"method"`
		} `json:"transfer"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Object.SchemaVersion != 2 || body.Object.ObjectID != store.object.ID ||
		body.Object.Authority != WorkspaceStorageScope(workspaceA) || body.Transfer.Method != http.MethodGet {
		t.Fatalf("unexpected response: %#v", body)
	}
}

func TestGovernedGrantIssueRejectsControllerRoleBeforeStore(t *testing.T) {
	store := &fakeGovernedStore{}
	server := governedServerForTest("transformation-sandbox-controller", store)
	request := httptest.NewRequest(http.MethodPost, "/v2/grants", strings.NewReader(`{}`))
	request.Header.Set("Authorization", "Bearer test-token")
	response := httptest.NewRecorder()

	server.handleIssueGovernedGrant(response, request)

	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", response.Code)
	}
	if store.issueCalls != 0 {
		t.Fatalf("issue calls = %d, want 0", store.issueCalls)
	}
}

func TestGovernedGrantIssueRequiresAndForwardsCleanupOwner(t *testing.T) {
	store := &fakeGovernedStore{grant: ExecutionGrant{SchemaVersion: 1,
		ID: "44444444-4444-4444-8444-444444444444", Tenant: "nimmly",
		Scope: WorkspaceStorageScope(workspaceA),
	}}
	server := governedServerForTest("transformation-storage-authority", store)
	expiresAt := time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
	requestBody := func(owner string) string {
		return fmt.Sprintf(`{
			"authority":{"kind":"workspace","workspaceId":"%s"},
			"executionId":"33333333-3333-4333-8333-333333333333",
			"readableObjectIds":["55555555-5555-4555-8555-555555555555"],
			"writableArtifactClasses":["sandbox_relation"],
			"expiresAt":%q,
			"cleanupOwnerId":%q
		}`, workspaceA, expiresAt, owner)
	}
	request := httptest.NewRequest(http.MethodPost, "/v2/grants",
		strings.NewReader(requestBody("22222222-2222-4222-8222-222222222222")))
	request.Header.Set("Authorization", "Bearer test-token")
	response := httptest.NewRecorder()
	server.handleIssueGovernedGrant(response, request)
	if response.Code != http.StatusCreated ||
		store.issued.CleanupOwnerID != "22222222-2222-4222-8222-222222222222" {
		t.Fatalf("grant issue = %d, input %#v, body %s", response.Code, store.issued, response.Body.String())
	}
	missing := httptest.NewRequest(http.MethodPost, "/v2/grants", strings.NewReader(requestBody("")))
	missing.Header.Set("Authorization", "Bearer test-token")
	missingResponse := httptest.NewRecorder()
	server.handleIssueGovernedGrant(missingResponse, missing)
	if missingResponse.Code != http.StatusBadRequest || store.issueCalls != 1 {
		t.Fatalf("missing owner = %d, issue calls %d", missingResponse.Code, store.issueCalls)
	}
}

func TestGovernedGrantIssueEncodesReadOnlyGrantForClients(t *testing.T) {
	expiresAt := time.Now().Add(time.Hour).Truncate(time.Second).UTC()
	clientExpiry := expiresAt.Format("2006-01-02T15:04:05.000Z07:00") // JavaScript toISOString shape
	store := &fakeGovernedStore{grant: ExecutionGrant{SchemaVersion: 1,
		ID: "44444444-4444-4444-8444-444444444444", Tenant: "nimmly",
		Scope: WorkspaceStorageScope(workspaceA), ExecutionID: "33333333-3333-4333-8333-333333333333",
		ReadableObjectIDs: []string{"55555555-5555-4555-8555-555555555555"},
		// A read-only grant, carrying the non-UTC location a database scan can return.
		ExpiresAt: expiresAt.In(time.FixedZone("CEST", 2*60*60)),
	}}
	server := governedServerForTest("transformation-storage-authority", store)
	request := httptest.NewRequest(http.MethodPost, "/v2/grants", strings.NewReader(fmt.Sprintf(`{
		"authority":{"kind":"workspace","workspaceId":"%s"},
		"executionId":"33333333-3333-4333-8333-333333333333",
		"readableObjectIds":["55555555-5555-4555-8555-555555555555"],
		"writableArtifactClasses":[],
		"expiresAt":%q,
		"cleanupOwnerId":"22222222-2222-4222-8222-222222222222"
	}`, workspaceA, clientExpiry)))
	request.Header.Set("Authorization", "Bearer test-token")
	response := httptest.NewRecorder()

	server.handleIssueGovernedGrant(response, request)

	body := response.Body.String()
	if response.Code != http.StatusCreated {
		t.Fatalf("grant issue = %d, body %s", response.Code, body)
	}
	for _, want := range []string{
		`"writableArtifactClasses":[]`,
		`"readableObjectIds":["55555555-5555-4555-8555-555555555555"]`,
		`"expiresAt":"` + clientExpiry + `"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("grant body %s does not contain %s", body, want)
		}
	}
	if strings.Contains(body, "null") || strings.Contains(body, "CleanupOwnerID") {
		t.Fatalf("grant body leaks null or internal fields: %s", body)
	}
}

func TestExecutionGrantJSONNeverEncodesNullLists(t *testing.T) {
	encoded, err := json.Marshal(ExecutionGrant{SchemaVersion: 1,
		ExpiresAt: time.Date(2026, 10, 1, 8, 0, 0, 123456789, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	body := string(encoded)
	for _, want := range []string{
		`"readableObjectIds":[]`,
		`"writableArtifactClasses":[]`,
		`"expiresAt":"2026-10-01T08:00:00.123456789Z"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("grant JSON %s does not contain %s", body, want)
		}
	}
}

func TestGovernedObjectReservePersistsWorkspaceScopeWithoutExposingLocator(t *testing.T) {
	store := &fakeGovernedStore{object: GovernedObject{
		ID: "44444444-4444-4444-8444-444444444444", Tenant: "nimmly",
		Scope: WorkspaceStorageScope(workspaceA), ArtifactClass: ArtifactTransformationSQL,
		ContentHash: testContentHash, ByteSize: 12, MediaType: "text/sql",
		State: ObjectStateReserved, Bucket: "nimmly-files",
		Key: "governed-staging/workspace/" + workspaceA + "/transformation_sql/44444444-4444-4444-8444-444444444444",
	}}
	server := governedServerForTest("transformation-storage-authority", store)
	request := httptest.NewRequest(http.MethodPost, "/v2/objects/reserve", strings.NewReader(`{
		"authority":{"kind":"workspace","workspaceId":"11111111-1111-4111-8111-111111111111"},
		"artifactClass":"transformation_sql",
		"contentHash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"byteSize":12,
		"mediaType":"text/sql",
		"cleanupOwnerId":"22222222-2222-4222-8222-222222222222"
	}`))
	request.Header.Set("Authorization", "Bearer test-token")
	response := httptest.NewRecorder()

	server.handleReserveGovernedObject(response, request)

	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if store.reserveCalls != 1 || store.reserved.Scope != WorkspaceStorageScope(workspaceA) {
		t.Fatalf("reserve input = %#v", store.reserved)
	}
	if store.reserved.CleanupOwnerID != "22222222-2222-4222-8222-222222222222" {
		t.Fatalf("cleanup owner = %q", store.reserved.CleanupOwnerID)
	}
	if store.ticketsIssuedInReservation != 1 || store.reservationOpen || store.registrationBarrier {
		t.Fatalf("upload tickets signed inside reservation = %d, reservation open = %t, session barrier = %t",
			store.ticketsIssuedInReservation, store.reservationOpen, store.registrationBarrier)
	}
	var body struct {
		Object   map[string]any   `json:"object"`
		Transfer governedTransfer `json:"transfer"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if _, exposed := body.Object["key"]; exposed {
		t.Fatalf("object reference exposed physical key: %#v", body.Object)
	}
	if _, exposed := body.Object["bucket"]; exposed {
		t.Fatalf("object reference exposed physical bucket: %#v", body.Object)
	}
	if body.Transfer.Method != http.MethodPut || body.Transfer.URL == "" {
		t.Fatalf("reservation transfer = %#v", body.Transfer)
	}
	if !strings.HasPrefix(store.reserved.Key, "governed-staging/workspace/"+workspaceA+"/") {
		t.Fatalf("upload did not use a broker staging key: %q", store.reserved.Key)
	}
}

func TestGovernedObjectReserveRejectsHistoricallyForeignBucket(t *testing.T) {
	store := &fakeGovernedStore{nonExclusiveBucket: true}
	server := governedServerForTest("transformation-storage-authority", store)
	request := httptest.NewRequest(http.MethodPost, "/v2/objects/reserve", strings.NewReader(`{
		"authority":{"kind":"tenant"},
		"artifactClass":"transformation_work_bundle",
		"contentHash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"byteSize":12,
		"mediaType":"application/octet-stream",
		"cleanupOwnerId":"22222222-2222-4222-8222-222222222222"
	}`))
	request.Header.Set("Authorization", "Bearer test-token")
	response := httptest.NewRecorder()

	server.handleReserveGovernedObject(response, request)

	if response.Code != http.StatusConflict || store.reserveCalls != 0 || store.registrationBarrier {
		t.Fatalf("status = %d, reserves = %d, barrier held = %t, body = %s",
			response.Code, store.reserveCalls, store.registrationBarrier, response.Body.String())
	}
}

func TestCleanupOwnerRevokesReservedAndActiveWithoutCrossingOwnerOrClass(t *testing.T) {
	ownerID := "22222222-2222-4222-8222-222222222222"
	otherOwnerID := "33333333-3333-4333-8333-333333333333"
	makeDeletion := func(id string, state ObjectState, class ArtifactClass) GovernedObjectDeletion {
		return GovernedObjectDeletion{Object: GovernedObject{
			ID: id, Tenant: "nimmly", Scope: WorkspaceStorageScope(workspaceA), ArtifactClass: class,
			ContentHash: testContentHash, ByteSize: 12, MediaType: "application/octet-stream",
			State: state, Bucket: "nimmly-files", Key: "staging/" + id,
		}, StagingKey: "staging/" + id}
	}
	reserved := makeDeletion("44444444-4444-4444-8444-444444444444", ObjectStateReserved,
		ArtifactTransformationSQL)
	active := makeDeletion("55555555-5555-4555-8555-555555555555", ObjectStateActive,
		ArtifactAnalysisResult)
	other := makeDeletion("66666666-6666-4666-8666-666666666666", ObjectStateActive,
		ArtifactAnalysisResult)
	lake := makeDeletion("77777777-7777-4777-8777-777777777777", ObjectStateActive,
		ArtifactDuckLakeData)
	store := &fakeGovernedStore{
		reservationDeletions: []GovernedObjectDeletion{reserved, active, other, lake},
		cleanupOwners: map[string]string{
			reserved.Object.ID: ownerID, active.Object.ID: ownerID,
			other.Object.ID: otherOwnerID, lake.Object.ID: ownerID,
		},
	}
	cleaner := &fakeObjectCleaner{barrierStore: store}
	server := governedServerForTest("transformation-storage-authority", store)
	server.objectCleaner = cleaner
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	server.now = func() time.Time { return now }
	request := func() *http.Request {
		r := httptest.NewRequest(http.MethodDelete, "/v2/cleanup-owners/"+ownerID+"/objects",
			strings.NewReader(`{"authority":{"kind":"workspace","workspaceId":"`+workspaceA+`"}}`))
		r.Header.Set("Authorization", "Bearer test-token")
		r.SetPathValue("cleanupOwnerId", ownerID)
		return r
	}
	first := httptest.NewRecorder()
	server.handleCleanupGovernedOwner(first, request())
	if first.Code != http.StatusAccepted || len(cleaner.deleted) != 0 {
		t.Fatalf("first cleanup = %d, deleted %#v, body %s", first.Code, cleaner.deleted, first.Body.String())
	}
	if store.reservationDeletions[0].Object.State != ObjectStateDeleted ||
		store.reservationDeletions[1].Object.State != ObjectStateDeleted ||
		store.reservationDeletions[2].Object.State != ObjectStateActive ||
		store.reservationDeletions[3].Object.State != ObjectStateActive {
		t.Fatalf("states after revocation = %#v", store.reservationDeletions)
	}
	now = now.Add(6 * time.Minute)
	cleaner.failKey = active.Object.Key
	second := httptest.NewRecorder()
	server.handleCleanupGovernedOwner(second, request())
	if second.Code != http.StatusServiceUnavailable || store.confirmRemovalCalls != 1 || cleaner.walks != 0 {
		t.Fatalf("second cleanup = %d, confirmations %d, walks %d, body %s",
			second.Code, store.confirmRemovalCalls, cleaner.walks, second.Body.String())
	}
	cleaner.failKey = ""
	third := httptest.NewRecorder()
	server.handleCleanupGovernedOwner(third, request())
	if third.Code != http.StatusOK || store.confirmRemovalCalls != 2 {
		t.Fatalf("retry cleanup = %d, confirmations %d, body %s",
			third.Code, store.confirmRemovalCalls, third.Body.String())
	}
	for _, deletion := range cleaner.deleted {
		if strings.Contains(deletion, other.Object.ID) || strings.Contains(deletion, lake.Object.ID) {
			t.Fatalf("cleanup crossed owner/class boundary: %#v", cleaner.deleted)
		}
	}
}

func TestWorkspaceOrdinaryCleanupRequiresWorkspaceAuthority(t *testing.T) {
	store := &fakeGovernedStore{}
	server := governedServerForTest("transformation-storage-authority", store)
	request := httptest.NewRequest(http.MethodDelete, "/v2/ordinary-objects",
		strings.NewReader(`{"authority":{"kind":"tenant"}}`))
	request.Header.Set("Authorization", "Bearer test-token")
	response := httptest.NewRecorder()
	server.handleCleanupGovernedWorkspace(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestGovernedOutputReserveSignsTicketInsideReservation(t *testing.T) {
	store := &fakeGovernedStore{object: GovernedObject{
		ID: "44444444-4444-4444-8444-444444444444", Tenant: "nimmly",
		Scope: WorkspaceStorageScope(workspaceA), ArtifactClass: ArtifactSandboxRelation,
		ContentHash: testContentHash, ByteSize: 12, MediaType: "application/parquet",
		State: ObjectStateReserved, Bucket: "nimmly-files",
		Key: "governed-staging/workspace/" + workspaceA + "/sandbox_relation/output",
	}}
	server := governedServerForTest("transformation-sandbox-controller", store)
	request := httptest.NewRequest(http.MethodPost, "/v2/grants/44444444-4444-4444-8444-444444444444/outputs/reserve",
		strings.NewReader(`{
			"executionId":"55555555-5555-4555-8555-555555555555",
			"artifactClass":"sandbox_relation",
			"contentHash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"byteSize":12,
			"mediaType":"application/parquet"
		}`))
	request.Header.Set("Authorization", "Bearer test-token")
	response := httptest.NewRecorder()

	server.handleReserveGovernedOutput(response, request)

	if response.Code != http.StatusCreated || store.ticketsIssuedInReservation != 1 ||
		store.reservationOpen || store.registrationBarrier {
		t.Fatalf("status = %d, tickets signed inside reservation = %d, session barrier = %t, body = %s",
			response.Code, store.ticketsIssuedInReservation, store.registrationBarrier, response.Body.String())
	}
	if !slices.Contains(store.claimedBuckets, "nimmly/nimmly-files") {
		t.Fatalf("output reservation skipped bucket registration check: %#v", store.claimedBuckets)
	}
}

func TestGovernedOutputReserveRejectsCleanupOwnerOverride(t *testing.T) {
	store := &fakeGovernedStore{}
	server := governedServerForTest("transformation-sandbox-controller", store)
	request := httptest.NewRequest(http.MethodPost,
		"/v2/grants/44444444-4444-4444-8444-444444444444/outputs/reserve",
		strings.NewReader(`{
			"executionId":"55555555-5555-4555-8555-555555555555",
			"artifactClass":"sandbox_relation",
			"contentHash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"byteSize":12,
			"mediaType":"application/parquet",
			"cleanupOwnerId":"22222222-2222-4222-8222-222222222222"
		}`))
	request.Header.Set("Authorization", "Bearer test-token")
	request.SetPathValue("grantId", "44444444-4444-4444-8444-444444444444")
	response := httptest.NewRecorder()
	server.handleReserveGovernedOutput(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestGovernedObjectReserveRejectsControllerRoleBeforeStore(t *testing.T) {
	store := &fakeGovernedStore{}
	server := governedServerForTest("transformation-sandbox-controller", store)
	request := httptest.NewRequest(http.MethodPost, "/v2/objects/reserve", strings.NewReader(`{}`))
	request.Header.Set("Authorization", "Bearer test-token")
	response := httptest.NewRecorder()

	server.handleReserveGovernedObject(response, request)

	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", response.Code)
	}
	if store.reserveCalls != 0 {
		t.Fatalf("reserve calls = %d, want 0", store.reserveCalls)
	}
}

func TestGovernedObjectReserveRejectsTrailingJSONBeforeStore(t *testing.T) {
	store := &fakeGovernedStore{}
	server := governedServerForTest("transformation-storage-authority", store)
	request := httptest.NewRequest(http.MethodPost, "/v2/objects/reserve", strings.NewReader(`{
		"authority":{"kind":"tenant"},
		"artifactClass":"transformation_sql",
		"contentHash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"byteSize":12,
		"mediaType":"text/sql"
	} trailing`))
	request.Header.Set("Authorization", "Bearer test-token")
	response := httptest.NewRecorder()

	server.handleReserveGovernedObject(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", response.Code)
	}
	if store.reserveCalls != 0 {
		t.Fatalf("reserve calls = %d, want 0", store.reserveCalls)
	}
}

func governedServerForTest(account string, store GovernedStore) *server {
	config := &Config{
		S3Endpoint: "https://s3.example.test", S3Region: "us-east-1",
		S3AccessKey: "test-access", S3SecretKey: "test-secret",
	}
	return &server{
		appAuth: fixedReviewer{namespace: "nimmly", account: account},
		policy: &Policy{
			DefaultTTLSeconds: TTLConfig{Get: 900, Put: 300},
			Namespaces: map[string]NamespacePolicy{
				"nimmly": {Bucket: "nimmly-files"},
			},
		},
		governedPolicy: &GovernedPolicy{Namespaces: map[string]GovernedNamespacePolicy{
			"nimmly": {ServiceAccounts: map[string][]GovernedOperation{
				"transformation-sandbox-controller": {OperationConsumeGrant, OperationRevokeGrant, OperationRegisterOutput},
				"transformation-storage-authority":  {OperationRegisterObject, OperationDeleteObject, OperationIssueGrant, OperationConsumeGrant, OperationRevokeGrant, OperationLakeDelete},
			}},
		}},
		governedStore: store,
		presigner:     NewPresigner(config),
		log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestGovernedReadTicketTakesNoMutationLock(t *testing.T) {
	store := &fakeGovernedStore{object: GovernedObject{
		ID: "44444444-4444-4444-8444-444444444444", Tenant: "nimmly",
		Scope: WorkspaceStorageScope(workspaceA), ArtifactClass: ArtifactTransformationSQL,
		ContentHash: testContentHash, ByteSize: 12, MediaType: "text/sql",
		State: ObjectStateActive, Bucket: "nimmly-files", Key: "governed/final/object",
	}}
	server := governedServerForTest("transformation-sandbox-controller", store)
	request := httptest.NewRequest(http.MethodPost,
		"/v2/grants/55555555-5555-4555-8555-555555555555/objects/44444444-4444-4444-8444-444444444444/ticket",
		strings.NewReader(`{"executionId":"66666666-6666-4666-8666-666666666666"}`))
	request.SetPathValue("grantId", "55555555-5555-4555-8555-555555555555")
	request.SetPathValue("objectId", "44444444-4444-4444-8444-444444444444")
	request.Header.Set("Authorization", "Bearer test-token")
	response := httptest.NewRecorder()

	server.handleGovernedObjectTicket(response, request)

	if response.Code != http.StatusOK || store.consumeCalls != 1 || store.acquireObjectLockCalls != 0 {
		t.Fatalf("status = %d, consume calls = %d, object lock calls = %d, body = %s",
			response.Code, store.consumeCalls, store.acquireObjectLockCalls, response.Body.String())
	}
}

func TestGovernedReserveRejectsHistoricallySharedBucketBeforeReserving(t *testing.T) {
	store := &fakeGovernedStore{nonExclusiveBucket: true}
	server := governedServerForTest("transformation-storage-authority", store)
	request := httptest.NewRequest(http.MethodPost, "/v2/objects/reserve", strings.NewReader(`{
		"authority":{"kind":"tenant"},
		"artifactClass":"transformation_sql",
		"contentHash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"byteSize":12,
		"mediaType":"text/sql",
		"cleanupOwnerId":"22222222-2222-4222-8222-222222222222"
	}`))
	request.Header.Set("Authorization", "Bearer test-token")
	response := httptest.NewRecorder()

	server.handleReserveGovernedObject(response, request)

	if response.Code != http.StatusConflict || store.reserveCalls != 0 {
		t.Fatalf("status = %d, reserve calls = %d, body = %s",
			response.Code, store.reserveCalls, response.Body.String())
	}
}

func TestGovernedReservationBarrierTimeoutIsRetryable(t *testing.T) {
	for name, call := range map[string]func(*server, http.ResponseWriter, *http.Request){
		"object": (*server).handleReserveGovernedObject,
		"output": (*server).handleReserveGovernedOutput,
	} {
		t.Run(name, func(t *testing.T) {
			store := &fakeGovernedStore{reserveErr: ErrRegistrationBusy}
			account, path, body := "transformation-storage-authority", "/v2/objects/reserve", `{
				"authority":{"kind":"tenant"},
				"artifactClass":"transformation_sql",
				"contentHash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				"byteSize":12,
				"mediaType":"text/sql",
				"cleanupOwnerId":"22222222-2222-4222-8222-222222222222"
			}`
			if name == "output" {
				account, path, body = "transformation-sandbox-controller",
					"/v2/grants/44444444-4444-4444-8444-444444444444/outputs/reserve", `{
					"executionId":"55555555-5555-4555-8555-555555555555",
					"artifactClass":"sandbox_relation",
					"contentHash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
					"byteSize":12,
					"mediaType":"application/parquet"
				}`
			}
			server := governedServerForTest(account, store)
			request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
			request.SetPathValue("grantId", "44444444-4444-4444-8444-444444444444")
			request.Header.Set("Authorization", "Bearer test-token")
			response := httptest.NewRecorder()

			call(server, response, request)

			if response.Code != http.StatusServiceUnavailable || response.Header().Get("Retry-After") == "" {
				t.Fatalf("status = %d, Retry-After = %q, body = %s", response.Code,
					response.Header().Get("Retry-After"), response.Body.String())
			}
		})
	}
}
