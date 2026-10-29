package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
)

const defaultLakeInventoryLimit = 200

type lakeInventoryRequest struct {
	SchemaVersion int          `json:"schemaVersion"`
	Authority     StorageScope `json:"authority"`
	Cursor        string       `json:"cursor,omitempty"`
	Limit         int          `json:"limit,omitempty"`
}

type lakeInventoryObject struct {
	PhysicalKey string      `json:"physicalKey"`
	State       ObjectState `json:"state"`
}

type lakeInventoryResponse struct {
	SchemaVersion int                   `json:"schemaVersion"`
	TenantID      string                `json:"tenantId"`
	Authority     StorageScope          `json:"authority"`
	Objects       []lakeInventoryObject `json:"objects"`
	NextCursor    string                `json:"nextCursor,omitempty"`
}

type lakeInventoryCursor struct {
	TenantID    string `json:"tenantId"`
	WorkspaceID string `json:"workspaceId"`
	LastObject  string `json:"lastObjectId"`
}

func encodeLakeInventoryCursor(cursor lakeInventoryCursor) (string, error) {
	encoded, err := json.Marshal(cursor)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(encoded), nil
}

func decodeLakeInventoryCursor(raw, tenant string, scope StorageScope) (string, error) {
	if raw == "" {
		return "", nil
	}
	encoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return "", ErrGovernedDenied
	}
	var cursor lakeInventoryCursor
	if err := json.Unmarshal(encoded, &cursor); err != nil || cursor.TenantID != tenant ||
		cursor.WorkspaceID != scope.WorkspaceID || !uuidPattern.MatchString(cursor.LastObject) {
		return "", ErrGovernedDenied
	}
	return cursor.LastObject, nil
}

func (s *server) handleInventoryLakeObjects(w http.ResponseWriter, r *http.Request) {
	tenant, _, ok := s.authorizeGoverned(w, r, OperationLakeDelete)
	if !ok {
		return
	}
	var request lakeInventoryRequest
	if !decodeGovernedJSON(w, r, &request) {
		return
	}
	if request.SchemaVersion != 1 || request.Authority.Validate() != nil ||
		request.Authority.Kind != StorageScopeWorkspace {
		writeJSON(w, http.StatusBadRequest, errorBody("workspace authority and schemaVersion 1 are required"))
		return
	}
	limit := request.Limit
	if limit == 0 {
		limit = defaultLakeInventoryLimit
	}
	if limit < 1 || limit > maxLakeDeleteObjects {
		writeJSON(w, http.StatusBadRequest, errorBody("inventory limit must be between 1 and 1000"))
		return
	}
	afterObjectID, err := decodeLakeInventoryCursor(request.Cursor, tenant, request.Authority)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody("invalid inventory cursor"))
		return
	}
	objects, hasMore, err := s.governedStore.InventoryLakeObjects(r.Context(), tenant,
		request.Authority, afterObjectID, limit)
	if err != nil {
		s.writeGovernedError(w, err)
		return
	}
	response := lakeInventoryResponse{
		SchemaVersion: 1, TenantID: tenant, Authority: request.Authority,
		Objects: make([]lakeInventoryObject, len(objects)),
	}
	for index, object := range objects {
		response.Objects[index] = lakeInventoryObject{PhysicalKey: object.Key, State: object.State}
	}
	if hasMore && len(objects) > 0 {
		response.NextCursor, err = encodeLakeInventoryCursor(lakeInventoryCursor{
			TenantID: tenant, WorkspaceID: request.Authority.WorkspaceID,
			LastObject: objects[len(objects)-1].ID,
		})
		if err != nil {
			s.writeGovernedError(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, response)
}
