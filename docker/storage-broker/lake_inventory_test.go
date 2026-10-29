package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLakeInventoryPaginatesExactWorkspaceAndBindsCursor(t *testing.T) {
	object := func(id, workspace, key string, state ObjectState) GovernedObject {
		return GovernedObject{ID: id, Tenant: "nimmly", Scope: WorkspaceStorageScope(workspace),
			ArtifactClass: ArtifactDuckLakeData, State: state, Key: key}
	}
	store := &fakeGovernedStore{lakeInventory: []GovernedObject{
		object("11111111-1111-4111-8111-111111111111", workspaceA, "ducklake/a.parquet", ObjectStateReserved),
		object("22222222-2222-4222-8222-222222222222", workspaceA, "ducklake/b.parquet", ObjectStateActive),
		object("33333333-3333-4333-8333-333333333333", workspaceA, "ducklake/c.parquet", ObjectStateDeleted),
		object("44444444-4444-4444-8444-444444444444", workspaceB, "ducklake/other.parquet", ObjectStateActive),
	}}
	server := governedServerForTest("transformation-storage-authority", store)
	call := func(workspace, cursor string) *httptest.ResponseRecorder {
		body := map[string]any{"schemaVersion": 1, "authority": WorkspaceStorageScope(workspace), "limit": 2}
		if cursor != "" {
			body["cursor"] = cursor
		}
		encoded, _ := json.Marshal(body)
		request := httptest.NewRequest(http.MethodPost, "/v2/lake-objects/inventory",
			strings.NewReader(string(encoded)))
		request.Header.Set("Authorization", "Bearer test-token")
		response := httptest.NewRecorder()
		server.handleInventoryLakeObjects(response, request)
		return response
	}
	first := call(workspaceA, "")
	if first.Code != http.StatusOK {
		t.Fatalf("first page = %d, body %s", first.Code, first.Body.String())
	}
	var firstBody lakeInventoryResponse
	if err := json.Unmarshal(first.Body.Bytes(), &firstBody); err != nil {
		t.Fatal(err)
	}
	if len(firstBody.Objects) != 2 || firstBody.NextCursor == "" ||
		firstBody.Objects[0].State != ObjectStateReserved {
		t.Fatalf("first page = %#v", firstBody)
	}
	second := call(workspaceA, firstBody.NextCursor)
	var secondBody lakeInventoryResponse
	if err := json.Unmarshal(second.Body.Bytes(), &secondBody); err != nil {
		t.Fatal(err)
	}
	if second.Code != http.StatusOK || len(secondBody.Objects) != 1 || secondBody.NextCursor != "" ||
		secondBody.Objects[0].PhysicalKey != "ducklake/c.parquet" {
		t.Fatalf("second page = %d %#v", second.Code, secondBody)
	}
	wrongWorkspace := call(workspaceB, firstBody.NextCursor)
	if wrongWorkspace.Code != http.StatusBadRequest {
		t.Fatalf("cross-workspace cursor status = %d", wrongWorkspace.Code)
	}
}

func TestLakeInventoryRequiresWorkspaceAuthorityAndBoundedLimit(t *testing.T) {
	server := governedServerForTest("transformation-storage-authority", &fakeGovernedStore{})
	for _, body := range []string{
		`{"schemaVersion":1,"authority":{"kind":"tenant"}}`,
		`{"schemaVersion":1,"authority":{"kind":"workspace","workspaceId":"` + workspaceA + `"},"limit":1001}`,
	} {
		request := httptest.NewRequest(http.MethodPost, "/v2/lake-objects/inventory", strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer test-token")
		response := httptest.NewRecorder()
		server.handleInventoryLakeObjects(response, request)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
		}
	}
}
