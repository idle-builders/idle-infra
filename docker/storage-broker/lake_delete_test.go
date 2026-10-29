package main

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func newLakeDeleteObjectsRequest(path string, scope StorageScope, keys []string, quiet bool) *http.Request {
	var body bytes.Buffer
	_, _ = io.WriteString(&body, `<Delete xmlns="`+s3XMLNamespace+`">`)
	for _, key := range keys {
		_, _ = io.WriteString(&body, "<Object><Key>")
		_ = xml.EscapeText(&body, []byte(key))
		_, _ = io.WriteString(&body, "</Key></Object>")
	}
	if quiet {
		_, _ = io.WriteString(&body, "<Quiet>true</Quiet>")
	}
	_, _ = io.WriteString(&body, "</Delete>")
	return newRawLakeDeleteObjectsRequest(path, scope, body.Bytes())
}

func newRawLakeDeleteObjectsRequest(path string, scope StorageScope, body []byte) *http.Request {
	request := lakeRequest(http.MethodPost, path, scope, bytes.NewReader(body))
	digest := md5.Sum(body)
	request.Header.Set("Content-MD5", base64.StdEncoding.EncodeToString(digest[:]))
	request.Header.Set("Content-Type", "application/xml")
	return request
}

func seedLakeDeleteObject(store *lakeMemoryStore, backend *lakeMemoryBackend, scope StorageScope, key string) {
	payload := []byte("payload for " + key)
	object := activeLakeObject(scope, payload)
	object.ID = fmt.Sprintf("object-%d", len(store.objects)+1)
	object.Key = key
	store.objects[lakeMapKey(object.Bucket, key)] = object
	backend.objects[lakeMapKey(object.Bucket, key)] = payload
}

func decodeLakeDeleteResult(t *testing.T, response *httptest.ResponseRecorder) s3DeleteResult {
	t.Helper()
	var result s3DeleteResult
	if err := xml.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode DeleteResult: %v\n%s", err, response.Body.String())
	}
	return result
}

func TestLakeDeleteObjectsValidTenantAndExactWorkspaceBatches(t *testing.T) {
	for _, test := range []struct {
		name  string
		scope StorageScope
	}{
		{"tenant-public", TenantStorageScope()},
		{"exact workspace", WorkspaceStorageScope(workspaceA)},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := newLakeMemoryStore()
			backend := newLakeMemoryBackend()
			keys := []string{"ducklake/main/delete/a.parquet", "ducklake/main/delete/b.parquet"}
			for _, key := range keys {
				seedLakeDeleteObject(store, backend, test.scope, key)
			}
			s := lakeServerForTest(store, backend)
			response := httptest.NewRecorder()
			s.handleLakeS3(response, newLakeDeleteObjectsRequest("/acme-files?delete", test.scope, keys, false))

			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
			}
			result := decodeLakeDeleteResult(t, response)
			if len(result.Deleted) != len(keys) || len(result.Errors) != 0 {
				t.Fatalf("unexpected result: %#v", result)
			}
			for _, key := range keys {
				if store.objects[lakeMapKey("acme-files", key)].State != ObjectStateDeleted {
					t.Fatalf("row %q was not marked deleted", key)
				}
				if _, exists := backend.objects[lakeMapKey("acme-files", key)]; exists {
					t.Fatalf("physical object %q remains", key)
				}
			}
		})
	}
}

func TestLakeDeleteObjectsRejectsMixedAuthorityWithoutEffects(t *testing.T) {
	for _, test := range []struct {
		name      string
		requester StorageScope
		first     StorageScope
		second    StorageScope
	}{
		{"tenant and workspace", TenantStorageScope(), TenantStorageScope(), WorkspaceStorageScope(workspaceA)},
		{"workspace A and B", WorkspaceStorageScope(workspaceA), WorkspaceStorageScope(workspaceA), WorkspaceStorageScope(workspaceB)},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := newLakeMemoryStore()
			backend := newLakeMemoryBackend()
			keys := []string{"ducklake/main/delete/a.parquet", "ducklake/main/delete/b.parquet"}
			seedLakeDeleteObject(store, backend, test.first, keys[0])
			seedLakeDeleteObject(store, backend, test.second, keys[1])
			s := lakeServerForTest(store, backend)
			response := httptest.NewRecorder()
			s.handleLakeS3(response, newLakeDeleteObjectsRequest("/acme-files/?delete", test.requester, keys, false))

			if response.Code != http.StatusNotFound || backend.deleteCalls != 0 {
				t.Fatalf("status=%d backend calls=%d body=%s", response.Code, backend.deleteCalls, response.Body.String())
			}
			for _, key := range keys {
				if store.objects[lakeMapKey("acme-files", key)].State != ObjectStateActive {
					t.Fatalf("unauthorized batch changed %q", key)
				}
				if _, exists := backend.objects[lakeMapKey("acme-files", key)]; !exists {
					t.Fatalf("unauthorized batch deleted %q", key)
				}
			}
		})
	}
}

func TestLakeDeleteObjectsUnknownReservedAndDeletedRows(t *testing.T) {
	for _, state := range []ObjectState{""} {
		t.Run(string(state), func(t *testing.T) {
			store := newLakeMemoryStore()
			backend := newLakeMemoryBackend()
			activeKey := "ducklake/main/delete/active.parquet"
			unavailableKey := "ducklake/main/delete/unavailable.parquet"
			seedLakeDeleteObject(store, backend, TenantStorageScope(), activeKey)
			if state != "" {
				seedLakeDeleteObject(store, backend, TenantStorageScope(), unavailableKey)
				object := store.objects[lakeMapKey("acme-files", unavailableKey)]
				object.State = state
				store.objects[lakeMapKey("acme-files", unavailableKey)] = object
			}
			s := lakeServerForTest(store, backend)
			response := httptest.NewRecorder()
			s.handleLakeS3(response, newLakeDeleteObjectsRequest("/acme-files?delete", TenantStorageScope(),
				[]string{activeKey, unavailableKey}, false))

			if response.Code != http.StatusNotFound || backend.deleteCalls != 0 ||
				store.objects[lakeMapKey("acme-files", activeKey)].State != ObjectStateActive {
				t.Fatalf("unavailable batch had effects: status=%d calls=%d", response.Code, backend.deleteCalls)
			}
		})
	}

	reservedStore := newLakeMemoryStore()
	reservedBackend := newLakeMemoryBackend()
	reservedKey := "ducklake/main/delete/reserved.parquet"
	seedLakeDeleteObject(reservedStore, reservedBackend, TenantStorageScope(), reservedKey)
	reserved := reservedStore.objects[lakeMapKey("acme-files", reservedKey)]
	reserved.State = ObjectStateReserved
	reservedStore.objects[lakeMapKey("acme-files", reservedKey)] = reserved
	reservedServer := lakeServerForTest(reservedStore, reservedBackend)
	reservedResponse := httptest.NewRecorder()
	reservedServer.handleLakeS3(reservedResponse, newLakeDeleteObjectsRequest("/acme-files?delete",
		TenantStorageScope(), []string{reservedKey}, false))
	if reservedResponse.Code != http.StatusOK || reservedBackend.deleteCalls != 1 ||
		reservedStore.objects[lakeMapKey("acme-files", reservedKey)].State != ObjectStateDeleted {
		t.Fatalf("reserved cleanup = status %d calls %d", reservedResponse.Code, reservedBackend.deleteCalls)
	}

	store := newLakeMemoryStore()
	backend := newLakeMemoryBackend()
	key := "ducklake/main/delete/retry.parquet"
	seedLakeDeleteObject(store, backend, TenantStorageScope(), key)
	object := store.objects[lakeMapKey("acme-files", key)]
	object.State = ObjectStateDeleted
	store.objects[lakeMapKey("acme-files", key)] = object
	s := lakeServerForTest(store, backend)
	response := httptest.NewRecorder()
	s.handleLakeS3(response, newLakeDeleteObjectsRequest("/acme-files?delete", TenantStorageScope(), []string{key}, false))
	if response.Code != http.StatusOK || backend.deleteCalls != 1 {
		t.Fatalf("deleted retry status=%d calls=%d", response.Code, backend.deleteCalls)
	}
}

func TestLakeDeleteObjectsAuthenticationAndAuthorityDenials(t *testing.T) {
	key := "ducklake/main/delete/a.parquet"
	for _, test := range []struct {
		name   string
		mutate func(*http.Request, *server)
		want   int
	}{
		{"missing token", func(r *http.Request, _ *server) { r.Header.Del("x-amz-security-token") }, http.StatusUnauthorized},
		{"invalid token", func(_ *http.Request, s *server) { s.appAuth = rejectingReviewer{} }, http.StatusUnauthorized},
		{"mismatched token namespace", func(_ *http.Request, s *server) {
			s.appAuth = fixedReviewer{namespace: "atlas", account: "transformation-storage-authority"}
		}, http.StatusForbidden},
		{"invalid key id", func(r *http.Request, _ *server) {
			r.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=admin/20260908/us-east-1/s3/aws4_request, Signature=ignored")
		}, http.StatusForbidden},
		{"unapproved service account", func(_ *http.Request, s *server) {
			s.appAuth = fixedReviewer{namespace: "acme", account: "default"}
		}, http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := newLakeMemoryStore()
			backend := newLakeMemoryBackend()
			seedLakeDeleteObject(store, backend, TenantStorageScope(), key)
			s := lakeServerForTest(store, backend)
			request := newLakeDeleteObjectsRequest("/acme-files?delete", TenantStorageScope(), []string{key}, false)
			test.mutate(request, s)
			response := httptest.NewRecorder()
			s.handleLakeS3(response, request)
			if response.Code != test.want || backend.deleteCalls != 0 ||
				store.objects[lakeMapKey("acme-files", key)].State != ObjectStateActive {
				t.Fatalf("status=%d want=%d calls=%d", response.Code, test.want, backend.deleteCalls)
			}
		})
	}
}

func TestLakeDeleteObjectsRejectsMalformedRequests(t *testing.T) {
	validKey := "ducklake/main/delete/a.parquet"
	tests := []struct {
		name   string
		body   string
		mutate func(*http.Request)
		want   int
	}{
		{"malformed XML", `<Delete><Object><Key>` + validKey + `</Object></Delete>`, nil, http.StatusBadRequest},
		{"empty", `<Delete xmlns="` + s3XMLNamespace + `"></Delete>`, nil, http.StatusBadRequest},
		{"duplicate object", `<Delete><Object><Key>` + validKey + `</Key></Object><Object><Key>` + validKey + `</Key></Object></Delete>`, nil, http.StatusBadRequest},
		{"duplicate key element", `<Delete><Object><Key>` + validKey + `</Key><Key>` + validKey + `</Key></Object></Delete>`, nil, http.StatusBadRequest},
		{"nested key representation", `<Delete><Object><Key>ducklake/main/<Part>a.parquet</Part></Key></Object></Delete>`, nil, http.StatusBadRequest},
		{"version ID", `<Delete><Object><Key>` + validKey + `</Key><VersionId>1</VersionId></Object></Delete>`, nil, http.StatusBadRequest},
		{"outside root", `<Delete><Object><Key>corpus/a.parquet</Key></Object></Delete>`, nil, http.StatusBadRequest},
		{"overlong key", `<Delete><Object><Key>ducklake/` + strings.Repeat("a", 1025) + `</Key></Object></Delete>`, nil, http.StatusBadRequest},
		{"missing MD5", `<Delete><Object><Key>` + validKey + `</Key></Object></Delete>`, func(r *http.Request) { r.Header.Del("Content-MD5") }, http.StatusBadRequest},
		{"invalid MD5", `<Delete><Object><Key>` + validKey + `</Key></Object></Delete>`, func(r *http.Request) { r.Header.Set("Content-MD5", "not-base64") }, http.StatusBadRequest},
		{"mismatched MD5", `<Delete><Object><Key>` + validKey + `</Key></Object></Delete>`, func(r *http.Request) {
			r.Header.Set("Content-MD5", base64.StdEncoding.EncodeToString(make([]byte, md5.Size)))
		}, http.StatusBadRequest},
		{"wrong content type", `<Delete><Object><Key>` + validKey + `</Key></Object></Delete>`, func(r *http.Request) { r.Header.Set("Content-Type", "text/xml") }, http.StatusBadRequest},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := newLakeMemoryStore()
			backend := newLakeMemoryBackend()
			s := lakeServerForTest(store, backend)
			request := newRawLakeDeleteObjectsRequest("/acme-files?delete", TenantStorageScope(), []byte(test.body))
			if test.mutate != nil {
				test.mutate(request)
			}
			response := httptest.NewRecorder()
			s.handleLakeS3(response, request)
			if response.Code != test.want || backend.deleteCalls != 0 || store.deleteCalls != 0 {
				t.Fatalf("status=%d want=%d store=%d backend=%d body=%s", response.Code, test.want,
					store.deleteCalls, backend.deleteCalls, response.Body.String())
			}
		})
	}
}

func TestLakeDeleteObjectsEnforcesCountAndBodyLimits(t *testing.T) {
	keys := make([]string, maxLakeDeleteObjects+1)
	for index := range keys {
		keys[index] = fmt.Sprintf("ducklake/main/delete/%04d.parquet", index)
	}
	maximumRequest := newLakeDeleteObjectsRequest("/acme-files?delete", TenantStorageScope(),
		keys[:maxLakeDeleteObjects], false)
	maximumBody, err := io.ReadAll(maximumRequest.Body)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := parseLakeDeleteXML(maximumBody, "ducklake/")
	if err != nil || len(parsed.Keys) != maxLakeDeleteObjects {
		t.Fatalf("maximum valid batch parsed %d keys: %v", len(parsed.Keys), err)
	}
	store := newLakeMemoryStore()
	backend := newLakeMemoryBackend()
	s := lakeServerForTest(store, backend)
	overCount := httptest.NewRecorder()
	s.handleLakeS3(overCount, newLakeDeleteObjectsRequest("/acme-files?delete", TenantStorageScope(), keys, false))
	if overCount.Code != http.StatusBadRequest || store.deleteCalls != 0 || backend.deleteCalls != 0 {
		t.Fatalf("over-count status=%d store=%d backend=%d", overCount.Code, store.deleteCalls, backend.deleteCalls)
	}

	oversized := bytes.Repeat([]byte("x"), maxLakeDeleteBodyBytes+1)
	overBodyRequest := newRawLakeDeleteObjectsRequest("/acme-files?delete", TenantStorageScope(), oversized)
	overBody := httptest.NewRecorder()
	s.handleLakeS3(overBody, overBodyRequest)
	if overBody.Code != http.StatusRequestEntityTooLarge || store.deleteCalls != 0 || backend.deleteCalls != 0 {
		t.Fatalf("over-body status=%d store=%d backend=%d", overBody.Code, store.deleteCalls, backend.deleteCalls)
	}
}

func TestLakeDeleteObjectsRetriesCompleteAndPartialResults(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(fmt.Sprintf("partial=%t", partial), func(t *testing.T) {
			store := newLakeMemoryStore()
			backend := newLakeMemoryBackend()
			keys := []string{"ducklake/main/delete/a.parquet", "ducklake/main/delete/b.parquet"}
			for _, key := range keys {
				seedLakeDeleteObject(store, backend, TenantStorageScope(), key)
			}
			if partial {
				backend.deleteErrors = map[string]error{keys[1]: errors.New("temporary backend failure")}
			}
			s := lakeServerForTest(store, backend)
			first := httptest.NewRecorder()
			s.handleLakeS3(first, newLakeDeleteObjectsRequest("/acme-files?delete", TenantStorageScope(), keys, false))
			if first.Code != http.StatusOK {
				t.Fatalf("first status=%d body=%s", first.Code, first.Body.String())
			}
			firstResult := decodeLakeDeleteResult(t, first)
			if partial && (len(firstResult.Deleted) != 1 || len(firstResult.Errors) != 1) {
				t.Fatalf("partial result = %#v", firstResult)
			}
			backend.deleteErrors = nil
			second := httptest.NewRecorder()
			s.handleLakeS3(second, newLakeDeleteObjectsRequest("/acme-files?delete", TenantStorageScope(), keys, false))
			secondResult := decodeLakeDeleteResult(t, second)
			if second.Code != http.StatusOK || len(secondResult.Deleted) != 2 || len(secondResult.Errors) != 0 ||
				backend.deleteCalls != 4 {
				t.Fatalf("retry status=%d result=%#v calls=%d", second.Code, secondResult, backend.deleteCalls)
			}
		})
	}
}

func TestLakeDeleteObjectsQuietResponseAndConcurrentReservation(t *testing.T) {
	store := newLakeMemoryStore()
	backend := newLakeMemoryBackend()
	key := "ducklake/main/delete/race.parquet"
	seedLakeDeleteObject(store, backend, WorkspaceStorageScope(workspaceA), key)
	object := store.objects[lakeMapKey("acme-files", key)]

	start := make(chan struct{})
	var wait sync.WaitGroup
	wait.Add(2)
	var deleteErr, reserveErr error
	go func() {
		defer wait.Done()
		<-start
		_, deleteErr = store.DeleteLakeObjects(context.Background(), "acme", WorkspaceStorageScope(workspaceA),
			"acme-files", []string{key}, time.Now())
	}()
	go func() {
		defer wait.Done()
		<-start
		_, reserveErr = store.ReserveLakeObject(context.Background(), RegisterActiveObjectInput{
			Tenant: object.Tenant, Scope: object.Scope, ArtifactClass: object.ArtifactClass,
			ContentHash: object.ContentHash, ByteSize: object.ByteSize, MediaType: object.MediaType,
			Bucket: object.Bucket, Key: object.Key,
		})
	}()
	close(start)
	wait.Wait()
	if deleteErr != nil || (reserveErr != nil && !errors.Is(reserveErr, ErrGovernedConflict)) ||
		store.objects[lakeMapKey("acme-files", key)].State != ObjectStateDeleted {
		t.Fatalf("delete=%v reserve=%v state=%s", deleteErr, reserveErr,
			store.objects[lakeMapKey("acme-files", key)].State)
	}

	s := lakeServerForTest(store, backend)
	response := httptest.NewRecorder()
	s.handleLakeS3(response, newLakeDeleteObjectsRequest("/acme-files?delete", WorkspaceStorageScope(workspaceA), []string{key}, true))
	result := decodeLakeDeleteResult(t, response)
	if response.Code != http.StatusOK || len(result.Deleted) != 0 || len(result.Errors) != 0 {
		t.Fatalf("quiet response status=%d result=%#v", response.Code, result)
	}
}
