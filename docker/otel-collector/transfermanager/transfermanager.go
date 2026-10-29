// Package transfermanager provides the small subset of AWS transfermanager
// used by the OpenTelemetry S3 exporter. It deliberately uses PutObject so
// S3-compatible storage receives a normal content-length request instead of
// an aws-chunked checksum trailer.
package transfermanager

import (
	"bytes"
	"context"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

type PutObjectAPI interface {
	PutObject(context.Context, *s3.PutObjectInput, ...func(*s3.Options)) (*s3.PutObjectOutput, error)
}

type Client struct {
	s3 PutObjectAPI
}

type Options struct{}

type UploadObjectInput struct {
	Bucket          *string
	Key             *string
	Body            io.Reader
	StorageClass    types.StorageClass
	ACL             types.ObjectCannedACL
	ContentEncoding *string
}

type UploadObjectOutput struct{}

func New(client PutObjectAPI, _ ...func(*Options)) *Client {
	return &Client{s3: client}
}

func (c *Client) UploadObject(ctx context.Context, input *UploadObjectInput, _ ...func(*Options)) (*UploadObjectOutput, error) {
	body := input.Body
	if _, ok := body.(io.ReadSeeker); !ok {
		payload, err := io.ReadAll(body)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(payload)
	}

	put := &s3.PutObjectInput{
		Bucket:          input.Bucket,
		Key:             input.Key,
		Body:            body,
		StorageClass:    s3types.StorageClass(input.StorageClass),
		ACL:             s3types.ObjectCannedACL(input.ACL),
		ContentEncoding: input.ContentEncoding,
	}
	if sized, ok := body.(interface{ Len() int }); ok {
		put.ContentLength = aws.Int64(int64(sized.Len()))
	}

	if _, err := c.s3.PutObject(ctx, put); err != nil {
		return nil, err
	}
	return &UploadObjectOutput{}, nil
}
