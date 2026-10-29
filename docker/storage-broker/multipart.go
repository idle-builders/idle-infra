package main

// Multipart upload support.
//
// The AWS SDK (service/s3 v1.104.0) can only PRESIGN UploadPart — the three
// control-plane operations (CreateMultipartUpload, CompleteMultipartUpload,
// AbortMultipartUpload) are not presignable. So the broker splits the upload:
//
//   - Control plane (create/complete/abort): tiny calls the broker executes
//     server-side with its own credential, each gated by the same policy as
//     /v1/presign (namespace -> bucket, key within an allowed prefix, op
//     "multipart" allowed).
//   - Data plane (the part bodies): the client PUTs each part DIRECTLY to a
//     short-TTL presigned UploadPart URL, so the bytes never flow through the
//     broker. This keeps the broker lightweight and the client's memory flat
//     (~one part in flight), which is the whole point of streaming large
//     objects (e.g. 100MB+ E-Periodica OCR TXT) instead of buffering them.
//
// The client is responsible for calling abort on any failure so no orphaned
// parts are billed; a hard client crash mid-upload can still orphan parts, so
// a bucket lifecycle rule that aborts incomplete uploads is recommended as a
// backstop (see docs/plans/presign-storage-broker.md).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// maxCompleteBodyBytes bounds the /v1/multipart/complete body. It carries a
// parts list ({partNumber, etag} ~60 bytes each); 1 MiB covers well beyond the
// S3 maximum of 10,000 parts. All other multipart bodies are tiny and use the
// standard cfg.MaxBodyBytes cap.
const maxCompleteBodyBytes = 1 << 20

// CompletedPartInput is one client-reported part (its number and S3 ETag).
type CompletedPartInput struct {
	PartNumber int32
	ETag       string
}

// CreateMultipart opens a multipart upload and returns the S3 upload id.
func (p *Presigner) CreateMultipart(ctx context.Context, bucket, key, contentType string) (string, error) {
	in := &s3.CreateMultipartUploadInput{Bucket: &bucket, Key: &key}
	if contentType != "" {
		in.ContentType = &contentType
	}
	out, err := p.s3.CreateMultipartUpload(ctx, in)
	if err != nil {
		return "", err
	}
	if out.UploadId == nil {
		return "", fmt.Errorf("s3 returned a nil upload id")
	}
	return *out.UploadId, nil
}

// PresignPart returns a short-TTL presigned PUT URL for a single part.
func (p *Presigner) PresignPart(ctx context.Context, bucket, key, uploadID string, partNumber int32, ttl time.Duration) (*PresignResult, error) {
	r, err := p.client.PresignUploadPart(ctx, &s3.UploadPartInput{
		Bucket:     &bucket,
		Key:        &key,
		UploadId:   &uploadID,
		PartNumber: aws.Int32(partNumber),
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return nil, err
	}
	return &PresignResult{URL: r.URL, Method: r.Method, ExpiresAt: time.Now().Add(ttl)}, nil
}

// CompleteMultipart finalizes the upload from the client-reported part etags.
func (p *Presigner) CompleteMultipart(ctx context.Context, bucket, key, uploadID string, parts []CompletedPartInput) error {
	completed := make([]types.CompletedPart, 0, len(parts))
	for _, part := range parts {
		etag := part.ETag
		num := part.PartNumber
		completed = append(completed, types.CompletedPart{ETag: &etag, PartNumber: aws.Int32(num)})
	}
	_, err := p.s3.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:          &bucket,
		Key:             &key,
		UploadId:        &uploadID,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: completed},
	})
	return err
}

// AbortMultipart discards an in-progress upload so its parts are not billed.
func (p *Presigner) AbortMultipart(ctx context.Context, bucket, key, uploadID string) error {
	_, err := p.s3.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
		Bucket:   &bucket,
		Key:      &key,
		UploadId: &uploadID,
	})
	return err
}

// --- HTTP layer ---------------------------------------------------------------

type mpCreateRequest struct {
	Key         string `json:"key"`
	ContentType string `json:"contentType,omitempty"`
}
type mpCreateResponse struct {
	UploadID string `json:"uploadId"`
	Key      string `json:"key"`
}

type mpPartRequest struct {
	Key        string `json:"key"`
	UploadID   string `json:"uploadId"`
	PartNumber int32  `json:"partNumber"`
}
type mpPartResponse struct {
	URL       string `json:"url"`
	Method    string `json:"method"`
	ExpiresAt string `json:"expiresAt"`
}

type mpCompletePart struct {
	PartNumber int32  `json:"partNumber"`
	ETag       string `json:"etag"`
}
type mpCompleteRequest struct {
	Key      string           `json:"key"`
	UploadID string           `json:"uploadId"`
	Parts    []mpCompletePart `json:"parts"`
}

type mpAbortRequest struct {
	Key      string `json:"key"`
	UploadID string `json:"uploadId"`
}

type mpOKResponse struct {
	OK  bool   `json:"ok"`
	Key string `json:"key,omitempty"`
}

// authContext carries the verified identity + policy decision for a request.
type authContext struct {
	namespace string
	sa        string
	sub       string
	decision  *Decision
}

// decodeJSON reads a bounded, strict JSON body. On failure it writes a 400 and
// returns false.
func decodeJSON(w http.ResponseWriter, r *http.Request, maxBytes int64, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody("invalid request body"))
		return false
	}
	return true
}

// authorizeMultipart runs the same identity + policy chain as handlePresign,
// with the op fixed to "multipart". On denial it writes the response and
// returns ok=false. All multipart control-plane endpoints gate on this.
func (s *server) authorizeMultipart(w http.ResponseWriter, r *http.Request, key string) (*authContext, bool) {
	ctx := r.Context()

	appTok := bearer(r.Header.Get("Authorization"))
	if appTok == "" {
		writeJSON(w, http.StatusUnauthorized, errorBody("missing app token"))
		return nil, false
	}
	namespace, sa, err := s.appAuth.Review(ctx, appTok)
	if err != nil {
		s.log.Warn("app auth failed", "err", err.Error())
		writeJSON(w, http.StatusUnauthorized, errorBody("invalid app token"))
		return nil, false
	}

	var sub string
	if userTok := bearer(r.Header.Get("X-User-Authorization")); userTok != "" {
		sub, err = s.userAuth.Verify(ctx, userTok, s.policy.UserTokenClients(namespace))
		if err != nil {
			s.log.Warn("user auth failed", "namespace", namespace, "err", err.Error())
			writeJSON(w, http.StatusUnauthorized, errorBody("invalid user token"))
			return nil, false
		}
	}

	decision, err := s.policy.Authorize(namespace, "multipart", key, sub)
	if err != nil {
		reason := "denied"
		var de *DenyError
		if errors.As(err, &de) {
			reason = de.Reason
		}
		s.log.Info("grant denied",
			"namespace", namespace, "sa", sa, "sub", sub, "op", "multipart", "key", key, "reason", reason)
		writeJSON(w, http.StatusForbidden, errorBody(reason))
		return nil, false
	}
	if err := s.governedStore.ClaimBucketOwnership(ctx, namespace, decision.Bucket); err != nil {
		s.log.Error("bucket ownership rejected", "namespace", namespace, "bucket", decision.Bucket, "err", err)
		if errors.Is(err, ErrGovernedConflict) {
			writeJSON(w, http.StatusConflict, errorBody("bucket ownership conflict"))
		} else {
			writeJSON(w, http.StatusInternalServerError, errorBody("storage authorization unavailable"))
		}
		return nil, false
	}
	return &authContext{namespace: namespace, sa: sa, sub: sub, decision: decision}, true
}

func (s *server) handleMultipartCreate(w http.ResponseWriter, r *http.Request) {
	var req mpCreateRequest
	if !decodeJSON(w, r, s.cfg.MaxBodyBytes, &req) {
		return
	}
	ac, ok := s.authorizeMultipart(w, r, req.Key)
	if !ok {
		return
	}
	uploadID, err := s.presigner.CreateMultipart(r.Context(), ac.decision.Bucket, req.Key, req.ContentType)
	if err != nil {
		s.log.Error("multipart create failed", "namespace", ac.namespace, "key", req.Key, "err", err.Error())
		writeJSON(w, http.StatusInternalServerError, errorBody("multipart create failed"))
		return
	}
	s.log.Info("multipart create",
		"namespace", ac.namespace, "sa", ac.sa, "bucket", ac.decision.Bucket, "key", req.Key)
	writeJSON(w, http.StatusOK, mpCreateResponse{UploadID: uploadID, Key: req.Key})
}

func (s *server) handleMultipartPart(w http.ResponseWriter, r *http.Request) {
	var req mpPartRequest
	if !decodeJSON(w, r, s.cfg.MaxBodyBytes, &req) {
		return
	}
	if req.UploadID == "" {
		writeJSON(w, http.StatusBadRequest, errorBody("missing uploadId"))
		return
	}
	if req.PartNumber < 1 || req.PartNumber > 10000 {
		writeJSON(w, http.StatusBadRequest, errorBody("partNumber out of range (1..10000)"))
		return
	}
	ac, ok := s.authorizeMultipart(w, r, req.Key)
	if !ok {
		return
	}
	res, err := s.presigner.PresignPart(r.Context(), ac.decision.Bucket, req.Key, req.UploadID, req.PartNumber,
		time.Duration(ac.decision.TTLSeconds)*time.Second)
	if err != nil {
		s.log.Error("multipart presign part failed", "namespace", ac.namespace, "key", req.Key, "err", err.Error())
		writeJSON(w, http.StatusInternalServerError, errorBody("multipart presign part failed"))
		return
	}
	writeJSON(w, http.StatusOK, mpPartResponse{
		URL:       res.URL,
		Method:    res.Method,
		ExpiresAt: res.ExpiresAt.UTC().Format(time.RFC3339),
	})
}

func (s *server) handleMultipartComplete(w http.ResponseWriter, r *http.Request) {
	var req mpCompleteRequest
	if !decodeJSON(w, r, maxCompleteBodyBytes, &req) {
		return
	}
	if req.UploadID == "" {
		writeJSON(w, http.StatusBadRequest, errorBody("missing uploadId"))
		return
	}
	if len(req.Parts) == 0 {
		writeJSON(w, http.StatusBadRequest, errorBody("no parts"))
		return
	}
	ac, ok := s.authorizeMultipart(w, r, req.Key)
	if !ok {
		return
	}
	parts := make([]CompletedPartInput, 0, len(req.Parts))
	for _, part := range req.Parts {
		parts = append(parts, CompletedPartInput{PartNumber: part.PartNumber, ETag: part.ETag})
	}
	if err := s.presigner.CompleteMultipart(r.Context(), ac.decision.Bucket, req.Key, req.UploadID, parts); err != nil {
		s.log.Error("multipart complete failed", "namespace", ac.namespace, "key", req.Key, "err", err.Error())
		writeJSON(w, http.StatusInternalServerError, errorBody("multipart complete failed"))
		return
	}
	s.log.Info("multipart complete",
		"namespace", ac.namespace, "sa", ac.sa, "bucket", ac.decision.Bucket, "key", req.Key, "parts", len(parts))
	writeJSON(w, http.StatusOK, mpOKResponse{OK: true, Key: req.Key})
}

func (s *server) handleMultipartAbort(w http.ResponseWriter, r *http.Request) {
	var req mpAbortRequest
	if !decodeJSON(w, r, s.cfg.MaxBodyBytes, &req) {
		return
	}
	if req.UploadID == "" {
		writeJSON(w, http.StatusBadRequest, errorBody("missing uploadId"))
		return
	}
	ac, ok := s.authorizeMultipart(w, r, req.Key)
	if !ok {
		return
	}
	if err := s.presigner.AbortMultipart(r.Context(), ac.decision.Bucket, req.Key, req.UploadID); err != nil {
		s.log.Error("multipart abort failed", "namespace", ac.namespace, "key", req.Key, "err", err.Error())
		writeJSON(w, http.StatusInternalServerError, errorBody("multipart abort failed"))
		return
	}
	s.log.Info("multipart abort",
		"namespace", ac.namespace, "sa", ac.sa, "bucket", ac.decision.Bucket, "key", req.Key)
	writeJSON(w, http.StatusOK, mpOKResponse{OK: true})
}
