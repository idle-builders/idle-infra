package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const lakeObjectID = "44444444-4444-4444-8444-444444444444"

type rejectingReviewer struct{}

func (rejectingReviewer) Review(context.Context, string) (string, string, error) {
	return "", "", errors.New("rejected")
}

type lakeMemoryStore struct {
	*fakeGovernedStore
	mu                             sync.Mutex
	objects                        map[string]GovernedObject
	reserveCalls                   int
	activateCalls                  int
	deleteCalls                    int
	activationGate                 func()
	activateErr                    error
	lakeRegistrationLockHeld       bool
	reserveSawLakeRegistrationLock bool
}

func newLakeMemoryStore() *lakeMemoryStore {
	return &lakeMemoryStore{fakeGovernedStore: &fakeGovernedStore{}, objects: make(map[string]GovernedObject)}
}

func lakeMapKey(bucket, key string) string { return bucket + "\x00" + key }

func (s *lakeMemoryStore) AcquireLakePathLock(context.Context, string, string, string) (func(), error) {
	return func() {}, nil
}

func (s *lakeMemoryStore) AcquireLakeRegistrationLock(context.Context, string, string, string) (func(), error) {
	s.lakeRegistrationLockHeld = true
	s.registrationBarrier = true
	return func() {
		s.lakeRegistrationLockHeld = false
		s.registrationBarrier = false
	}, nil
}

func (s *lakeMemoryStore) ReserveLakeObject(_ context.Context, input RegisterActiveObjectInput) (GovernedObject, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reserveSawLakeRegistrationLock = s.lakeRegistrationLockHeld
	s.reserveCalls++
	mapKey := lakeMapKey(input.Bucket, input.Key)
	if existing, ok := s.objects[mapKey]; ok {
		if existing.Tenant != input.Tenant || existing.Scope != input.Scope ||
			existing.ArtifactClass != input.ArtifactClass || existing.ContentHash != input.ContentHash ||
			existing.ByteSize != input.ByteSize || existing.MediaType != input.MediaType ||
			(existing.State != ObjectStateReserved && existing.State != ObjectStateActive) {
			return GovernedObject{}, ErrGovernedConflict
		}
		return existing, nil
	}
	object := GovernedObject{
		ID: lakeObjectID, Tenant: input.Tenant, Scope: input.Scope, ArtifactClass: input.ArtifactClass,
		ContentHash: input.ContentHash, ByteSize: input.ByteSize, MediaType: input.MediaType,
		State: ObjectStateReserved, Bucket: input.Bucket, Key: input.Key,
	}
	s.objects[mapKey] = object
	return object, nil
}

func (s *lakeMemoryStore) ActivateLakeObject(_ context.Context, tenant, objectID string) (GovernedObject, error) {
	if s.activationGate != nil {
		s.activationGate()
	}
	if s.activateErr != nil {
		return GovernedObject{}, s.activateErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.activateCalls++
	for key, object := range s.objects {
		if object.Tenant == tenant && object.ID == objectID && object.State == ObjectStateReserved {
			object.State = ObjectStateActive
			s.objects[key] = object
			return object, nil
		}
	}
	return GovernedObject{}, ErrGovernedDenied
}

func (s *lakeMemoryStore) ReadLakeObject(
	_ context.Context,
	tenant string,
	scope StorageScope,
	bucket, key string,
) (GovernedObject, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	object, ok := s.objects[lakeMapKey(bucket, key)]
	if !ok || object.State != ObjectStateActive || object.Tenant != tenant || !scope.AllowsObject(object.Scope) {
		return GovernedObject{}, ErrGovernedDenied
	}
	return object, nil
}

func (s *lakeMemoryStore) DeleteLakeObject(
	_ context.Context,
	tenant string,
	scope StorageScope,
	bucket, key string,
	_ time.Time,
) (GovernedObject, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleteCalls++
	mapKey := lakeMapKey(bucket, key)
	object, ok := s.objects[mapKey]
	if !ok || object.Tenant != tenant || object.Scope != scope ||
		(object.State != ObjectStateReserved && object.State != ObjectStateActive && object.State != ObjectStateDeleted) {
		return GovernedObject{}, ErrGovernedDenied
	}
	object.State = ObjectStateDeleted
	s.objects[mapKey] = object
	return object, nil
}

func (s *lakeMemoryStore) DeleteLakeObjects(
	_ context.Context,
	tenant string,
	scope StorageScope,
	bucket string,
	keys []string,
	_ time.Time,
) ([]GovernedObject, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	objects := make([]GovernedObject, len(keys))
	for index, key := range keys {
		object, ok := s.objects[lakeMapKey(bucket, key)]
		if !ok || object.Tenant != tenant || object.Scope != scope ||
			(object.State != ObjectStateReserved && object.State != ObjectStateActive && object.State != ObjectStateDeleted) {
			return nil, ErrGovernedDenied
		}
		objects[index] = object
	}
	for index, key := range keys {
		objects[index].State = ObjectStateDeleted
		s.objects[lakeMapKey(bucket, key)] = objects[index]
	}
	s.deleteCalls += len(keys)
	return objects, nil
}

type lakeMemoryBackend struct {
	mu            sync.Mutex
	objects       map[string][]byte
	putCalls      int
	deleteCalls   int
	putErr        error
	deleteErr     error
	deleteErrors  map[string]error
	discardPut    bool
	discardDelete bool
}

func newLakeMemoryBackend() *lakeMemoryBackend {
	return &lakeMemoryBackend{objects: make(map[string][]byte)}
}

func (b *lakeMemoryBackend) Put(_ context.Context, bucket, key string, body io.Reader, _ int64, _ string) error {
	b.mu.Lock()
	b.putCalls++
	putErr := b.putErr
	discard := b.discardPut
	b.mu.Unlock()
	if putErr != nil {
		return putErr
	}
	if discard {
		_, err := io.Copy(io.Discard, body)
		return err
	}
	payload, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	b.mu.Lock()
	b.objects[lakeMapKey(bucket, key)] = payload
	b.mu.Unlock()
	return nil
}

func (b *lakeMemoryBackend) Get(_ context.Context, bucket, key, byteRange string) (lakeObjectMetadata, error) {
	b.mu.Lock()
	payload, ok := b.objects[lakeMapKey(bucket, key)]
	b.mu.Unlock()
	if !ok {
		return lakeObjectMetadata{}, errors.New("missing")
	}
	start, end := int64(0), int64(len(payload)-1)
	contentRange := ""
	if byteRange != "" {
		var err error
		start, end, err = testByteRange(byteRange, int64(len(payload)))
		if err != nil {
			return lakeObjectMetadata{}, err
		}
		contentRange = fmt.Sprintf("bytes %d-%d/%d", start, end, len(payload))
	}
	selected := append([]byte(nil), payload[start:end+1]...)
	return lakeObjectMetadata{
		Body: io.NopCloser(bytes.NewReader(selected)), ContentLength: int64(len(selected)),
		ContentRange: contentRange, LastModified: time.Unix(1, 0),
	}, nil
}

func testByteRange(value string, size int64) (int64, int64, error) {
	raw := strings.TrimPrefix(value, "bytes=")
	left, right, _ := strings.Cut(raw, "-")
	if left == "" {
		length, _ := strconv.ParseInt(right, 10, 64)
		return size - length, size - 1, nil
	}
	start, _ := strconv.ParseInt(left, 10, 64)
	end := size - 1
	if right != "" {
		end, _ = strconv.ParseInt(right, 10, 64)
	}
	if start < 0 || start > end || end >= size {
		return 0, 0, errors.New("invalid range")
	}
	return start, end, nil
}

func (b *lakeMemoryBackend) Head(_ context.Context, bucket, key string) (lakeObjectMetadata, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	payload, ok := b.objects[lakeMapKey(bucket, key)]
	if !ok {
		return lakeObjectMetadata{}, errors.New("missing")
	}
	return lakeObjectMetadata{ContentLength: int64(len(payload)), LastModified: time.Unix(1, 0)}, nil
}

func (b *lakeMemoryBackend) ConfirmAbsent(_ context.Context, bucket, key string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.objects[lakeMapKey(bucket, key)]; ok {
		return errors.New("object remains")
	}
	return nil
}

func (b *lakeMemoryBackend) Delete(_ context.Context, bucket, key string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.deleteCalls++
	if b.deleteErr != nil {
		return b.deleteErr
	}
	if b.discardDelete {
		return nil
	}
	delete(b.objects, lakeMapKey(bucket, key))
	return nil
}

func (b *lakeMemoryBackend) DeleteObjects(_ context.Context, bucket string, keys []string) ([]lakeDeleteOutcome, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.deleteCalls += len(keys)
	if b.deleteErr != nil {
		return nil, b.deleteErr
	}
	outcomes := make([]lakeDeleteOutcome, len(keys))
	for index, key := range keys {
		if err := b.deleteErrors[key]; err != nil {
			outcomes[index] = lakeDeleteOutcome{Key: key, Code: "InternalError", Message: err.Error()}
			continue
		}
		delete(b.objects, lakeMapKey(bucket, key))
		outcomes[index] = lakeDeleteOutcome{Key: key, Deleted: true}
	}
	return outcomes, nil
}

func lakeServerForTest(store GovernedStore, backend lakeObjectBackend) *server {
	return &server{
		cfg: &Config{
			DuckLakeRoot: "ducklake/", DuckLakeMaxPut: 96 * 1024 * 1024,
			LakeOperationTTL: time.Minute, LakeIOIdleTTL: time.Second, LakeMaxConcurrency: 4,
		},
		policy:  &Policy{Namespaces: map[string]NamespacePolicy{"nimmly": {Bucket: "nimmly-files"}}},
		appAuth: fixedReviewer{namespace: "nimmly", account: "transformation-storage-authority"},
		governedPolicy: &GovernedPolicy{Namespaces: map[string]GovernedNamespacePolicy{
			"nimmly": {ServiceAccounts: map[string][]GovernedOperation{
				"transformation-storage-authority": {OperationLakeRead, OperationLakeWrite, OperationLakeDelete},
			}},
		}},
		governedStore: store, lakeBackend: backend, lakeSlots: make(chan struct{}, 4),
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func lakeAuthorization(scope StorageScope) string {
	key := "tenant"
	if scope.Kind == StorageScopeWorkspace {
		key = "workspace-" + scope.WorkspaceID
	}
	return "AWS4-HMAC-SHA256 Credential=" + key + "/20260908/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date;x-amz-security-token, Signature=ignored"
}

func lakeRequest(method, path string, scope StorageScope, body io.Reader) *http.Request {
	request := httptest.NewRequest(method, path, body)
	request.Header.Set("x-amz-security-token", "reviewed-token")
	request.Header.Set("Authorization", lakeAuthorization(scope))
	return request
}

func lakePutRequest(path string, scope StorageScope, payload []byte) *http.Request {
	request := lakeRequest(http.MethodPut, path, scope, bytes.NewReader(payload))
	digest := sha256.Sum256(payload)
	request.Header.Set("x-amz-content-sha256", hex.EncodeToString(digest[:]))
	request.Header.Set("Content-Type", "application/vnd.apache.parquet")
	return request
}

func activeLakeObject(scope StorageScope, payload []byte) GovernedObject {
	digest := sha256.Sum256(payload)
	return GovernedObject{
		ID: lakeObjectID, Tenant: "nimmly", Scope: scope, ArtifactClass: ArtifactDuckLakeData,
		ContentHash: hex.EncodeToString(digest[:]), ByteSize: int64(len(payload)),
		MediaType: "application/vnd.apache.parquet", State: ObjectStateActive,
		Bucket: "nimmly-files", Key: "ducklake/main/facts/ducklake-file.parquet",
	}
}

func TestLakePutStreamsExactPathAndActivatesBeforeSuccess(t *testing.T) {
	store := newLakeMemoryStore()
	backend := newLakeMemoryBackend()
	payload := []byte("parquet bytes")
	store.activationGate = func() {
		if string(backend.objects[lakeMapKey("nimmly-files", "ducklake/main/facts/ducklake-file.parquet")]) != string(payload) {
			t.Fatal("activation ran before the physical PUT completed")
		}
	}
	s := lakeServerForTest(store, backend)
	response := httptest.NewRecorder()
	s.handleLakeS3(response, lakePutRequest("/nimmly-files/ducklake/main/facts/ducklake-file.parquet", TenantStorageScope(), payload))

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	object := store.objects[lakeMapKey("nimmly-files", "ducklake/main/facts/ducklake-file.parquet")]
	if object.State != ObjectStateActive || object.Key != "ducklake/main/facts/ducklake-file.parquet" ||
		object.ArtifactClass != ArtifactDuckLakeData || store.activateCalls != 1 ||
		!store.reserveSawLakeRegistrationLock || store.lakeRegistrationLockHeld {
		t.Fatalf("unexpected governed object: %#v", object)
	}
	if !slices.Contains(store.claimedBuckets, "nimmly/nimmly-files") {
		t.Fatalf("Lake PUT did not claim persisted bucket ownership: %#v", store.claimedBuckets)
	}
	if response.Header().Get("ETag") != `"`+object.ContentHash+`"` {
		t.Fatalf("ETag = %q", response.Header().Get("ETag"))
	}
}

func TestLakePutRejectsConflictingPersistedBucketOwnerBeforeReservation(t *testing.T) {
	store := newLakeMemoryStore()
	store.bucketOwnerConflict = true
	backend := newLakeMemoryBackend()
	s := lakeServerForTest(store, backend)
	response := httptest.NewRecorder()
	s.handleLakeS3(response, lakePutRequest(
		"/nimmly-files/ducklake/main/facts/foreign.parquet", TenantStorageScope(), []byte("parquet bytes")))

	if response.Code != http.StatusConflict || store.reserveCalls != 0 ||
		store.lakeRegistrationLockHeld || store.registrationBarrier || len(backend.objects) != 0 {
		t.Fatalf("status = %d, reserves = %d, locks = %t/%t, objects = %#v, body = %s",
			response.Code, store.reserveCalls, store.lakeRegistrationLockHeld,
			store.registrationBarrier, backend.objects, response.Body.String())
	}
}

func TestLakePutRejectsHashAndSizeMismatchWithoutReadableObject(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*http.Request)
	}{
		{"hash mismatch", func(request *http.Request) {
			request.Header.Set("x-amz-content-sha256", strings.Repeat("a", 64))
		}},
		{"size mismatch", func(request *http.Request) { request.ContentLength++ }},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := newLakeMemoryStore()
			backend := newLakeMemoryBackend()
			s := lakeServerForTest(store, backend)
			request := lakePutRequest("/nimmly-files/ducklake/main/t/file.parquet", TenantStorageScope(), []byte("bytes"))
			test.mutate(request)
			response := httptest.NewRecorder()
			s.handleLakeS3(response, request)
			if response.Code == http.StatusOK {
				t.Fatal("mismatching PUT succeeded")
			}
			if _, err := store.ReadLakeObject(context.Background(), "nimmly", TenantStorageScope(),
				"nimmly-files", "ducklake/main/t/file.parquet"); !errors.Is(err, ErrGovernedDenied) {
				t.Fatalf("failed upload became readable: %v", err)
			}
			if _, exists := backend.objects[lakeMapKey("nimmly-files", "ducklake/main/t/file.parquet")]; exists {
				t.Fatal("mismatching physical object was not removed")
			}
		})
	}
}

func TestLakePutRecoveryAndImmutableRetry(t *testing.T) {
	payload := []byte("stable bytes")
	path := "/nimmly-files/ducklake/main/t/file.parquet"
	key := "ducklake/main/t/file.parquet"

	t.Run("reserved after interrupted upload retries", func(t *testing.T) {
		store := newLakeMemoryStore()
		backend := newLakeMemoryBackend()
		backend.putErr = errors.New("interrupted")
		s := lakeServerForTest(store, backend)
		first := httptest.NewRecorder()
		s.handleLakeS3(first, lakePutRequest(path, TenantStorageScope(), payload))
		if first.Code == http.StatusOK || store.objects[lakeMapKey("nimmly-files", key)].State != ObjectStateReserved {
			t.Fatal("interrupted upload did not remain an unreadable reservation")
		}
		backend.putErr = nil
		second := httptest.NewRecorder()
		s.handleLakeS3(second, lakePutRequest(path, TenantStorageScope(), payload))
		if second.Code != http.StatusOK || store.objects[lakeMapKey("nimmly-files", key)].State != ObjectStateActive {
			t.Fatalf("retry status = %d", second.Code)
		}
	})

	t.Run("stored bytes before activation retries", func(t *testing.T) {
		store := newLakeMemoryStore()
		backend := newLakeMemoryBackend()
		s := lakeServerForTest(store, backend)
		store.activateErr = errors.New("database unavailable")
		first := httptest.NewRecorder()
		s.handleLakeS3(first, lakePutRequest(path, TenantStorageScope(), payload))
		if first.Code == http.StatusOK || string(backend.objects[lakeMapKey("nimmly-files", key)]) != string(payload) {
			t.Fatal("activation failure boundary was not established")
		}
		store.activateErr = nil
		second := httptest.NewRecorder()
		s.handleLakeS3(second, lakePutRequest(path, TenantStorageScope(), payload))
		if second.Code != http.StatusOK {
			t.Fatalf("retry status = %d", second.Code)
		}
	})

	t.Run("active retry never overwrites", func(t *testing.T) {
		store := newLakeMemoryStore()
		backend := newLakeMemoryBackend()
		object := activeLakeObject(TenantStorageScope(), payload)
		object.Key = key
		store.objects[lakeMapKey(object.Bucket, object.Key)] = object
		backend.objects[lakeMapKey(object.Bucket, object.Key)] = append([]byte(nil), payload...)
		s := lakeServerForTest(store, backend)
		response := httptest.NewRecorder()
		s.handleLakeS3(response, lakePutRequest(path, TenantStorageScope(), payload))
		if response.Code != http.StatusOK || backend.putCalls != 0 {
			t.Fatalf("active retry status=%d putCalls=%d", response.Code, backend.putCalls)
		}
		bad := lakePutRequest(path, TenantStorageScope(), []byte("evil bytes"))
		bad.Header.Set("x-amz-content-sha256", object.ContentHash)
		badResponse := httptest.NewRecorder()
		s.handleLakeS3(badResponse, bad)
		if badResponse.Code == http.StatusOK || string(backend.objects[lakeMapKey(object.Bucket, object.Key)]) != string(payload) {
			t.Fatal("active object was overwritten by different bytes")
		}
	})
}

func TestLakeReadHeadAndScopeMatrix(t *testing.T) {
	publicPayload := []byte("0123456789")
	privatePayload := []byte("workspace secret")
	store := newLakeMemoryStore()
	backend := newLakeMemoryBackend()
	public := activeLakeObject(TenantStorageScope(), publicPayload)
	private := activeLakeObject(WorkspaceStorageScope(workspaceA), privatePayload)
	private.ID = "55555555-5555-4555-8555-555555555555"
	private.Key = "ducklake/main/private/ducklake-private.parquet"
	store.objects[lakeMapKey(public.Bucket, public.Key)] = public
	store.objects[lakeMapKey(private.Bucket, private.Key)] = private
	backend.objects[lakeMapKey(public.Bucket, public.Key)] = publicPayload
	backend.objects[lakeMapKey(private.Bucket, private.Key)] = privatePayload
	s := lakeServerForTest(store, backend)

	for _, test := range []struct {
		name   string
		scope  StorageScope
		object GovernedObject
		want   int
	}{
		{"tenant reads public", TenantStorageScope(), public, http.StatusOK},
		{"workspace reads public overlay", WorkspaceStorageScope(workspaceA), public, http.StatusOK},
		{"same workspace reads private", WorkspaceStorageScope(workspaceA), private, http.StatusOK},
		{"tenant cannot read private", TenantStorageScope(), private, http.StatusNotFound},
		{"other workspace cannot read private", WorkspaceStorageScope(workspaceB), private, http.StatusNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			request := lakeRequest(http.MethodGet, "/"+test.object.Bucket+"/"+test.object.Key, test.scope, nil)
			s.handleLakeS3(response, request)
			if response.Code != test.want {
				t.Fatalf("status = %d, want %d", response.Code, test.want)
			}
		})
	}

	rangeResponse := httptest.NewRecorder()
	rangeRequest := lakeRequest(http.MethodGet, "/"+public.Bucket+"/"+public.Key, TenantStorageScope(), nil)
	rangeRequest.Header.Set("Range", "bytes=2-5")
	s.handleLakeS3(rangeResponse, rangeRequest)
	if rangeResponse.Code != http.StatusPartialContent || rangeResponse.Body.String() != "2345" ||
		rangeResponse.Header().Get("Content-Range") != "bytes 2-5/10" {
		t.Fatalf("range response = %d %#v %q", rangeResponse.Code, rangeResponse.Header(), rangeResponse.Body.String())
	}

	headResponse := httptest.NewRecorder()
	s.handleLakeS3(headResponse, lakeRequest(http.MethodHead, "/"+public.Bucket+"/"+public.Key, TenantStorageScope(), nil))
	if headResponse.Code != http.StatusOK || headResponse.Header().Get("Content-Length") != "10" ||
		headResponse.Header().Get("Accept-Ranges") != "bytes" ||
		headResponse.Header().Get("ETag") != `"`+public.ContentHash+`"` {
		t.Fatalf("HEAD response = %d %#v", headResponse.Code, headResponse.Header())
	}
}

func TestLakeDeleteUsesExactWriteScopeAndRetriesPhysicalFailure(t *testing.T) {
	payload := []byte("bytes")
	store := newLakeMemoryStore()
	backend := newLakeMemoryBackend()
	public := activeLakeObject(TenantStorageScope(), payload)
	store.objects[lakeMapKey(public.Bucket, public.Key)] = public
	backend.objects[lakeMapKey(public.Bucket, public.Key)] = payload
	s := lakeServerForTest(store, backend)

	denied := httptest.NewRecorder()
	s.handleLakeS3(denied, lakeRequest(http.MethodDelete, "/"+public.Bucket+"/"+public.Key,
		WorkspaceStorageScope(workspaceA), nil))
	if denied.Code != http.StatusNotFound || store.objects[lakeMapKey(public.Bucket, public.Key)].State != ObjectStateActive {
		t.Fatal("workspace deletion affected tenant-public data")
	}

	backend.deleteErr = errors.New("temporary")
	failed := httptest.NewRecorder()
	s.handleLakeS3(failed, lakeRequest(http.MethodDelete, "/"+public.Bucket+"/"+public.Key, TenantStorageScope(), nil))
	if failed.Code == http.StatusNoContent || store.objects[lakeMapKey(public.Bucket, public.Key)].State != ObjectStateDeleted {
		t.Fatal("failed physical deletion did not revoke reads first")
	}
	read := httptest.NewRecorder()
	s.handleLakeS3(read, lakeRequest(http.MethodGet, "/"+public.Bucket+"/"+public.Key, TenantStorageScope(), nil))
	if read.Code != http.StatusNotFound {
		t.Fatal("deleted row remained readable")
	}

	backend.deleteErr = nil
	backend.discardDelete = true
	unconfirmed := httptest.NewRecorder()
	s.handleLakeS3(unconfirmed, lakeRequest(http.MethodDelete, "/"+public.Bucket+"/"+public.Key, TenantStorageScope(), nil))
	if unconfirmed.Code != http.StatusBadGateway || len(store.confirmedLakeKeys) != 0 {
		t.Fatalf("unconfirmed delete status=%d confirmations=%#v", unconfirmed.Code, store.confirmedLakeKeys)
	}
	backend.discardDelete = false
	retry := httptest.NewRecorder()
	s.handleLakeS3(retry, lakeRequest(http.MethodDelete, "/"+public.Bucket+"/"+public.Key, TenantStorageScope(), nil))
	if retry.Code != http.StatusNoContent || backend.deleteCalls != 3 || len(store.confirmedLakeKeys) != 1 {
		t.Fatalf("delete retry status=%d calls=%d", retry.Code, backend.deleteCalls)
	}
}

func TestLakeCrossWorkspaceWriteAndDeleteFail(t *testing.T) {
	payload := []byte("workspace A bytes")
	store := newLakeMemoryStore()
	backend := newLakeMemoryBackend()
	object := activeLakeObject(WorkspaceStorageScope(workspaceA), payload)
	store.objects[lakeMapKey(object.Bucket, object.Key)] = object
	backend.objects[lakeMapKey(object.Bucket, object.Key)] = append([]byte(nil), payload...)
	s := lakeServerForTest(store, backend)
	path := "/" + object.Bucket + "/" + object.Key

	put := httptest.NewRecorder()
	s.handleLakeS3(put, lakePutRequest(path, WorkspaceStorageScope(workspaceB), payload))
	deleteResponse := httptest.NewRecorder()
	s.handleLakeS3(deleteResponse, lakeRequest(http.MethodDelete, path, WorkspaceStorageScope(workspaceB), nil))

	if put.Code != http.StatusConflict || deleteResponse.Code != http.StatusNotFound ||
		string(backend.objects[lakeMapKey(object.Bucket, object.Key)]) != string(payload) ||
		store.objects[lakeMapKey(object.Bucket, object.Key)].State != ObjectStateActive {
		t.Fatalf("cross-workspace PUT=%d DELETE=%d object=%#v", put.Code, deleteResponse.Code,
			store.objects[lakeMapKey(object.Bucket, object.Key)])
	}
}

func TestLakeRequestBoundaryDenials(t *testing.T) {
	payload := []byte("bytes")
	tests := []struct {
		name   string
		method string
		path   string
		mutate func(*http.Request, *server)
		want   int
	}{
		{"wrong bucket", http.MethodPut, "/ambrovia-files/ducklake/main/t/f.parquet", nil, http.StatusForbidden},
		{"outside root", http.MethodPut, "/nimmly-files/corpus/f.parquet", nil, http.StatusForbidden},
		{"encoded traversal", http.MethodPut, "/nimmly-files/ducklake/%2e%2e/f.parquet", nil, http.StatusForbidden},
		{"missing token", http.MethodPut, "/nimmly-files/ducklake/main/t/f.parquet", func(r *http.Request, _ *server) { r.Header.Del("x-amz-security-token") }, http.StatusUnauthorized},
		{"invalid token", http.MethodPut, "/nimmly-files/ducklake/main/t/f.parquet", func(_ *http.Request, s *server) { s.appAuth = rejectingReviewer{} }, http.StatusUnauthorized},
		{"invalid authority", http.MethodPut, "/nimmly-files/ducklake/main/t/f.parquet", func(r *http.Request, _ *server) {
			r.Header.Set("Authorization", lakeAuthorization(WorkspaceStorageScope("bad")))
		}, http.StatusForbidden},
		{"unapproved account", http.MethodPut, "/nimmly-files/ducklake/main/t/f.parquet", func(_ *http.Request, s *server) { s.appAuth = fixedReviewer{namespace: "nimmly", account: "default"} }, http.StatusForbidden},
		{"missing length", http.MethodPut, "/nimmly-files/ducklake/main/t/f.parquet", func(r *http.Request, _ *server) { r.ContentLength = -1 }, http.StatusLengthRequired},
		{"missing hash", http.MethodPut, "/nimmly-files/ducklake/main/t/f.parquet", func(r *http.Request, _ *server) { r.Header.Del("x-amz-content-sha256") }, http.StatusBadRequest},
		{"oversize", http.MethodPut, "/nimmly-files/ducklake/main/t/f.parquet", func(r *http.Request, s *server) { r.ContentLength = s.cfg.DuckLakeMaxPut + 1 }, http.StatusRequestEntityTooLarge},
		{"multipart initiate", http.MethodPost, "/nimmly-files/ducklake/main/t/f.parquet?uploads", nil, http.StatusNotImplemented},
		{"multipart part", http.MethodPut, "/nimmly-files/ducklake/main/t/f.parquet?partNumber=1&uploadId=x", nil, http.StatusNotImplemented},
		{"multipart complete", http.MethodPost, "/nimmly-files/ducklake/main/t/f.parquet?uploadId=x", nil, http.StatusNotImplemented},
		{"multipart abort", http.MethodDelete, "/nimmly-files/ducklake/main/t/f.parquet?uploadId=x", nil, http.StatusNotImplemented},
		{"list objects", http.MethodGet, "/nimmly-files/?list-type=2", nil, http.StatusNotImplemented},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := newLakeMemoryStore()
			backend := newLakeMemoryBackend()
			s := lakeServerForTest(store, backend)
			request := lakePutRequest(test.path, TenantStorageScope(), payload)
			request.Method = test.method
			if test.mutate != nil {
				test.mutate(request, s)
			}
			response := httptest.NewRecorder()
			s.handleLakeS3(response, request)
			if response.Code != test.want {
				t.Fatalf("status = %d, want %d, body=%s", response.Code, test.want, response.Body.String())
			}
		})
	}
}

func TestLakePhysicalBytesCannotBypassGovernedLifecycle(t *testing.T) {
	payload := []byte("unguarded bytes")
	backend := newLakeMemoryBackend()
	backend.objects[lakeMapKey("nimmly-files", "ducklake/main/t/file.parquet")] = payload
	store := newLakeMemoryStore()
	s := lakeServerForTest(store, backend)

	for _, state := range []ObjectState{"", ObjectStateReserved, ObjectStateDeleted} {
		if state != "" {
			object := activeLakeObject(TenantStorageScope(), payload)
			object.Key = "ducklake/main/t/file.parquet"
			object.State = state
			store.objects[lakeMapKey(object.Bucket, object.Key)] = object
		}
		response := httptest.NewRecorder()
		s.handleLakeS3(response, lakeRequest(http.MethodGet,
			"/nimmly-files/ducklake/main/t/file.parquet", TenantStorageScope(), nil))
		if response.Code != http.StatusNotFound {
			t.Fatalf("state %q exposed physical bytes with status %d", state, response.Code)
		}
	}
}

func TestLakeGetFailsClosedOnPhysicalSizeMismatch(t *testing.T) {
	payload := []byte("truncated")
	store := newLakeMemoryStore()
	object := activeLakeObject(TenantStorageScope(), payload)
	object.ByteSize++
	store.objects[lakeMapKey(object.Bucket, object.Key)] = object
	backend := newLakeMemoryBackend()
	backend.objects[lakeMapKey(object.Bucket, object.Key)] = payload
	s := lakeServerForTest(store, backend)
	response := httptest.NewRecorder()

	s.handleLakeS3(response, lakeRequest(http.MethodGet,
		"/"+object.Bucket+"/"+object.Key, TenantStorageScope(), nil))

	if response.Code != http.StatusNotFound || response.Body.String() == string(payload) {
		t.Fatalf("mismatched physical object response = %d %q", response.Code, response.Body.String())
	}
}

type boundedReadProbe struct {
	remaining int64
	maxRead   int
}

func (r *boundedReadProbe) Read(buffer []byte) (int, error) {
	if len(buffer) > r.maxRead {
		r.maxRead = len(buffer)
	}
	if r.remaining == 0 {
		return 0, io.EOF
	}
	if int64(len(buffer)) > r.remaining {
		buffer = buffer[:r.remaining]
	}
	for index := range buffer {
		buffer[index] = 'x'
	}
	r.remaining -= int64(len(buffer))
	return len(buffer), nil
}

func TestLakeMaximumPutUsesBoundedStreamingReads(t *testing.T) {
	const size = int64(96 * 1024 * 1024)
	probe := &boundedReadProbe{remaining: size}
	hasher := sha256.New()
	_, _ = io.Copy(hasher, &boundedReadProbe{remaining: size})
	request := lakeRequest(http.MethodPut, "/nimmly-files/ducklake/main/t/large.parquet", TenantStorageScope(), probe)
	request.ContentLength = size
	request.Header.Set("x-amz-content-sha256", hex.EncodeToString(hasher.Sum(nil)))
	store := newLakeMemoryStore()
	backend := newLakeMemoryBackend()
	backend.discardPut = true
	s := lakeServerForTest(store, backend)
	response := httptest.NewRecorder()
	s.handleLakeS3(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", response.Code, response.Body.String())
	}
	if probe.maxRead > 64*1024 {
		t.Fatalf("largest body read = %d bytes", probe.maxRead)
	}
}

type generatedLakeBackend struct {
	body *boundedReadProbe
	size int64
}

func (b *generatedLakeBackend) Put(context.Context, string, string, io.Reader, int64, string) error {
	return errors.New("unexpected PUT")
}

func (b *generatedLakeBackend) Get(_ context.Context, _, _, byteRange string) (lakeObjectMetadata, error) {
	if byteRange != fmt.Sprintf("bytes=0-%d", b.size-1) {
		return lakeObjectMetadata{}, fmt.Errorf("unexpected range %q", byteRange)
	}
	b.body = &boundedReadProbe{remaining: b.size}
	return lakeObjectMetadata{
		Body: io.NopCloser(b.body), ContentLength: b.size,
		ContentRange: fmt.Sprintf("bytes 0-%d/%d", b.size-1, b.size),
	}, nil
}

func (b *generatedLakeBackend) Head(context.Context, string, string) (lakeObjectMetadata, error) {
	return lakeObjectMetadata{ContentLength: b.size}, nil
}

func (*generatedLakeBackend) ConfirmAbsent(context.Context, string, string) error { return nil }

func (*generatedLakeBackend) Delete(context.Context, string, string) error {
	return errors.New("unexpected DELETE")
}

func (*generatedLakeBackend) DeleteObjects(context.Context, string, []string) ([]lakeDeleteOutcome, error) {
	return nil, errors.New("unexpected DELETE objects")
}

type discardResponseWriter struct {
	header   http.Header
	status   int
	maxWrite int
}

func (w *discardResponseWriter) Header() http.Header { return w.header }

func (w *discardResponseWriter) WriteHeader(status int) { w.status = status }

func (w *discardResponseWriter) Write(payload []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if len(payload) > w.maxWrite {
		w.maxWrite = len(payload)
	}
	return len(payload), nil
}

func TestLakeMaximumRangedGetUsesBoundedStreaming(t *testing.T) {
	const size = int64(96 * 1024 * 1024)
	store := newLakeMemoryStore()
	object := activeLakeObject(TenantStorageScope(), nil)
	object.Key = "ducklake/main/t/large.parquet"
	object.ByteSize = size
	store.objects[lakeMapKey(object.Bucket, object.Key)] = object
	backend := &generatedLakeBackend{size: size}
	s := lakeServerForTest(store, backend)
	request := lakeRequest(http.MethodGet, "/"+object.Bucket+"/"+object.Key, TenantStorageScope(), nil)
	request.Header.Set("Range", fmt.Sprintf("bytes=0-%d", size-1))
	response := &discardResponseWriter{header: make(http.Header)}

	s.handleLakeS3(response, request)

	if response.status != http.StatusPartialContent {
		t.Fatalf("status = %d", response.status)
	}
	if backend.body == nil || backend.body.maxRead > 64*1024 || response.maxWrite > 64*1024 {
		t.Fatalf("unbounded stream: backend read=%v response write=%d", backend.body, response.maxWrite)
	}
}

func TestLakeRequestsDoNotDependOnReplicaLocalSessionState(t *testing.T) {
	payload := []byte("shared durable bytes")
	store := newLakeMemoryStore()
	backend := newLakeMemoryBackend()
	firstReplica := lakeServerForTest(store, backend)
	secondReplica := lakeServerForTest(store, backend)
	path := "/nimmly-files/ducklake/main/t/replicated.parquet"

	put := httptest.NewRecorder()
	firstReplica.handleLakeS3(put, lakePutRequest(path, TenantStorageScope(), payload))
	get := httptest.NewRecorder()
	secondReplica.handleLakeS3(get, lakeRequest(http.MethodGet, path, TenantStorageScope(), nil))

	if put.Code != http.StatusOK || get.Code != http.StatusOK || !bytes.Equal(get.Body.Bytes(), payload) {
		t.Fatalf("cross-replica PUT=%d GET=%d body=%q", put.Code, get.Code, get.Body.Bytes())
	}
}
