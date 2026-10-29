package main

import (
	"testing"
	"time"
)

const (
	workspaceA = "11111111-1111-4111-8111-111111111111"
	workspaceB = "22222222-2222-4222-8222-222222222222"
)

func TestValidateGrantObjectsKeepsWorkspaceRowsClosed(t *testing.T) {
	grantScope := WorkspaceStorageScope(workspaceA)
	tenantObject := governedObjectForTest("acme", TenantStorageScope())
	workspaceObject := governedObjectForTest("acme", WorkspaceStorageScope(workspaceA))

	if err := validateGrantObjects("acme", grantScope, []GovernedObject{tenantObject, workspaceObject}); err != nil {
		t.Fatalf("validateGrantObjects() error = %v", err)
	}

	tests := []struct {
		name   string
		object GovernedObject
	}{
		{"other workspace", governedObjectForTest("acme", WorkspaceStorageScope(workspaceB))},
		{"other tenant", governedObjectForTest("atlas", TenantStorageScope())},
		{"inactive", func() GovernedObject {
			object := governedObjectForTest("acme", WorkspaceStorageScope(workspaceA))
			object.State = ObjectStateReserved
			return object
		}()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := validateGrantObjects("acme", grantScope, []GovernedObject{test.object}); err == nil {
				t.Fatal("validateGrantObjects() unexpectedly accepted incompatible object")
			}
		})
	}
}

func TestValidateGrantObjectsTenantScopeRejectsWorkspaceRows(t *testing.T) {
	if err := validateGrantObjects("acme", TenantStorageScope(), []GovernedObject{
		governedObjectForTest("acme", WorkspaceStorageScope(workspaceA)),
	}); err == nil {
		t.Fatal("validateGrantObjects() accepted a workspace object for tenant-public execution")
	}
}

func TestValidateGrantObjectsBoundsClosedReadSet(t *testing.T) {
	objects := make([]GovernedObject, maxGrantReadableObjects+1)
	for index := range objects {
		objects[index] = governedObjectForTest("acme", TenantStorageScope())
	}
	if err := validateGrantObjects("acme", TenantStorageScope(), objects); err == nil {
		t.Fatal("validateGrantObjects() accepted more than the maximum readable objects")
	}
}

func TestValidateOutputInheritsGrantScopeAndClass(t *testing.T) {
	grant := ExecutionGrant{
		Tenant:                  "acme",
		Scope:                   WorkspaceStorageScope(workspaceA),
		WritableArtifactClasses: []ArtifactClass{ArtifactSandboxRelation},
	}
	if err := validateOutput(grant, ArtifactSandboxRelation); err != nil {
		t.Fatalf("validateOutput() error = %v", err)
	}
	if err := validateOutput(grant, ArtifactSandboxDiagnostic); err == nil {
		t.Fatal("validateOutput() accepted an undeclared artifact class")
	}
	if validGrantOutputClass(ArtifactCheckpoint) {
		t.Fatal("grant output accepted a trusted-host artifact class")
	}
}

func TestValidateIssueGrantInputAllowsReadOnlyGrant(t *testing.T) {
	now := time.Now().UTC()
	input := IssueGrantInput{
		Tenant:               "acme",
		Scope:                WorkspaceStorageScope(workspaceA),
		ExecutionID:          "33333333-3333-4333-8333-333333333333",
		ReadableObjectIDs:    []string{"44444444-4444-4444-8444-444444444444"},
		IssuerServiceAccount: "transformation-storage-authority",
		ExpiresAt:            now.Add(time.Hour),
		CleanupOwnerID:       "22222222-2222-4222-8222-222222222222",
	}
	if err := validateIssueGrantInput(input, now); err != nil {
		t.Fatalf("validateIssueGrantInput() rejected a read-only grant: %v", err)
	}
	input.WritableArtifactClasses = []ArtifactClass{ArtifactCheckpoint}
	if err := validateIssueGrantInput(input, now); err == nil {
		t.Fatal("validateIssueGrantInput() accepted an invalid output class")
	}
}

func governedObjectForTest(tenant string, scope StorageScope) GovernedObject {
	return GovernedObject{Tenant: tenant, Scope: scope, State: ObjectStateActive}
}
