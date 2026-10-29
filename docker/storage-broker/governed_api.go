package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"time"
)

const maxReservationCleanupBatch = 64

type AppReviewer interface {
	Review(context.Context, string) (namespace, serviceAccount string, err error)
}

type GovernedStore interface {
	RegisterActiveObject(context.Context, RegisterActiveObjectInput) (GovernedObject, error)
	ReserveObject(context.Context, RegisterActiveObjectInput, ReservationIssuer) (GovernedObject, error)
	AuthorizeObjectReservation(context.Context, string, string) (GovernedObject, error)
	ActivateObject(context.Context, string, string, string, string) (GovernedObject, error)
	IssueGrant(context.Context, IssueGrantInput) (ExecutionGrant, error)
	ConsumeGrantObject(context.Context, ConsumeGrantObjectInput) (GovernedObject, error)
	ReserveGrantOutput(context.Context, ReserveGrantOutputInput, ReservationIssuer) (GovernedObject, error)
	AuthorizeGrantOutputReservation(context.Context, GrantOutputReservationInput) (GovernedObject, error)
	ActivateGrantOutput(context.Context, GrantOutputReservationInput, string, string) (GovernedObject, error)
	RevokeGrant(context.Context, string, string, time.Time) error
	AcquireObjectLock(context.Context, string, string) (func(), error)
	AcquireRegistrationBarrier(context.Context, string, string) (func(), error)
	AcquireRegistrationBarriers(context.Context, string, []string) (func(), error)
	AcquireRegisterExistingLock(context.Context, string, string, string) (func(), error)
	AcquireLakeRegistrationLock(context.Context, string, string, string) (func(), error)
	ClaimBucketOwnership(context.Context, string, string) error
	VerifyBucketRegistration(context.Context, string, string) error
	TombstoneObject(context.Context, string, StorageScope, string, time.Time) (GovernedObjectDeletion, error)
	ReservationCleanupBuckets(context.Context, string, StorageScope) ([]string, error)
	TombstoneReservations(context.Context, string, StorageScope, time.Time) error
	PendingReservationDeletions(context.Context, string, StorageScope, int) ([]GovernedObjectDeletion, error)
	RecordReservationCleanupAttempt(context.Context, string, StorageScope, string, time.Time) error
	ReservationCleanupStatus(context.Context, string, StorageScope) (int, *time.Time, error)
	OrdinaryCleanupBuckets(context.Context, string, StorageScope, string, bool) ([]string, error)
	TombstoneOrdinaryObjects(context.Context, string, StorageScope, string, bool, time.Time) error
	PendingOrdinaryObjectDeletions(context.Context, string, StorageScope, string, bool, int) ([]GovernedObjectDeletion, error)
	OrdinaryCleanupStatus(context.Context, string, StorageScope, string, bool) (int, *time.Time, error)
	RecordLegacyStagingSweep(context.Context, string, StorageScope, string, time.Time) (GovernedObjectDeletion, error)
	ConfirmObjectRemoval(context.Context, string, StorageScope, string, time.Time) (GovernedObjectDeletion, error)
	IsBucketExclusiveToTenant(context.Context, string, string) (bool, error)
	IsLiveObjectKeyReferenced(context.Context, string, string, string) (bool, error)
	HasLiveObjectKeyUnderPrefix(context.Context, string, string, string) (bool, error)
	AcquireLakePathLock(context.Context, string, string, string) (func(), error)
	AcquireLakePathLocks(context.Context, string, string, []string) (func(), error)
	ReserveLakeObject(context.Context, RegisterActiveObjectInput) (GovernedObject, error)
	ActivateLakeObject(context.Context, string, string) (GovernedObject, error)
	ReadLakeObject(context.Context, string, StorageScope, string, string) (GovernedObject, error)
	DeleteLakeObject(context.Context, string, StorageScope, string, string, time.Time) (GovernedObject, error)
	DeleteLakeObjects(context.Context, string, StorageScope, string, []string, time.Time) ([]GovernedObject, error)
	ConfirmLakeObjectRemoval(context.Context, string, StorageScope, string, string, time.Time) error
	InventoryLakeObjects(context.Context, string, StorageScope, string, int) ([]GovernedObject, bool, error)
}

type governedObjectCleaner interface {
	DeleteAndConfirmAbsent(context.Context, string, string) error
	WalkKeys(context.Context, string, string, func(string) error) error
}

type governedObjectRefV2 struct {
	SchemaVersion int          `json:"schemaVersion"`
	ObjectID      string       `json:"objectId"`
	TenantID      string       `json:"tenantId"`
	Authority     StorageScope `json:"authority"`
	ContentHash   string       `json:"contentHash"`
	ByteSize      int64        `json:"byteSize"`
	MediaType     string       `json:"mediaType"`
}

type governedTransfer struct {
	URL       string            `json:"url"`
	Method    string            `json:"method"`
	ExpiresAt string            `json:"expiresAt"`
	Headers   map[string]string `json:"headers,omitempty"`
}

type governedTicketResponse struct {
	Object   governedObjectRefV2 `json:"object"`
	Transfer governedTransfer    `json:"transfer"`
}

type issueGovernedGrantRequest struct {
	Authority               StorageScope    `json:"authority"`
	ExecutionID             string          `json:"executionId"`
	ReadableObjectIDs       []string        `json:"readableObjectIds"`
	WritableArtifactClasses []ArtifactClass `json:"writableArtifactClasses"`
	ExpiresAt               string          `json:"expiresAt"`
	CleanupOwnerID          string          `json:"cleanupOwnerId"`
}

type executionRequest struct {
	ExecutionID string `json:"executionId"`
}

type registerGovernedObjectRequest struct {
	Authority     StorageScope  `json:"authority"`
	ArtifactClass ArtifactClass `json:"artifactClass"`
	ContentHash   string        `json:"contentHash"`
	ByteSize      int64         `json:"byteSize"`
	MediaType     string        `json:"mediaType"`
	PhysicalKey   string        `json:"physicalKey"`
}

type reserveGovernedObjectRequest struct {
	Authority      StorageScope  `json:"authority"`
	ArtifactClass  ArtifactClass `json:"artifactClass"`
	ContentHash    string        `json:"contentHash"`
	ByteSize       int64         `json:"byteSize"`
	MediaType      string        `json:"mediaType"`
	CleanupOwnerID string        `json:"cleanupOwnerId"`
}

type deleteGovernedObjectRequest struct {
	Authority StorageScope `json:"authority"`
}

type governedObjectDeletionResponse struct {
	SchemaVersion              int          `json:"schemaVersion"`
	ObjectID                   string       `json:"objectId"`
	TenantID                   string       `json:"tenantId"`
	Authority                  StorageScope `json:"authority"`
	AccessState                string       `json:"accessState"`
	AccessRevokedAt            string       `json:"accessRevokedAt"`
	PhysicalRemovalState       string       `json:"physicalRemovalState"`
	PhysicalRemovalNotBefore   string       `json:"physicalRemovalNotBefore,omitempty"`
	PhysicalRemovalConfirmedAt string       `json:"physicalRemovalConfirmedAt,omitempty"`
	RetryAfterSeconds          int64        `json:"retryAfterSeconds,omitempty"`
	Error                      string       `json:"error,omitempty"`
}

type governedReservationCleanupResponse struct {
	SchemaVersion            int          `json:"schemaVersion"`
	TenantID                 string       `json:"tenantId"`
	Authority                StorageScope `json:"authority"`
	AccessState              string       `json:"accessState"`
	PhysicalRemovalState     string       `json:"physicalRemovalState"`
	PendingReservationCount  int          `json:"pendingReservationCount"`
	PhysicalRemovalNotBefore string       `json:"physicalRemovalNotBefore,omitempty"`
	RetryAfterSeconds        int64        `json:"retryAfterSeconds,omitempty"`
	Error                    string       `json:"error,omitempty"`
}

type governedOrdinaryCleanupResponse struct {
	SchemaVersion            int          `json:"schemaVersion"`
	TenantID                 string       `json:"tenantId"`
	Authority                StorageScope `json:"authority"`
	CleanupOwnerID           string       `json:"cleanupOwnerId,omitempty"`
	AccessState              string       `json:"accessState"`
	PhysicalRemovalState     string       `json:"physicalRemovalState"`
	PendingObjectCount       int          `json:"pendingObjectCount"`
	PhysicalRemovalNotBefore string       `json:"physicalRemovalNotBefore,omitempty"`
	RetryAfterSeconds        int64        `json:"retryAfterSeconds,omitempty"`
	Error                    string       `json:"error,omitempty"`
}

type governedOutputRequest struct {
	ExecutionID   string        `json:"executionId"`
	ArtifactClass ArtifactClass `json:"artifactClass"`
	ContentHash   string        `json:"contentHash"`
	ByteSize      int64         `json:"byteSize"`
	MediaType     string        `json:"mediaType"`
}

type commitGovernedOutputRequest struct {
	ExecutionID string `json:"executionId"`
	ObjectID    string `json:"objectId"`
}

func (s *server) handleIssueGovernedGrant(w http.ResponseWriter, r *http.Request) {
	tenant, account, ok := s.authorizeGoverned(w, r, OperationIssueGrant)
	if !ok {
		return
	}
	var request issueGovernedGrantRequest
	if !decodeGovernedJSON(w, r, &request) {
		return
	}
	expiresAt, err := time.Parse(time.RFC3339Nano, request.ExpiresAt)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody("invalid grant expiry"))
		return
	}
	now := time.Now()
	grantInput := IssueGrantInput{
		Tenant: tenant, Scope: request.Authority, ExecutionID: request.ExecutionID,
		ReadableObjectIDs:       request.ReadableObjectIDs,
		WritableArtifactClasses: request.WritableArtifactClasses,
		IssuerServiceAccount:    account, ExpiresAt: expiresAt, CleanupOwnerID: request.CleanupOwnerID,
	}
	if err := validateIssueGrantInput(grantInput, now); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody("invalid execution grant"))
		return
	}
	grant, err := s.governedStore.IssueGrant(r.Context(), grantInput)
	if err != nil {
		s.writeGovernedError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, grant)
}

func (s *server) handleGovernedObjectTicket(w http.ResponseWriter, r *http.Request) {
	tenant, _, ok := s.authorizeGoverned(w, r, OperationConsumeGrant)
	if !ok {
		return
	}
	var request executionRequest
	if !decodeGovernedJSON(w, r, &request) {
		return
	}
	// A read ticket mutates nothing, so it takes no object mutation lock. Its
	// single authorization query is the linearization point: it observes either
	// the active row or the committed tombstone, and activation and tombstoning
	// are each one atomic UPDATE. A ticket authorized just before a tombstone is
	// the same as one issued just before deletion, which the deletion contract
	// already tolerates (GET tickets stay bearer-readable until expiry or
	// physical removal, and physical removal never depends on GET tickets).
	object, err := s.governedStore.ConsumeGrantObject(r.Context(), ConsumeGrantObjectInput{
		Tenant: tenant, GrantID: r.PathValue("grantId"), ExecutionID: request.ExecutionID,
		ObjectID: r.PathValue("objectId"), Now: time.Now(),
	})
	if err != nil {
		s.writeGovernedError(w, err)
		return
	}
	ticket, err := s.presigner.Presign(r.Context(), "get", object.Bucket, object.Key, object.MediaType,
		time.Duration(s.policy.DefaultTTLSeconds.Get)*time.Second)
	if err != nil {
		s.log.Error("governed object presign failed", "tenant", tenant, "object_id", object.ID, "err", err.Error())
		writeJSON(w, http.StatusInternalServerError, errorBody("object transfer unavailable"))
		return
	}
	writeJSON(w, http.StatusOK, governedTicketResponse{
		Object: objectRefV2(object),
		Transfer: governedTransfer{URL: ticket.URL, Method: ticket.Method,
			ExpiresAt: ticket.ExpiresAt.UTC().Format(time.RFC3339)},
	})
}

func (s *server) handleRegisterGovernedObject(w http.ResponseWriter, r *http.Request) {
	tenant, _, ok := s.authorizeGoverned(w, r, OperationRegisterObject)
	if !ok {
		return
	}
	var request registerGovernedObjectRequest
	if !decodeGovernedJSON(w, r, &request) {
		return
	}
	bucket, ok := s.tenantBucket(w, tenant)
	if !ok {
		return
	}
	registration := RegisterActiveObjectInput{
		Tenant: tenant, Scope: request.Authority, ArtifactClass: request.ArtifactClass,
		ContentHash: request.ContentHash, ByteSize: request.ByteSize, MediaType: request.MediaType,
		Bucket: bucket, Key: request.PhysicalKey,
	}
	if err := validateObjectRegistration(registration); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody("invalid object declaration"))
		return
	}
	unlock, err := s.governedStore.AcquireRegisterExistingLock(r.Context(), tenant, bucket, request.PhysicalKey)
	if err != nil {
		s.writeGovernedError(w, err)
		return
	}
	defer unlock()
	if !s.authorizeBucketRegistration(w, r, tenant, bucket) {
		return
	}
	if err := s.presigner.VerifyObject(r.Context(), bucket, request.PhysicalKey,
		request.ContentHash, request.ByteSize); err != nil {
		if errors.Is(err, ErrObjectMismatch) {
			s.log.Info("governed object verification rejected", "tenant", tenant, "err", err.Error())
			writeJSON(w, http.StatusUnprocessableEntity, errorBody("stored object does not match declaration"))
		} else {
			s.log.Error("governed object verification unavailable", "tenant", tenant, "err", err.Error())
			writeJSON(w, http.StatusInternalServerError, errorBody("object verification unavailable"))
		}
		return
	}
	object, err := s.governedStore.RegisterActiveObject(r.Context(), registration)
	if err != nil {
		s.writeGovernedError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, objectRefV2(object))
}

func (s *server) handleReserveGovernedObject(w http.ResponseWriter, r *http.Request) {
	tenant, _, ok := s.authorizeGoverned(w, r, OperationRegisterObject)
	if !ok {
		return
	}
	var request reserveGovernedObjectRequest
	if !decodeGovernedJSON(w, r, &request) {
		return
	}
	bucket, ok := s.tenantBucket(w, tenant)
	if !ok {
		return
	}
	reservationID, err := newUUIDv4()
	if err != nil {
		s.writeGovernedError(w, err)
		return
	}
	registration := RegisterActiveObjectInput{
		Tenant: tenant, Scope: request.Authority, ArtifactClass: request.ArtifactClass,
		ContentHash: request.ContentHash, ByteSize: request.ByteSize, MediaType: request.MediaType,
		Bucket: bucket, Key: governedReservationKey(request.Authority, request.ArtifactClass, reservationID),
		CleanupOwnerID: request.CleanupOwnerID,
	}
	if !uuidPattern.MatchString(request.CleanupOwnerID) {
		writeJSON(w, http.StatusBadRequest, errorBody("invalid cleanup owner ID"))
		return
	}
	if err := validateObjectRegistration(registration); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody("invalid object declaration"))
		return
	}
	if !s.authorizeBucketRegistration(w, r, tenant, bucket) {
		return
	}
	issuer, ticket := s.uploadTicketIssuer(r.Context())
	object, err := s.governedStore.ReserveObject(r.Context(), registration, issuer)
	if errors.Is(err, errUploadTicketUnavailable) {
		s.log.Error("governed object upload presign failed", "tenant", tenant, "err", err.Error())
		writeJSON(w, http.StatusInternalServerError, errorBody("object transfer unavailable"))
		return
	}
	if err != nil {
		s.writeGovernedError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, governedTicketResponse{
		Object: objectRefV2(object),
		Transfer: governedTransfer{URL: ticket.URL, Method: ticket.Method,
			ExpiresAt: ticket.ExpiresAt.UTC().Format(time.RFC3339), Headers: ticket.Headers},
	})
}

func (s *server) handleCommitGovernedObject(w http.ResponseWriter, r *http.Request) {
	tenant, _, ok := s.authorizeGoverned(w, r, OperationRegisterObject)
	if !ok {
		return
	}
	unlock, err := s.governedStore.AcquireObjectLock(r.Context(), tenant, r.PathValue("objectId"))
	if err != nil {
		s.writeGovernedError(w, err)
		return
	}
	defer unlock()
	object, err := s.governedStore.AuthorizeObjectReservation(r.Context(), tenant, r.PathValue("objectId"))
	if err != nil {
		s.writeGovernedError(w, err)
		return
	}
	if object.State == ObjectStateActive {
		if err := s.presigner.VerifyObject(r.Context(), object.Bucket, object.Key,
			object.ContentHash, object.ByteSize); err != nil {
			s.log.Error("active governed object failed verification", "tenant", tenant,
				"object_id", object.ID, "err", err.Error())
			writeJSON(w, http.StatusInternalServerError, errorBody("active object verification failed"))
			return
		}
		writeJSON(w, http.StatusCreated, objectRefV2(object))
		return
	}
	finalKey := governedObjectKey(object.Scope, object.ArtifactClass, object.ContentHash, object.ID)
	if err := s.presigner.PromoteObject(r.Context(), object.Bucket, object.Key, finalKey,
		object.ContentHash, object.ByteSize); err != nil {
		if errors.Is(err, ErrObjectMismatch) {
			s.log.Info("governed object verification rejected", "tenant", tenant,
				"object_id", object.ID, "err", err.Error())
			s.deleteRejectedReservation(r.Context(), object)
			writeJSON(w, http.StatusUnprocessableEntity, errorBody("stored object does not match declaration"))
		} else {
			s.log.Error("governed object promotion unavailable", "tenant", tenant,
				"object_id", object.ID, "err", err.Error())
			writeJSON(w, http.StatusInternalServerError, errorBody("object promotion unavailable"))
		}
		return
	}
	stagingKey := object.Key
	object, err = s.governedStore.ActivateObject(r.Context(), tenant, object.ID, object.Bucket, finalKey)
	if err != nil {
		s.writeGovernedError(w, err)
		return
	}
	s.deleteStagedUpload(r.Context(), object.Tenant, object.ID, object.Bucket, stagingKey)
	writeJSON(w, http.StatusCreated, objectRefV2(object))
}

func (s *server) handleReserveGovernedOutput(w http.ResponseWriter, r *http.Request) {
	tenant, _, ok := s.authorizeGoverned(w, r, OperationRegisterOutput)
	if !ok {
		return
	}
	var request governedOutputRequest
	if !decodeGovernedJSON(w, r, &request) {
		return
	}
	if !validGrantOutputClass(request.ArtifactClass) ||
		!contentHashPattern.MatchString(request.ContentHash) || request.ByteSize < 0 ||
		request.MediaType == "" || len(request.MediaType) > 255 {
		writeJSON(w, http.StatusBadRequest, errorBody("invalid output declaration"))
		return
	}
	bucket, ok := s.tenantBucket(w, tenant)
	if !ok {
		return
	}
	if !s.authorizeBucketRegistration(w, r, tenant, bucket) {
		return
	}
	issuer, ticket := s.uploadTicketIssuer(r.Context())
	object, err := s.governedStore.ReserveGrantOutput(r.Context(), ReserveGrantOutputInput{
		Tenant: tenant, GrantID: r.PathValue("grantId"), ExecutionID: request.ExecutionID,
		ArtifactClass: request.ArtifactClass, ContentHash: request.ContentHash,
		ByteSize: request.ByteSize, MediaType: request.MediaType, Bucket: bucket,
		Now: time.Now(),
	}, issuer)
	if errors.Is(err, errUploadTicketUnavailable) {
		s.log.Error("governed output presign failed", "tenant", tenant,
			"grant_id", r.PathValue("grantId"), "err", err.Error())
		writeJSON(w, http.StatusInternalServerError, errorBody("output transfer unavailable"))
		return
	}
	if err != nil {
		s.writeGovernedError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, governedTicketResponse{
		Object: objectRefV2(object),
		Transfer: governedTransfer{URL: ticket.URL, Method: ticket.Method,
			ExpiresAt: ticket.ExpiresAt.UTC().Format(time.RFC3339), Headers: ticket.Headers},
	})
}

func (s *server) handleCommitGovernedOutput(w http.ResponseWriter, r *http.Request) {
	tenant, _, ok := s.authorizeGoverned(w, r, OperationRegisterOutput)
	if !ok {
		return
	}
	var request commitGovernedOutputRequest
	if !decodeGovernedJSON(w, r, &request) {
		return
	}
	reservation := GrantOutputReservationInput{
		Tenant: tenant, GrantID: r.PathValue("grantId"), ExecutionID: request.ExecutionID,
		ObjectID: request.ObjectID, Now: time.Now(),
	}
	unlock, err := s.governedStore.AcquireObjectLock(r.Context(), tenant, request.ObjectID)
	if err != nil {
		s.writeGovernedError(w, err)
		return
	}
	defer unlock()
	object, err := s.governedStore.AuthorizeGrantOutputReservation(r.Context(), reservation)
	if err != nil {
		s.writeGovernedError(w, err)
		return
	}
	if object.State == ObjectStateActive {
		if err := s.presigner.VerifyObject(r.Context(), object.Bucket, object.Key,
			object.ContentHash, object.ByteSize); err != nil {
			s.log.Error("active governed output failed verification", "tenant", tenant,
				"grant_id", r.PathValue("grantId"), "object_id", object.ID, "err", err.Error())
			writeJSON(w, http.StatusInternalServerError, errorBody("active output verification failed"))
			return
		}
		writeJSON(w, http.StatusCreated, objectRefV2(object))
		return
	}
	finalKey := governedObjectKey(object.Scope, object.ArtifactClass, object.ContentHash, object.ID)
	if err := s.presigner.PromoteObject(r.Context(), object.Bucket, object.Key, finalKey,
		object.ContentHash, object.ByteSize); err != nil {
		if errors.Is(err, ErrObjectMismatch) {
			s.log.Info("governed output verification rejected", "tenant", tenant,
				"grant_id", r.PathValue("grantId"), "object_id", object.ID, "err", err.Error())
			s.deleteRejectedReservation(r.Context(), object)
			writeJSON(w, http.StatusUnprocessableEntity, errorBody("stored output does not match declaration"))
		} else {
			s.log.Error("governed output promotion unavailable", "tenant", tenant,
				"grant_id", r.PathValue("grantId"), "object_id", object.ID, "err", err.Error())
			writeJSON(w, http.StatusInternalServerError, errorBody("output promotion unavailable"))
		}
		return
	}
	stagingKey := object.Key
	object, err = s.governedStore.ActivateGrantOutput(r.Context(), reservation, object.Bucket, finalKey)
	if err != nil {
		s.writeGovernedError(w, err)
		return
	}
	s.deleteStagedUpload(r.Context(), object.Tenant, object.ID, object.Bucket, stagingKey)
	writeJSON(w, http.StatusCreated, objectRefV2(object))
}

func (s *server) handleDeleteGovernedObject(w http.ResponseWriter, r *http.Request) {
	tenant, _, ok := s.authorizeGoverned(w, r, OperationDeleteObject)
	if !ok {
		return
	}
	var request deleteGovernedObjectRequest
	if !decodeGovernedJSON(w, r, &request) {
		return
	}
	if err := request.Authority.Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody("invalid object authority"))
		return
	}
	objectID := r.PathValue("objectId")
	unlock, err := s.governedStore.AcquireObjectLock(r.Context(), tenant, objectID)
	if err != nil {
		s.writeGovernedError(w, err)
		return
	}
	now := s.currentTime()
	deletion, err := s.governedStore.TombstoneObject(r.Context(), tenant, request.Authority, objectID, now)
	// The tombstone is the serialization boundary: once durable, commit and ticket
	// paths reject the object. Release its database connection before cleanup,
	// which takes physical-key locks and may process a large legacy prefix.
	unlock()
	if err != nil {
		s.writeGovernedError(w, err)
		return
	}
	if deletion.PhysicalRemovalConfirmedAt != nil {
		s.writeObjectDeletion(w, http.StatusOK, deletion, time.Time{}, "")
		return
	}
	notBefore := deletion.AccessRevokedAt.Add(time.Duration(s.policy.DefaultTTLSeconds.Put) * time.Second)
	if now.Before(notBefore) {
		s.writeObjectDeletion(w, http.StatusAccepted, deletion, notBefore, "")
		return
	}
	unlockRegistrations, err := s.governedStore.AcquireRegistrationBarrier(r.Context(), tenant,
		deletion.Object.Bucket)
	if err != nil {
		s.writeGovernedError(w, err)
		return
	}
	defer unlockRegistrations()
	exclusive, err := s.governedStore.IsBucketExclusiveToTenant(r.Context(), tenant,
		deletion.Object.Bucket)
	if err != nil {
		s.writeGovernedError(w, err)
		return
	}
	if !exclusive {
		s.log.Error("governed object physical removal blocked by historical bucket sharing",
			"tenant", tenant, "object_id", objectID, "bucket", deletion.Object.Bucket)
		s.writeObjectDeletion(w, http.StatusServiceUnavailable, deletion, notBefore,
			"physical removal blocked by non-exclusive bucket history; retry after storage remediation")
		return
	}
	keys := uniqueObjectKeys(
		deletion.Object.Key,
		deletion.StagingKey,
		governedObjectKey(deletion.Object.Scope, deletion.Object.ArtifactClass,
			deletion.Object.ContentHash, deletion.Object.ID),
	)
	for _, key := range keys {
		if err := s.objectCleaner.DeleteAndConfirmAbsent(r.Context(), deletion.Object.Bucket, key); err != nil {
			s.log.Error("governed object physical removal failed", "tenant", tenant,
				"object_id", objectID, "err", err.Error())
			s.writeObjectDeletion(w, http.StatusServiceUnavailable, deletion, notBefore,
				"physical removal unavailable; retry deletion")
			return
		}
	}
	if deletion.StagingKey == "" {
		if deletion.LegacyStagingSweptAt != nil {
			settledAt := deletion.LegacyStagingSweptAt.Add(
				time.Duration(s.policy.DefaultTTLSeconds.Put) * time.Second)
			if now.Before(settledAt) {
				s.writeObjectDeletion(w, http.StatusAccepted, deletion, settledAt, "")
				return
			}
		}
		prefix := governedStagingPrefix(deletion.Object.Scope, deletion.Object.ArtifactClass)
		if err := s.cleanUnreferencedStaging(r.Context(), tenant, deletion.Object.Bucket, prefix); err != nil {
			s.log.Error("governed legacy staging cleanup failed", "tenant", tenant,
				"object_id", objectID, "err", err.Error())
			s.writeObjectDeletion(w, http.StatusServiceUnavailable, deletion, notBefore,
				"legacy staging removal unavailable; retry deletion")
			return
		}
		if deletion.LegacyStagingSweptAt == nil {
			deletion, err = s.governedStore.RecordLegacyStagingSweep(r.Context(), tenant,
				request.Authority, objectID, s.currentTime())
			if err != nil {
				s.writeGovernedError(w, err)
				return
			}
			settledAt := deletion.LegacyStagingSweptAt.Add(
				time.Duration(s.policy.DefaultTTLSeconds.Put) * time.Second)
			s.writeObjectDeletion(w, http.StatusAccepted, deletion, settledAt, "")
			return
		}
		// A prefix listing can find bytes to remove, but cannot prove that an
		// unknown legacy locator is absent. Keep access revoked and cleanup
		// retryable instead of reporting physical completion from elapsed time.
		nextSweep := s.currentTime().Add(time.Duration(s.policy.DefaultTTLSeconds.Put) * time.Second)
		s.writeObjectDeletion(w, http.StatusServiceUnavailable, deletion, nextSweep,
			"legacy staging locator is unknown; physical removal remains unconfirmed")
		return
	}
	deletion, err = s.governedStore.ConfirmObjectRemoval(r.Context(), tenant, request.Authority, objectID,
		s.currentTime())
	if err != nil {
		s.writeGovernedError(w, err)
		return
	}
	s.writeObjectDeletion(w, http.StatusOK, deletion, time.Time{}, "")
}

func (s *server) handleCleanupGovernedReservations(w http.ResponseWriter, r *http.Request) {
	tenant, _, ok := s.authorizeGoverned(w, r, OperationDeleteObject)
	if !ok {
		return
	}
	var request deleteGovernedObjectRequest
	if !decodeGovernedJSON(w, r, &request) {
		return
	}
	if err := request.Authority.Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody("invalid reservation authority"))
		return
	}
	currentBucket, ok := s.tenantBucket(w, tenant)
	if !ok {
		return
	}
	buckets, err := s.governedStore.ReservationCleanupBuckets(r.Context(), tenant, request.Authority)
	if err != nil {
		s.writeGovernedError(w, err)
		return
	}
	if !slices.Contains(buckets, currentBucket) {
		buckets = append(buckets, currentBucket)
	}
	slices.Sort(buckets)
	buckets = slices.Compact(buckets)
	unlockRegistrations, err := s.governedStore.AcquireRegistrationBarriers(r.Context(), tenant, buckets)
	if err != nil {
		s.writeGovernedError(w, err)
		return
	}
	defer unlockRegistrations()
	now := s.currentTime()
	if err := s.governedStore.TombstoneReservations(r.Context(), tenant, request.Authority, now); err != nil {
		s.writeGovernedError(w, err)
		return
	}
	deletions, err := s.governedStore.PendingReservationDeletions(r.Context(), tenant,
		request.Authority, maxReservationCleanupBatch)
	if err != nil {
		s.writeGovernedError(w, err)
		return
	}
	putTTL := time.Duration(s.policy.DefaultTTLSeconds.Put) * time.Second
	cleanupFailed := false
	exclusiveBuckets := make(map[string]bool, len(buckets))
	for _, deletion := range deletions {
		if now.Before(deletion.AccessRevokedAt.Add(putTTL)) {
			continue
		}
		exclusive, checked := exclusiveBuckets[deletion.Object.Bucket]
		if !checked {
			exclusive, err = s.governedStore.IsBucketExclusiveToTenant(r.Context(), tenant,
				deletion.Object.Bucket)
			if err != nil {
				s.log.Error("reservation cleanup bucket ownership check failed", "tenant", tenant,
					"bucket", deletion.Object.Bucket, "err", err.Error())
				cleanupFailed = true
				s.recordReservationCleanupAttempt(r.Context(), tenant, request.Authority,
					deletion.Object.ID)
				continue
			}
			exclusiveBuckets[deletion.Object.Bucket] = exclusive
		}
		if !exclusive {
			s.log.Error("reservation cleanup blocked by historical bucket sharing", "tenant", tenant,
				"bucket", deletion.Object.Bucket)
			cleanupFailed = true
			s.recordReservationCleanupAttempt(r.Context(), tenant, request.Authority, deletion.Object.ID)
			continue
		}
		if deletion.StagingKey == "" {
			s.log.Error("reservation cleanup has no exact staging locator", "tenant", tenant,
				"object_id", deletion.Object.ID)
			cleanupFailed = true
			s.recordReservationCleanupAttempt(r.Context(), tenant, request.Authority, deletion.Object.ID)
			continue
		}
		keys := uniqueObjectKeys(
			deletion.Object.Key,
			deletion.StagingKey,
			governedObjectKey(deletion.Object.Scope, deletion.Object.ArtifactClass,
				deletion.Object.ContentHash, deletion.Object.ID),
		)
		removed := true
		for _, key := range keys {
			if err := s.objectCleaner.DeleteAndConfirmAbsent(r.Context(), deletion.Object.Bucket, key); err != nil {
				s.log.Error("reservation physical removal failed", "tenant", tenant,
					"object_id", deletion.Object.ID, "err", err.Error())
				cleanupFailed = true
				removed = false
				break
			}
		}
		if !removed {
			s.recordReservationCleanupAttempt(r.Context(), tenant, request.Authority, deletion.Object.ID)
			continue
		}
		if _, err := s.governedStore.ConfirmObjectRemoval(r.Context(), tenant, request.Authority,
			deletion.Object.ID, s.currentTime()); err != nil {
			s.log.Error("reservation removal confirmation failed", "tenant", tenant,
				"object_id", deletion.Object.ID, "err", err.Error())
			cleanupFailed = true
			s.recordReservationCleanupAttempt(r.Context(), tenant, request.Authority, deletion.Object.ID)
		}
	}
	pending, oldestRevocation, err := s.governedStore.ReservationCleanupStatus(r.Context(), tenant,
		request.Authority)
	if err != nil {
		s.writeGovernedError(w, err)
		return
	}
	if pending == 0 {
		s.writeReservationCleanup(w, http.StatusOK, tenant, request.Authority, 0, time.Time{}, "")
		return
	}
	nextAttempt := now.Add(time.Second)
	if oldestRevocation != nil && oldestRevocation.Add(putTTL).After(nextAttempt) {
		nextAttempt = oldestRevocation.Add(putTTL)
	}
	if cleanupFailed {
		s.writeReservationCleanup(w, http.StatusServiceUnavailable, tenant, request.Authority,
			pending, nextAttempt, "reservation physical removal unavailable; retry cleanup")
		return
	}
	s.writeReservationCleanup(w, http.StatusAccepted, tenant, request.Authority,
		pending, nextAttempt, "")
}

func (s *server) handleCleanupGovernedOwner(w http.ResponseWriter, r *http.Request) {
	tenant, _, ok := s.authorizeGoverned(w, r, OperationDeleteObject)
	if !ok {
		return
	}
	ownerID := r.PathValue("cleanupOwnerId")
	if !uuidPattern.MatchString(ownerID) {
		writeJSON(w, http.StatusBadRequest, errorBody("invalid cleanup owner ID"))
		return
	}
	var request deleteGovernedObjectRequest
	if !decodeGovernedJSON(w, r, &request) {
		return
	}
	if err := request.Authority.Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody("invalid object authority"))
		return
	}
	s.cleanupGovernedOrdinaryObjects(w, r, tenant, request.Authority, ownerID, false)
}

func (s *server) handleCleanupGovernedWorkspace(w http.ResponseWriter, r *http.Request) {
	tenant, _, ok := s.authorizeGoverned(w, r, OperationDeleteObject)
	if !ok {
		return
	}
	var request deleteGovernedObjectRequest
	if !decodeGovernedJSON(w, r, &request) {
		return
	}
	if err := request.Authority.Validate(); err != nil || request.Authority.Kind != StorageScopeWorkspace {
		writeJSON(w, http.StatusBadRequest, errorBody("workspace authority is required"))
		return
	}
	s.cleanupGovernedOrdinaryObjects(w, r, tenant, request.Authority, "", true)
}

func (s *server) cleanupGovernedOrdinaryObjects(
	w http.ResponseWriter,
	r *http.Request,
	tenant string,
	authority StorageScope,
	ownerID string,
	allOwners bool,
) {
	currentBucket, ok := s.tenantBucket(w, tenant)
	if !ok {
		return
	}
	buckets, err := s.governedStore.OrdinaryCleanupBuckets(r.Context(), tenant, authority,
		ownerID, allOwners)
	if err != nil {
		s.writeGovernedError(w, err)
		return
	}
	if !slices.Contains(buckets, currentBucket) {
		buckets = append(buckets, currentBucket)
	}
	slices.Sort(buckets)
	buckets = slices.Compact(buckets)
	unlockRegistrations, err := s.governedStore.AcquireRegistrationBarriers(r.Context(), tenant, buckets)
	if err != nil {
		s.writeGovernedError(w, err)
		return
	}
	defer unlockRegistrations()
	now := s.currentTime()
	if err := s.governedStore.TombstoneOrdinaryObjects(r.Context(), tenant, authority,
		ownerID, allOwners, now); err != nil {
		s.writeGovernedError(w, err)
		return
	}
	deletions, err := s.governedStore.PendingOrdinaryObjectDeletions(r.Context(), tenant,
		authority, ownerID, allOwners, maxReservationCleanupBatch)
	if err != nil {
		s.writeGovernedError(w, err)
		return
	}
	putTTL := time.Duration(s.policy.DefaultTTLSeconds.Put) * time.Second
	cleanupFailed := false
	exclusiveBuckets := make(map[string]bool, len(buckets))
	for _, deletion := range deletions {
		if now.Before(deletion.AccessRevokedAt.Add(putTTL)) {
			continue
		}
		exclusive, checked := exclusiveBuckets[deletion.Object.Bucket]
		if !checked {
			exclusive, err = s.governedStore.IsBucketExclusiveToTenant(r.Context(), tenant,
				deletion.Object.Bucket)
			if err != nil {
				s.log.Error("ordinary cleanup bucket ownership check failed", "tenant", tenant,
					"bucket", deletion.Object.Bucket, "err", err.Error())
				cleanupFailed = true
				s.recordReservationCleanupAttempt(r.Context(), tenant, authority, deletion.Object.ID)
				continue
			}
			exclusiveBuckets[deletion.Object.Bucket] = exclusive
		}
		if !exclusive || deletion.StagingKey == "" {
			cleanupFailed = true
			s.recordReservationCleanupAttempt(r.Context(), tenant, authority, deletion.Object.ID)
			continue
		}
		keys := uniqueObjectKeys(deletion.Object.Key, deletion.StagingKey,
			governedObjectKey(deletion.Object.Scope, deletion.Object.ArtifactClass,
				deletion.Object.ContentHash, deletion.Object.ID))
		removed := true
		for _, key := range keys {
			if err := s.objectCleaner.DeleteAndConfirmAbsent(r.Context(), deletion.Object.Bucket, key); err != nil {
				s.log.Error("ordinary object physical removal failed", "tenant", tenant,
					"object_id", deletion.Object.ID, "err", err.Error())
				cleanupFailed = true
				removed = false
				break
			}
		}
		if !removed {
			s.recordReservationCleanupAttempt(r.Context(), tenant, authority, deletion.Object.ID)
			continue
		}
		if _, err := s.governedStore.ConfirmObjectRemoval(r.Context(), tenant, authority,
			deletion.Object.ID, s.currentTime()); err != nil {
			cleanupFailed = true
			s.recordReservationCleanupAttempt(r.Context(), tenant, authority, deletion.Object.ID)
		}
	}
	pending, oldestRevocation, err := s.governedStore.OrdinaryCleanupStatus(r.Context(), tenant,
		authority, ownerID, allOwners)
	if err != nil {
		s.writeGovernedError(w, err)
		return
	}
	if pending == 0 {
		s.writeOrdinaryCleanup(w, http.StatusOK, tenant, authority, ownerID, 0, time.Time{}, "")
		return
	}
	nextAttempt := now.Add(time.Second)
	if oldestRevocation != nil && oldestRevocation.Add(putTTL).After(nextAttempt) {
		nextAttempt = oldestRevocation.Add(putTTL)
	}
	if cleanupFailed {
		s.writeOrdinaryCleanup(w, http.StatusServiceUnavailable, tenant, authority, ownerID,
			pending, nextAttempt, "object physical removal unavailable; retry cleanup")
		return
	}
	s.writeOrdinaryCleanup(w, http.StatusAccepted, tenant, authority, ownerID,
		pending, nextAttempt, "")
}

func (s *server) recordReservationCleanupAttempt(
	ctx context.Context,
	tenant string,
	scope StorageScope,
	objectID string,
) {
	if err := s.governedStore.RecordReservationCleanupAttempt(ctx, tenant, scope, objectID,
		s.currentTime()); err != nil {
		s.log.Error("record reservation cleanup attempt", "tenant", tenant,
			"object_id", objectID, "err", err.Error())
	}
}

func (s *server) cleanUnreferencedStaging(ctx context.Context, tenant, bucket, prefix string) error {
	if err := s.objectCleaner.WalkKeys(ctx, bucket, prefix, func(key string) error {
		return s.cleanUnreferencedStagingKey(ctx, tenant, bucket, key)
	}); err != nil {
		return err
	}
	referenced, err := s.governedStore.HasLiveObjectKeyUnderPrefix(ctx, tenant, bucket, prefix)
	if err != nil {
		return err
	}
	if referenced {
		return fmt.Errorf("staging prefix remains referenced by a live governed object")
	}
	return nil
}

func (s *server) cleanUnreferencedStagingKey(ctx context.Context, tenant, bucket, key string) error {
	referenced, err := s.governedStore.IsLiveObjectKeyReferenced(ctx, tenant, bucket, key)
	if err != nil {
		return err
	}
	if referenced {
		// Keep walking so a referenced key early in the listing cannot strand
		// later orphaned uploads. The prefix-wide live-reference check after the
		// walk keeps the deletion pending without deleting this key.
		return nil
	}
	return s.objectCleaner.DeleteAndConfirmAbsent(ctx, bucket, key)
}

func (s *server) authorizeBucketRegistration(
	w http.ResponseWriter,
	r *http.Request,
	tenant, bucket string,
) bool {
	if err := s.governedStore.VerifyBucketRegistration(r.Context(), tenant, bucket); err != nil {
		s.writeGovernedError(w, err)
		return false
	}
	return true
}

var errUploadTicketUnavailable = errors.New("upload ticket unavailable")

// uploadTicketIssuer signs the reservation's PUT ticket inside the reservation
// transaction (see ReservationIssuer). Signing is local SigV4 computation, so
// the transaction stays short and performs no network I/O.
func (s *server) uploadTicketIssuer(ctx context.Context) (ReservationIssuer, *PresignResult) {
	ticket := &PresignResult{}
	return func(object GovernedObject) error {
		signed, err := s.presigner.Presign(ctx, "put", object.Bucket, object.Key, object.MediaType,
			time.Duration(s.policy.DefaultTTLSeconds.Put)*time.Second)
		if err != nil {
			return fmt.Errorf("%w: object %s: %v", errUploadTicketUnavailable, object.ID, err)
		}
		*ticket = *signed
		return nil
	}, ticket
}

func (s *server) currentTime() time.Time {
	if s.now != nil {
		return s.now().UTC()
	}
	return time.Now().UTC()
}

func (s *server) writeObjectDeletion(
	w http.ResponseWriter,
	status int,
	deletion GovernedObjectDeletion,
	notBefore time.Time,
	message string,
) {
	response := governedObjectDeletionResponse{
		SchemaVersion: 1, ObjectID: deletion.Object.ID, TenantID: deletion.Object.Tenant,
		Authority: deletion.Object.Scope, AccessState: "revoked",
		AccessRevokedAt:      deletion.AccessRevokedAt.UTC().Format(time.RFC3339Nano),
		PhysicalRemovalState: "pending", Error: message,
	}
	if !notBefore.IsZero() {
		response.PhysicalRemovalNotBefore = notBefore.UTC().Format(time.RFC3339Nano)
		retryAfter := ceilSeconds(notBefore.Sub(s.currentTime()))
		if retryAfter > 0 {
			response.RetryAfterSeconds = retryAfter
		}
	}
	if status == http.StatusServiceUnavailable && response.RetryAfterSeconds == 0 {
		response.RetryAfterSeconds = 1
	}
	if deletion.PhysicalRemovalConfirmedAt != nil {
		response.PhysicalRemovalState = "confirmed"
		response.PhysicalRemovalConfirmedAt = deletion.PhysicalRemovalConfirmedAt.UTC().Format(time.RFC3339Nano)
		response.PhysicalRemovalNotBefore = ""
		response.RetryAfterSeconds = 0
	}
	if response.RetryAfterSeconds > 0 {
		w.Header().Set("Retry-After", fmt.Sprintf("%d", response.RetryAfterSeconds))
	}
	writeJSON(w, status, response)
}

func (s *server) writeReservationCleanup(
	w http.ResponseWriter,
	status int,
	tenant string,
	authority StorageScope,
	pending int,
	notBefore time.Time,
	message string,
) {
	response := governedReservationCleanupResponse{
		SchemaVersion: 1, TenantID: tenant, Authority: authority, AccessState: "revoked",
		PhysicalRemovalState: "pending", PendingReservationCount: pending, Error: message,
	}
	if pending == 0 {
		response.PhysicalRemovalState = "confirmed"
	} else if !notBefore.IsZero() {
		response.PhysicalRemovalNotBefore = notBefore.UTC().Format(time.RFC3339Nano)
		response.RetryAfterSeconds = ceilSeconds(notBefore.Sub(s.currentTime()))
		if response.RetryAfterSeconds < 1 {
			response.RetryAfterSeconds = 1
		}
	}
	if response.RetryAfterSeconds > 0 {
		w.Header().Set("Retry-After", fmt.Sprintf("%d", response.RetryAfterSeconds))
	}
	writeJSON(w, status, response)
}

func (s *server) writeOrdinaryCleanup(
	w http.ResponseWriter,
	status int,
	tenant string,
	authority StorageScope,
	ownerID string,
	pending int,
	notBefore time.Time,
	message string,
) {
	response := governedOrdinaryCleanupResponse{
		SchemaVersion: 1, TenantID: tenant, Authority: authority, CleanupOwnerID: ownerID,
		AccessState: "revoked", PhysicalRemovalState: "pending", PendingObjectCount: pending,
		Error: message,
	}
	if pending == 0 {
		response.PhysicalRemovalState = "confirmed"
	} else if !notBefore.IsZero() {
		response.PhysicalRemovalNotBefore = notBefore.UTC().Format(time.RFC3339Nano)
		response.RetryAfterSeconds = ceilSeconds(notBefore.Sub(s.currentTime()))
		if response.RetryAfterSeconds < 1 {
			response.RetryAfterSeconds = 1
		}
	}
	if response.RetryAfterSeconds > 0 {
		w.Header().Set("Retry-After", fmt.Sprintf("%d", response.RetryAfterSeconds))
	}
	writeJSON(w, status, response)
}

func ceilSeconds(duration time.Duration) int64 {
	if duration <= 0 {
		return 0
	}
	return int64((duration + time.Second - 1) / time.Second)
}

func uniqueObjectKeys(keys ...string) []string {
	seen := make(map[string]struct{}, len(keys))
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		if key == "" {
			continue
		}
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, key)
	}
	return result
}

func (s *server) deleteRejectedReservation(ctx context.Context, object GovernedObject) {
	if err := s.presigner.DeleteObject(ctx, object.Bucket, object.Key); err != nil {
		s.log.Error("delete rejected governed upload", "tenant", object.Tenant,
			"object_id", object.ID, "err", err.Error())
	}
}

func (s *server) deleteStagedUpload(ctx context.Context, tenant, objectID, bucket, key string) {
	if err := s.presigner.DeleteObject(ctx, bucket, key); err != nil {
		s.log.Error("delete staged governed upload", "tenant", tenant,
			"object_id", objectID, "err", err.Error())
	}
}

func (s *server) handleRevokeGovernedGrant(w http.ResponseWriter, r *http.Request) {
	tenant, _, ok := s.authorizeGoverned(w, r, OperationRevokeGrant)
	if !ok {
		return
	}
	if err := s.governedStore.RevokeGrant(r.Context(), tenant, r.PathValue("grantId"), time.Now()); err != nil {
		s.writeGovernedError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) authorizeGoverned(
	w http.ResponseWriter,
	r *http.Request,
	operation GovernedOperation,
) (tenant, account string, ok bool) {
	token := bearer(r.Header.Get("Authorization"))
	if token == "" {
		writeJSON(w, http.StatusUnauthorized, errorBody("missing app token"))
		return "", "", false
	}
	tenant, account, err := s.appAuth.Review(r.Context(), token)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, errorBody("invalid app token"))
		return "", "", false
	}
	if err := s.governedPolicy.Authorize(tenant, account, operation); err != nil {
		s.log.Info("governed operation denied", "tenant", tenant, "service_account", account,
			"operation", operation, "reason", err.Error())
		writeJSON(w, http.StatusForbidden, errorBody("governed operation denied"))
		return "", "", false
	}
	return tenant, account, true
}

func decodeGovernedJSON(w http.ResponseWriter, r *http.Request, output any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16*1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody("invalid request body"))
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, errorBody("request body must contain one JSON value"))
		return false
	}
	return true
}

func (s *server) tenantBucket(w http.ResponseWriter, tenant string) (string, bool) {
	policy, ok := s.policy.Namespaces[tenant]
	if !ok || policy.Bucket == "" {
		writeJSON(w, http.StatusForbidden, errorBody("tenant storage is not provisioned"))
		return "", false
	}
	return policy.Bucket, true
}

func (s *server) writeGovernedError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrGovernedDenied):
		writeJSON(w, http.StatusForbidden, errorBody("governed storage denied"))
	case errors.Is(err, ErrGovernedConflict):
		writeJSON(w, http.StatusConflict, errorBody("governed storage identity conflict"))
	case errors.Is(err, ErrRegistrationBusy):
		// Cleanup held the bucket gate past the wait bound. Nothing was written,
		// so 503 + Retry-After lets clients' transient-retry path try again.
		s.log.Warn("governed registration waited too long for bucket cleanup")
		w.Header().Set("Retry-After", "1")
		writeJSON(w, http.StatusServiceUnavailable, errorBody("governed registration temporarily unavailable"))
	default:
		s.log.Error("governed storage failed", "err", err.Error())
		writeJSON(w, http.StatusInternalServerError, errorBody("governed storage unavailable"))
	}
}

func objectRefV2(object GovernedObject) governedObjectRefV2 {
	return governedObjectRefV2{
		SchemaVersion: 2, ObjectID: object.ID, TenantID: object.Tenant, Authority: object.Scope,
		ContentHash: object.ContentHash, ByteSize: object.ByteSize, MediaType: object.MediaType,
	}
}

func registerGovernedRoutes(mux *http.ServeMux, metrics *brokerMetrics, s *server) {
	mux.Handle("POST /v2/objects/reserve", metrics.instrument(operationGovernedReserveObject,
		http.HandlerFunc(s.handleReserveGovernedObject)))
	mux.Handle("POST /v2/objects/{objectId}/commit", metrics.instrument(operationGovernedCommitObject,
		http.HandlerFunc(s.handleCommitGovernedObject)))
	mux.Handle("DELETE /v2/objects/{objectId}", metrics.instrument(operationGovernedDeleteObject,
		http.HandlerFunc(s.handleDeleteGovernedObject)))
	mux.Handle("DELETE /v2/reservations", metrics.instrument(operationGovernedCleanupReservations,
		http.HandlerFunc(s.handleCleanupGovernedReservations)))
	mux.Handle("DELETE /v2/cleanup-owners/{cleanupOwnerId}/objects", metrics.instrument(operationGovernedCleanupOwner,
		http.HandlerFunc(s.handleCleanupGovernedOwner)))
	mux.Handle("DELETE /v2/ordinary-objects", metrics.instrument(operationGovernedCleanupWorkspace,
		http.HandlerFunc(s.handleCleanupGovernedWorkspace)))
	mux.Handle("POST /v2/lake-objects/inventory", metrics.instrument(operationLakeInventory,
		http.HandlerFunc(s.handleInventoryLakeObjects)))
	mux.Handle("POST /v2/objects/register-existing", metrics.instrument(operationGovernedRegisterObject,
		http.HandlerFunc(s.handleRegisterGovernedObject)))
	mux.Handle("POST /v2/grants", metrics.instrument(operationGovernedIssueGrant,
		http.HandlerFunc(s.handleIssueGovernedGrant)))
	mux.Handle("POST /v2/grants/{grantId}/objects/{objectId}/ticket", metrics.instrument(operationGovernedObjectTicket,
		http.HandlerFunc(s.handleGovernedObjectTicket)))
	mux.Handle("POST /v2/grants/{grantId}/outputs/reserve", metrics.instrument(operationGovernedReserveOutput,
		http.HandlerFunc(s.handleReserveGovernedOutput)))
	mux.Handle("POST /v2/grants/{grantId}/outputs/commit", metrics.instrument(operationGovernedCommitOutput,
		http.HandlerFunc(s.handleCommitGovernedOutput)))
	mux.Handle("POST /v2/grants/{grantId}/revoke", metrics.instrument(operationGovernedRevokeGrant,
		http.HandlerFunc(s.handleRevokeGovernedGrant)))
}
