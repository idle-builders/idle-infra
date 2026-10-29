package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

var ErrObjectMismatch = errors.New("stored object does not match declaration")

// Presigner produces short-TTL SigV4 presigned URLs for a single object/op
// against the Infomaniak S3 gateway. The S3 credential lives only here.
//
// Infomaniak's gateway needs path-style addressing and rejects flexible/chunked
// checksums, so checksum calculation/validation is forced to WHEN_REQUIRED — the
// same reason the OpenTofu S3 state backend sets skip_s3_checksum. Without this a
// plain client PUT to the presigned URL fails with "aws-chunked ... not supported".
type Presigner struct {
	client *s3.PresignClient
	// s3 is the same underlying client, kept for the multipart control-plane calls
	// (create/complete/abort) that the SDK cannot presign — the broker executes
	// those server-side with its credential. See multipart.go.
	s3 *s3.Client
}

func (p *Presigner) VerifyObject(
	ctx context.Context,
	bucket, key, contentHash string,
	byteSize int64,
) error {
	if bucket == "" || key == "" || !contentHashPattern.MatchString(contentHash) ||
		byteSize < 0 || byteSize == int64(^uint64(0)>>1) {
		return fmt.Errorf("invalid object verification declaration")
	}
	response, err := p.s3.GetObject(ctx, &s3.GetObjectInput{Bucket: &bucket, Key: &key})
	if err != nil {
		return fmt.Errorf("get object for verification: %w", err)
	}
	defer response.Body.Close()
	if response.ContentLength != nil && *response.ContentLength != byteSize {
		return fmt.Errorf("%w: byte size", ErrObjectMismatch)
	}
	hasher := sha256.New()
	written, err := io.Copy(hasher, io.LimitReader(response.Body, byteSize+1))
	if err != nil {
		return fmt.Errorf("read object for verification: %w", err)
	}
	if written != byteSize || hex.EncodeToString(hasher.Sum(nil)) != contentHash {
		return fmt.Errorf("%w: content", ErrObjectMismatch)
	}
	return nil
}

func (p *Presigner) DeleteObject(ctx context.Context, bucket, key string) error {
	_, err := p.s3.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &bucket, Key: &key})
	if err != nil {
		return fmt.Errorf("delete object: %w", err)
	}
	return nil
}

func (p *Presigner) DeleteAndConfirmAbsent(ctx context.Context, bucket, key string) error {
	if err := p.DeleteObject(ctx, bucket, key); err != nil {
		return err
	}
	if _, err := p.s3.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &bucket, Key: &key}); err == nil {
		return fmt.Errorf("object remains after delete")
	} else if !objectNotFound(err) {
		return fmt.Errorf("confirm object deletion: %w", err)
	}
	return nil
}

func (p *Presigner) WalkKeys(
	ctx context.Context,
	bucket, prefix string,
	visit func(string) error,
) error {
	paginator := s3.NewListObjectsV2Paginator(p.s3, &s3.ListObjectsV2Input{
		Bucket:  &bucket,
		Prefix:  &prefix,
		MaxKeys: aws.Int32(100),
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("list staged objects: %w", err)
		}
		for _, object := range page.Contents {
			if object.Key != nil {
				if err := visit(*object.Key); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func objectNotFound(err error) bool {
	var notFound *types.NotFound
	if errors.As(err, &notFound) {
		return true
	}
	var apiError smithy.APIError
	if errors.As(err, &apiError) {
		switch apiError.ErrorCode() {
		case "NoSuchKey", "NotFound", "404":
			return true
		}
	}
	var responseError *smithyhttp.ResponseError
	return errors.As(err, &responseError) && responseError.HTTPStatusCode() == http.StatusNotFound
}

// @lore Infomaniak Swift rejects conditional PutObject, so a presigned upload
// cannot itself become an immutable governed object. Uploads land at a unique
// staging key; only the broker copies them to a never-presigned final key.
func (p *Presigner) PromoteObject(
	ctx context.Context,
	bucket, sourceKey, targetKey, contentHash string,
	byteSize int64,
) error {
	copySource := url.PathEscape(bucket + "/" + sourceKey)
	if _, err := p.s3.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket: &bucket, Key: &targetKey, CopySource: &copySource,
	}); err != nil {
		return fmt.Errorf("copy staged object: %w", err)
	}
	if err := p.VerifyObject(ctx, bucket, targetKey, contentHash, byteSize); err != nil {
		_ = p.DeleteObject(ctx, bucket, targetKey)
		return fmt.Errorf("verify promoted object: %w", err)
	}
	return nil
}

func NewPresigner(c *Config) *Presigner {
	s3client := s3.New(s3.Options{
		Region:                     c.S3Region,
		Credentials:                credentials.NewStaticCredentialsProvider(c.S3AccessKey, c.S3SecretKey, ""),
		BaseEndpoint:               aws.String(c.S3Endpoint),
		UsePathStyle:               true,
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	})
	return &Presigner{client: s3.NewPresignClient(s3client), s3: s3client}
}

type PresignResult struct {
	URL       string
	Method    string
	ExpiresAt time.Time
	Headers   map[string]string
}

func (p *Presigner) Presign(ctx context.Context, op, bucket, key, contentType string, ttl time.Duration) (*PresignResult, error) {
	expire := s3.WithPresignExpires(ttl)

	var url, method string
	var signedHeaders http.Header
	switch op {
	case "get":
		r, err := p.client.PresignGetObject(ctx, &s3.GetObjectInput{Bucket: &bucket, Key: &key}, expire)
		if err != nil {
			return nil, err
		}
		url, method, signedHeaders = r.URL, r.Method, r.SignedHeader
	case "put":
		in := &s3.PutObjectInput{Bucket: &bucket, Key: &key}
		if contentType != "" {
			in.ContentType = &contentType
		}
		r, err := p.client.PresignPutObject(ctx, in, expire)
		if err != nil {
			return nil, err
		}
		url, method, signedHeaders = r.URL, r.Method, r.SignedHeader
	case "delete":
		r, err := p.client.PresignDeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &bucket, Key: &key}, expire)
		if err != nil {
			return nil, err
		}
		url, method, signedHeaders = r.URL, r.Method, r.SignedHeader
	default:
		return nil, fmt.Errorf("unsupported op %q", op)
	}

	headers := make(map[string]string, len(signedHeaders))
	for name, values := range signedHeaders {
		if len(values) > 0 {
			headers[name] = values[0]
		}
	}
	return &PresignResult{URL: url, Method: method, ExpiresAt: time.Now().Add(ttl), Headers: headers}, nil
}
