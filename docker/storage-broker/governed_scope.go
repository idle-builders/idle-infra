package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"time"
)

const maxGrantReadableObjects = 256

type StorageScopeKind string

const (
	StorageScopeTenant    StorageScopeKind = "tenant"
	StorageScopeWorkspace StorageScopeKind = "workspace"
)

type StorageScope struct {
	Kind        StorageScopeKind `json:"kind"`
	WorkspaceID string           `json:"workspaceId,omitempty"`
}

func TenantStorageScope() StorageScope {
	return StorageScope{Kind: StorageScopeTenant}
}

func WorkspaceStorageScope(workspaceID string) StorageScope {
	return StorageScope{Kind: StorageScopeWorkspace, WorkspaceID: workspaceID}
}

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func (s StorageScope) Validate() error {
	switch s.Kind {
	case StorageScopeTenant:
		if s.WorkspaceID != "" {
			return fmt.Errorf("tenant storage scope cannot carry a workspace")
		}
	case StorageScopeWorkspace:
		if !uuidPattern.MatchString(s.WorkspaceID) {
			return fmt.Errorf("workspace storage scope requires a UUID")
		}
	default:
		return fmt.Errorf("unknown storage scope kind %q", s.Kind)
	}
	return nil
}

func (s StorageScope) AllowsObject(objectScope StorageScope) bool {
	if s.Kind == StorageScopeTenant {
		return objectScope.Kind == StorageScopeTenant
	}
	return objectScope.Kind == StorageScopeTenant ||
		(objectScope.Kind == StorageScopeWorkspace && objectScope.WorkspaceID == s.WorkspaceID)
}

type ObjectState string

const (
	ObjectStateReserved ObjectState = "reserved"
	ObjectStateActive   ObjectState = "active"
	ObjectStateDeleted  ObjectState = "deleted"
)

type ArtifactClass string

const (
	ArtifactNormalizedSource  ArtifactClass = "normalized_source"
	ArtifactTransformationSQL ArtifactClass = "transformation_sql"
	ArtifactWorkBundle        ArtifactClass = "transformation_work_bundle"
	ArtifactCheckpoint        ArtifactClass = "transformation_checkpoint"
	ArtifactRuleBundle        ArtifactClass = "transformation_rule_bundle"
	ArtifactTrace             ArtifactClass = "transformation_trace"
	ArtifactResult            ArtifactClass = "transformation_result"
	ArtifactValidation        ArtifactClass = "transformation_validation"
	ArtifactEvidence          ArtifactClass = "transformation_evidence"
	ArtifactAnalysisManifest  ArtifactClass = "analysis_manifest"
	ArtifactAnalysisResult    ArtifactClass = "analysis_result"
	ArtifactSandboxRelation   ArtifactClass = "sandbox_relation"
	ArtifactSandboxDiagnostic ArtifactClass = "sandbox_diagnostic"
	ArtifactDuckLakeData      ArtifactClass = "ducklake_data"
)

type GovernedObject struct {
	ID            string        `json:"objectId"`
	Tenant        string        `json:"tenantId"`
	Scope         StorageScope  `json:"authority"`
	ArtifactClass ArtifactClass `json:"artifactClass"`
	ContentHash   string        `json:"contentHash"`
	ByteSize      int64         `json:"byteSize"`
	MediaType     string        `json:"mediaType"`
	State         ObjectState   `json:"-"`
	Bucket        string        `json:"-"`
	Key           string        `json:"-"`
}

type GovernedObjectDeletion struct {
	Object                        GovernedObject
	StagingKey                    string
	AccessRevokedAt               time.Time
	LegacyStagingSweptAt          *time.Time
	ReservationCleanupAttemptedAt *time.Time
	PhysicalRemovalConfirmedAt    *time.Time
}

type ExecutionGrant struct {
	SchemaVersion           int             `json:"schemaVersion"`
	ID                      string          `json:"grantId"`
	Tenant                  string          `json:"tenantId"`
	Scope                   StorageScope    `json:"authority"`
	ExecutionID             string          `json:"executionId"`
	ReadableObjectIDs       []string        `json:"readableObjectIds"`
	WritableArtifactClasses []ArtifactClass `json:"writableArtifactClasses"`
	ExpiresAt               time.Time       `json:"expiresAt"`
	CleanupOwnerID          string          `json:"-"`
}

// MarshalJSON is the grant's wire contract, whichever path built the value
// (fresh issue, idempotent replay from Postgres, or a test double).
// @lore A nil slice encodes as null, which clients typed as arrays reject: a
// read-only grant has no output classes. Clients expect an empty list rather
// than null for transformations and publications. Empty lists always encode as [].
func (grant ExecutionGrant) MarshalJSON() ([]byte, error) {
	type wireGrant ExecutionGrant
	wire := struct {
		wireGrant
		ReadableObjectIDs       []string        `json:"readableObjectIds"`
		WritableArtifactClasses []ArtifactClass `json:"writableArtifactClasses"`
		ExpiresAt               string          `json:"expiresAt"`
	}{
		wireGrant:               wireGrant(grant),
		ReadableObjectIDs:       nonNilSlice(grant.ReadableObjectIDs),
		WritableArtifactClasses: nonNilSlice(grant.WritableArtifactClasses),
		ExpiresAt:               formatGrantExpiry(grant.ExpiresAt),
	}
	return json.Marshal(wire)
}

func nonNilSlice[T any](values []T) []T {
	if values == nil {
		return []T{}
	}
	return values
}

// formatGrantExpiry renders the expiry in UTC. A millisecond-precision instant
// (what JavaScript's toISOString sends) keeps exactly three fractional digits,
// so the string round-trips byte for byte; finer instants keep full precision.
func formatGrantExpiry(expiresAt time.Time) string {
	expiresAt = expiresAt.UTC()
	if expiresAt.Nanosecond()%int(time.Millisecond) == 0 {
		return expiresAt.Format("2006-01-02T15:04:05.000Z07:00")
	}
	return expiresAt.Format(time.RFC3339Nano)
}

func validateGrantObjects(tenant string, scope StorageScope, objects []GovernedObject) error {
	if tenant == "" {
		return fmt.Errorf("grant tenant is required")
	}
	if err := scope.Validate(); err != nil {
		return err
	}
	if len(objects) == 0 || len(objects) > maxGrantReadableObjects {
		return fmt.Errorf("grant readable object count must be between 1 and %d", maxGrantReadableObjects)
	}
	for _, object := range objects {
		if object.Tenant != tenant {
			return fmt.Errorf("object tenant does not match grant tenant")
		}
		if object.State != ObjectStateActive {
			return fmt.Errorf("grant object is not active")
		}
		if err := object.Scope.Validate(); err != nil {
			return fmt.Errorf("object scope is invalid: %w", err)
		}
		if !scope.AllowsObject(object.Scope) {
			return fmt.Errorf("object scope is not readable by grant scope")
		}
	}
	return nil
}

func validateOutput(grant ExecutionGrant, artifactClass ArtifactClass) error {
	if err := grant.Scope.Validate(); err != nil {
		return err
	}
	for _, allowed := range grant.WritableArtifactClasses {
		if allowed == artifactClass {
			return nil
		}
	}
	return fmt.Errorf("artifact class is not writable by grant")
}
