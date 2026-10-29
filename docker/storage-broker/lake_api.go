package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	lakeRangePattern        = regexp.MustCompile(`^bytes=(?:[0-9]+-[0-9]*|-[0-9]+)$`)
	lakeContentRangePattern = regexp.MustCompile(`^bytes ([0-9]+)-([0-9]+)/([0-9]+)$`)
	lakeBucketPattern       = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,253}[a-z0-9]$`)
)

type s3ErrorResponse struct {
	XMLName xml.Name `xml:"Error"`
	Code    string   `xml:"Code"`
	Message string   `xml:"Message"`
}

type countingHashReader struct {
	reader io.Reader
	hash   hashWriter
	count  int64
}

type hashWriter interface {
	Write([]byte) (int, error)
	Sum([]byte) []byte
}

func (r *countingHashReader) Read(buffer []byte) (int, error) {
	read, err := r.reader.Read(buffer)
	if read > 0 {
		_, _ = r.hash.Write(buffer[:read])
		r.count += int64(read)
	}
	return read, err
}

type idleDeadlineReader struct {
	reader     io.ReadCloser
	controller *http.ResponseController
	timeout    time.Duration
}

func (r *idleDeadlineReader) Read(buffer []byte) (int, error) {
	_ = r.controller.SetReadDeadline(time.Now().Add(r.timeout))
	return r.reader.Read(buffer)
}

func (r *idleDeadlineReader) Close() error { return r.reader.Close() }

type idleDeadlineWriter struct {
	writer     http.ResponseWriter
	controller *http.ResponseController
	timeout    time.Duration
}

func (w *idleDeadlineWriter) Header() http.Header    { return w.writer.Header() }
func (w *idleDeadlineWriter) WriteHeader(status int) { w.writer.WriteHeader(status) }
func (w *idleDeadlineWriter) Write(buffer []byte) (int, error) {
	_ = w.controller.SetWriteDeadline(time.Now().Add(w.timeout))
	return w.writer.Write(buffer)
}

func validDuckLakeRoot(root string) error {
	if !strings.HasSuffix(root, "/") {
		return fmt.Errorf("root must end with /")
	}
	return validKey(strings.TrimSuffix(root, "/") + "/placeholder")
}

func parseLakeLocator(r *http.Request) (bucket, key string, err error) {
	path, err := url.PathUnescape(r.URL.EscapedPath())
	if err != nil || !strings.HasPrefix(path, "/") {
		return "", "", fmt.Errorf("invalid path")
	}
	parts := strings.SplitN(strings.TrimPrefix(path, "/"), "/", 2)
	if len(parts) != 2 || parts[0] == "" {
		return "", "", fmt.Errorf("object path required")
	}
	if !kubernetesName.MatchString(parts[0]) && !lakeBucketPattern.MatchString(parts[0]) {
		return "", "", fmt.Errorf("invalid bucket")
	}
	if err := validKey(parts[1]); err != nil {
		return "", "", err
	}
	return parts[0], parts[1], nil
}

func parseLakeAuthority(authorization string) (StorageScope, error) {
	if !strings.HasPrefix(authorization, "AWS4-HMAC-SHA256 ") {
		return StorageScope{}, fmt.Errorf("invalid authorization")
	}
	fields := strings.Split(strings.TrimPrefix(authorization, "AWS4-HMAC-SHA256 "), ",")
	var credential string
	for _, field := range fields {
		name, value, ok := strings.Cut(strings.TrimSpace(field), "=")
		if ok && name == "Credential" {
			credential = value
			break
		}
	}
	accessKey, _, ok := strings.Cut(credential, "/")
	if !ok {
		return StorageScope{}, fmt.Errorf("invalid credential")
	}
	switch {
	case accessKey == "tenant":
		return TenantStorageScope(), nil
	case strings.HasPrefix(accessKey, "workspace-"):
		scope := WorkspaceStorageScope(strings.TrimPrefix(accessKey, "workspace-"))
		return scope, scope.Validate()
	default:
		return StorageScope{}, fmt.Errorf("invalid authority")
	}
}

func (s *server) handleLakeS3(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.LakeOperationTTL)
	defer cancel()
	r = r.WithContext(ctx)
	controller := http.NewResponseController(w)
	if r.Body != nil {
		r.Body = &idleDeadlineReader{reader: r.Body, controller: controller, timeout: s.cfg.LakeIOIdleTTL}
	}

	if !s.acquireLakeSlot(w, r) {
		return
	}
	defer func() { <-s.lakeSlots }()

	if r.URL.Query().Has("uploads") || r.URL.Query().Has("uploadId") {
		writeS3Error(w, http.StatusNotImplemented, "NotImplemented", "Governed multipart upload is not supported")
		return
	}
	if r.Method == http.MethodGet && (r.URL.Query().Get("list-type") != "" || strings.HasSuffix(r.URL.Path, "/")) {
		writeS3Error(w, http.StatusNotImplemented, "NotImplemented", "Object listing is not supported")
		return
	}
	deleteObjects := isLakeDeleteObjectsRequest(r)
	var bucket, key string
	var err error
	if deleteObjects {
		bucket, err = parseLakeDeleteBucket(r)
	} else {
		bucket, key, err = parseLakeLocator(r)
	}
	if err != nil || (!deleteObjects && !strings.HasPrefix(key, s.cfg.DuckLakeRoot)) {
		writeS3Error(w, http.StatusForbidden, "AccessDenied", "Access denied")
		return
	}
	token := r.Header.Get("x-amz-security-token")
	if token == "" {
		writeS3Error(w, http.StatusUnauthorized, "InvalidToken", "Missing session token")
		return
	}
	tenant, account, err := s.appAuth.Review(ctx, token)
	if err != nil {
		writeS3Error(w, http.StatusUnauthorized, "InvalidToken", "Invalid session token")
		return
	}
	expectedBucket, ok := s.tenantBucketS3(tenant)
	if !ok || bucket != expectedBucket {
		writeS3Error(w, http.StatusForbidden, "AccessDenied", "Access denied")
		return
	}
	scope, err := parseLakeAuthority(r.Header.Get("Authorization"))
	if err != nil {
		writeS3Error(w, http.StatusForbidden, "AccessDenied", "Access denied")
		return
	}
	if deleteObjects {
		s.handleLakeDeleteObjects(w, r, tenant, account, scope, bucket)
		return
	}

	switch r.Method {
	case http.MethodPut:
		s.handleLakePut(w, r, tenant, account, scope, bucket, key)
	case http.MethodGet:
		s.handleLakeGet(w, r, tenant, account, scope, bucket, key, controller)
	case http.MethodHead:
		s.handleLakeHead(w, r, tenant, account, scope, bucket, key)
	case http.MethodDelete:
		s.handleLakeDelete(w, r, tenant, account, scope, bucket, key)
	default:
		w.Header().Set("Allow", "PUT, GET, HEAD, DELETE")
		writeS3Error(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "Method not allowed")
	}
}

func (s *server) acquireLakeSlot(w http.ResponseWriter, r *http.Request) bool {
	select {
	case s.lakeSlots <- struct{}{}:
		return true
	case <-r.Context().Done():
		writeS3Error(w, http.StatusServiceUnavailable, "ServiceUnavailable", "Request capacity unavailable")
		return false
	}
}

func (s *server) authorizeLake(w http.ResponseWriter, tenant, account string, operation GovernedOperation) bool {
	if err := s.governedPolicy.Authorize(tenant, account, operation); err != nil {
		s.log.Info("lake operation denied", "tenant", tenant, "service_account", account,
			"operation", operation, "reason", err.Error())
		writeS3Error(w, http.StatusForbidden, "AccessDenied", "Access denied")
		return false
	}
	return true
}

func (s *server) handleLakePut(
	w http.ResponseWriter,
	r *http.Request,
	tenant, account string,
	scope StorageScope,
	bucket, key string,
) {
	if !s.authorizeLake(w, tenant, account, OperationLakeWrite) {
		return
	}
	if r.ContentLength < 0 {
		writeS3Error(w, http.StatusLengthRequired, "MissingContentLength", "Content-Length is required")
		return
	}
	if r.ContentLength > s.cfg.DuckLakeMaxPut {
		writeS3Error(w, http.StatusRequestEntityTooLarge, "EntityTooLarge", "Object exceeds the single-PUT limit")
		return
	}
	declaredHash := strings.ToLower(r.Header.Get("x-amz-content-sha256"))
	if !contentHashPattern.MatchString(declaredHash) {
		writeS3Error(w, http.StatusBadRequest, "InvalidRequest", "A hexadecimal SHA-256 payload declaration is required")
		return
	}
	unlock, err := s.governedStore.AcquireLakeRegistrationLock(r.Context(), tenant, bucket, key)
	if err != nil {
		s.writeLakeStoreError(w, err)
		return
	}
	defer unlock()
	if err := s.governedStore.VerifyBucketRegistration(r.Context(), tenant, bucket); err != nil {
		s.writeLakeStoreError(w, err)
		return
	}
	mediaType := r.Header.Get("Content-Type")
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	object, err := s.governedStore.ReserveLakeObject(r.Context(), RegisterActiveObjectInput{
		Tenant: tenant, Scope: scope, ArtifactClass: ArtifactDuckLakeData,
		ContentHash: declaredHash, ByteSize: r.ContentLength, MediaType: mediaType,
		Bucket: bucket, Key: key,
	})
	if err != nil {
		s.writeLakeStoreError(w, err)
		return
	}
	hasher := sha256.New()
	observed := &countingHashReader{reader: io.LimitReader(r.Body, r.ContentLength+1), hash: hasher}
	if object.State == ObjectStateActive {
		if _, err := io.Copy(io.Discard, observed); err != nil || observed.count != r.ContentLength ||
			hex.EncodeToString(hasher.Sum(nil)) != declaredHash {
			writeS3Error(w, http.StatusBadRequest, "BadDigest", "Payload does not match its declaration")
			return
		}
		metadata, err := s.lakeBackend.Head(r.Context(), bucket, key)
		if err != nil || metadata.ContentLength != object.ByteSize {
			writeS3Error(w, http.StatusInternalServerError, "InternalError", "Active object verification failed")
			return
		}
		writeLakeSuccess(w, object)
		return
	}
	// @lore DuckLake catalog-persisted key: exact-path reserve/activate; root placement-only, never authority.
	if err := s.lakeBackend.Put(r.Context(), bucket, key, observed, r.ContentLength, mediaType); err != nil {
		s.cleanupRejectedLakeObject(r.Context(), tenant, object.ID, bucket, key)
		writeS3Error(w, http.StatusBadGateway, "InternalError", "Object storage write failed")
		return
	}
	if observed.count != r.ContentLength || hex.EncodeToString(hasher.Sum(nil)) != declaredHash {
		s.cleanupRejectedLakeObject(r.Context(), tenant, object.ID, bucket, key)
		writeS3Error(w, http.StatusBadRequest, "BadDigest", "Payload does not match its declaration")
		return
	}
	object, err = s.governedStore.ActivateLakeObject(r.Context(), tenant, object.ID)
	if err != nil {
		writeS3Error(w, http.StatusInternalServerError, "InternalError", "Object activation failed")
		return
	}
	writeLakeSuccess(w, object)
}

func (s *server) handleLakeGet(
	w http.ResponseWriter,
	r *http.Request,
	tenant, account string,
	scope StorageScope,
	bucket, key string,
	controller *http.ResponseController,
) {
	if !s.authorizeLake(w, tenant, account, OperationLakeRead) {
		return
	}
	byteRange := r.Header.Get("Range")
	if byteRange != "" && !lakeRangePattern.MatchString(byteRange) {
		writeS3Error(w, http.StatusRequestedRangeNotSatisfiable, "InvalidRange", "Invalid byte range")
		return
	}
	object, err := s.governedStore.ReadLakeObject(r.Context(), tenant, scope, bucket, key)
	if err != nil {
		s.writeLakeReadError(w, err)
		return
	}
	metadata, err := s.lakeBackend.Get(r.Context(), bucket, key, byteRange)
	if err != nil {
		writeS3Error(w, http.StatusNotFound, "NoSuchKey", "Object unavailable")
		return
	}
	defer metadata.Body.Close()
	if byteRange == "" && metadata.ContentLength != object.ByteSize {
		s.log.Error("lake object size mismatch", "tenant", tenant, "object_id", object.ID)
		writeS3Error(w, http.StatusNotFound, "NoSuchKey", "Object unavailable")
		return
	}
	setLakeHeaders(w.Header(), object, metadata)
	status := http.StatusOK
	if byteRange != "" {
		if !validLakeContentRange(metadata.ContentRange, metadata.ContentLength, object.ByteSize) {
			writeS3Error(w, http.StatusBadGateway, "InternalError", "Object storage omitted Content-Range")
			return
		}
		status = http.StatusPartialContent
	}
	w.WriteHeader(status)
	idleWriter := &idleDeadlineWriter{writer: w, controller: controller, timeout: s.cfg.LakeIOIdleTTL}
	if _, err := io.Copy(idleWriter, metadata.Body); err != nil {
		s.log.Warn("lake response interrupted", "tenant", tenant, "object_id", object.ID, "err", err.Error())
	}
}

func validLakeContentRange(value string, contentLength, objectSize int64) bool {
	matches := lakeContentRangePattern.FindStringSubmatch(value)
	if len(matches) != 4 {
		return false
	}
	start, startErr := strconv.ParseInt(matches[1], 10, 64)
	end, endErr := strconv.ParseInt(matches[2], 10, 64)
	total, totalErr := strconv.ParseInt(matches[3], 10, 64)
	return startErr == nil && endErr == nil && totalErr == nil && start >= 0 &&
		end >= start && total == objectSize && contentLength == end-start+1
}

func (s *server) handleLakeHead(
	w http.ResponseWriter,
	r *http.Request,
	tenant, account string,
	scope StorageScope,
	bucket, key string,
) {
	if !s.authorizeLake(w, tenant, account, OperationLakeRead) {
		return
	}
	object, err := s.governedStore.ReadLakeObject(r.Context(), tenant, scope, bucket, key)
	if err != nil {
		s.writeLakeReadError(w, err)
		return
	}
	metadata, err := s.lakeBackend.Head(r.Context(), bucket, key)
	if err != nil || metadata.ContentLength != object.ByteSize {
		writeS3Error(w, http.StatusNotFound, "NoSuchKey", "Object unavailable")
		return
	}
	setLakeHeaders(w.Header(), object, metadata)
	w.WriteHeader(http.StatusOK)
}

func (s *server) handleLakeDelete(
	w http.ResponseWriter,
	r *http.Request,
	tenant, account string,
	scope StorageScope,
	bucket, key string,
) {
	if !s.authorizeLake(w, tenant, account, OperationLakeDelete) {
		return
	}
	unlock, err := s.governedStore.AcquireLakePathLock(r.Context(), tenant, bucket, key)
	if err != nil {
		s.writeLakeStoreError(w, err)
		return
	}
	defer unlock()
	object, err := s.governedStore.DeleteLakeObject(r.Context(), tenant, scope, bucket, key, time.Now())
	if err != nil {
		s.writeLakeReadError(w, err)
		return
	}
	if err := s.lakeBackend.Delete(r.Context(), bucket, key); err != nil {
		s.log.Error("lake physical delete failed", "tenant", tenant, "object_id", object.ID, "err", err.Error())
		writeS3Error(w, http.StatusBadGateway, "InternalError", "Object storage delete failed")
		return
	}
	if err := s.lakeBackend.ConfirmAbsent(r.Context(), bucket, key); err != nil {
		s.log.Error("lake physical delete remains unconfirmed", "tenant", tenant,
			"object_id", object.ID, "err", err.Error())
		writeS3Error(w, http.StatusBadGateway, "InternalError", "Object storage delete remains unconfirmed")
		return
	}
	if err := s.governedStore.ConfirmLakeObjectRemoval(r.Context(), tenant, scope, bucket, key,
		time.Now()); err != nil {
		s.writeLakeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) cleanupRejectedLakeObject(ctx context.Context, tenant, objectID, bucket, key string) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err := s.lakeBackend.Delete(cleanupCtx, bucket, key); err != nil {
		s.log.Error("delete rejected lake upload", "tenant", tenant, "object_id", objectID, "err", err.Error())
	}
}

func (s *server) tenantBucketS3(tenant string) (string, bool) {
	policy, ok := s.policy.Namespaces[tenant]
	return policy.Bucket, ok && policy.Bucket != ""
}

func (s *server) writeLakeReadError(w http.ResponseWriter, err error) {
	if !errors.Is(err, ErrGovernedDenied) {
		s.log.Error("lake governed lookup failed", "err", err.Error())
	}
	writeS3Error(w, http.StatusNotFound, "NoSuchKey", "Object unavailable")
}

func (s *server) writeLakeStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrGovernedDenied):
		writeS3Error(w, http.StatusForbidden, "AccessDenied", "Access denied")
	case errors.Is(err, ErrGovernedConflict):
		writeS3Error(w, http.StatusConflict, "OperationAborted", "Object path is already governed by another declaration")
	default:
		s.log.Error("lake governed operation failed", "err", err.Error())
		writeS3Error(w, http.StatusInternalServerError, "InternalError", "Governed storage unavailable")
	}
}

func setLakeHeaders(headers http.Header, object GovernedObject, metadata lakeObjectMetadata) {
	headers.Set("Accept-Ranges", "bytes")
	headers.Set("Content-Length", strconv.FormatInt(metadata.ContentLength, 10))
	headers.Set("Content-Type", object.MediaType)
	headers.Set("ETag", `"`+object.ContentHash+`"`)
	if metadata.ContentRange != "" {
		headers.Set("Content-Range", metadata.ContentRange)
	}
	if !metadata.LastModified.IsZero() {
		headers.Set("Last-Modified", metadata.LastModified.UTC().Format(http.TimeFormat))
	}
}

func writeLakeSuccess(w http.ResponseWriter, object GovernedObject) {
	w.Header().Set("ETag", `"`+object.ContentHash+`"`)
	w.WriteHeader(http.StatusOK)
}

func writeS3Error(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_ = xml.NewEncoder(w).Encode(s3ErrorResponse{Code: code, Message: message})
}
