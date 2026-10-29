package main

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsv4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

type lakeObjectMetadata struct {
	Body          io.ReadCloser
	ContentLength int64
	ContentRange  string
	ContentType   string
	LastModified  time.Time
}

type lakeObjectBackend interface {
	Put(context.Context, string, string, io.Reader, int64, string) error
	Get(context.Context, string, string, string) (lakeObjectMetadata, error)
	Head(context.Context, string, string) (lakeObjectMetadata, error)
	ConfirmAbsent(context.Context, string, string) error
	Delete(context.Context, string, string) error
	DeleteObjects(context.Context, string, []string) ([]lakeDeleteOutcome, error)
}

type lakeDeleteOutcome struct {
	Key     string
	Deleted bool
	Code    string
	Message string
}

func (p *Presigner) Put(
	ctx context.Context,
	bucket, key string,
	body io.Reader,
	contentLength int64,
	contentType string,
) error {
	input := &s3.PutObjectInput{
		Bucket: &bucket, Key: &key, Body: body, ContentLength: &contentLength,
	}
	if contentType != "" {
		input.ContentType = &contentType
	}
	if _, err := p.s3.PutObject(ctx, input, func(options *s3.Options) {
		options.APIOptions = append(options.APIOptions, awsv4.SwapComputePayloadSHA256ForUnsignedPayloadMiddleware)
	}); err != nil {
		return fmt.Errorf("put lake object: %w", err)
	}
	return nil
}

func (p *Presigner) Get(ctx context.Context, bucket, key, byteRange string) (lakeObjectMetadata, error) {
	input := &s3.GetObjectInput{Bucket: &bucket, Key: &key}
	if byteRange != "" {
		input.Range = &byteRange
	}
	output, err := p.s3.GetObject(ctx, input)
	if err != nil {
		return lakeObjectMetadata{}, fmt.Errorf("get lake object: %w", err)
	}
	metadata := lakeObjectMetadata{Body: output.Body}
	if output.ContentLength != nil {
		metadata.ContentLength = *output.ContentLength
	}
	if output.ContentRange != nil {
		metadata.ContentRange = *output.ContentRange
	}
	if output.ContentType != nil {
		metadata.ContentType = *output.ContentType
	}
	if output.LastModified != nil {
		metadata.LastModified = *output.LastModified
	}
	return metadata, nil
}

func (p *Presigner) Head(ctx context.Context, bucket, key string) (lakeObjectMetadata, error) {
	output, err := p.s3.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &bucket, Key: &key})
	if err != nil {
		return lakeObjectMetadata{}, fmt.Errorf("head lake object: %w", err)
	}
	metadata := lakeObjectMetadata{}
	if output.ContentLength != nil {
		metadata.ContentLength = *output.ContentLength
	}
	if output.ContentType != nil {
		metadata.ContentType = *output.ContentType
	}
	if output.LastModified != nil {
		metadata.LastModified = *output.LastModified
	}
	return metadata, nil
}

func (p *Presigner) ConfirmAbsent(ctx context.Context, bucket, key string) error {
	if _, err := p.s3.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &bucket, Key: &key}); err == nil {
		return fmt.Errorf("object remains after delete")
	} else if !objectNotFound(err) {
		return fmt.Errorf("confirm lake object deletion: %w", err)
	}
	return nil
}

func (p *Presigner) Delete(ctx context.Context, bucket, key string) error {
	return p.DeleteObject(ctx, bucket, key)
}

func (p *Presigner) DeleteObjects(ctx context.Context, bucket string, keys []string) ([]lakeDeleteOutcome, error) {
	identifiers := make([]types.ObjectIdentifier, len(keys))
	for index, key := range keys {
		identifiers[index] = types.ObjectIdentifier{Key: aws.String(key)}
	}
	quiet := false
	output, err := p.s3.DeleteObjects(ctx, &s3.DeleteObjectsInput{
		Bucket: &bucket,
		Delete: &types.Delete{Objects: identifiers, Quiet: &quiet},
	}, func(options *s3.Options) {
		// General-purpose S3 requires Content-MD5 for DeleteObjects. The AWS SDK
		// v2 defaults to x-amz-checksum-crc32, which is accepted by AWS but not by
		// every S3-compatible provider. Compute MD5 over the exact serialized XML
		// body so the upstream can validate the request it actually receives.
		options.APIOptions = append(options.APIOptions, smithyhttp.AddContentChecksumMiddleware)
	})
	if err != nil {
		return nil, fmt.Errorf("delete lake objects: %w", err)
	}
	outcomes := make([]lakeDeleteOutcome, 0, len(output.Deleted)+len(output.Errors))
	for _, deleted := range output.Deleted {
		if deleted.Key != nil {
			outcomes = append(outcomes, lakeDeleteOutcome{Key: *deleted.Key, Deleted: true})
		}
	}
	for _, objectError := range output.Errors {
		if objectError.Key == nil {
			continue
		}
		outcome := lakeDeleteOutcome{Key: *objectError.Key}
		if objectError.Code != nil {
			outcome.Code = *objectError.Code
		}
		if objectError.Message != nil {
			outcome.Message = *objectError.Message
		}
		outcomes = append(outcomes, outcome)
	}
	return outcomes, nil
}
