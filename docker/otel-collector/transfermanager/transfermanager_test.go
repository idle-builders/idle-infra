package transfermanager

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type recordingS3 struct {
	input *s3.PutObjectInput
}

func (r *recordingS3) PutObject(_ context.Context, input *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	r.input = input
	return &s3.PutObjectOutput{}, nil
}

func TestUploadObjectUsesKnownLengthPut(t *testing.T) {
	api := &recordingS3{}
	client := New(api)
	body := bytes.NewBufferString("telemetry")
	encoding := "gzip"

	_, err := client.UploadObject(context.Background(), &UploadObjectInput{
		Bucket:          stringPointer("bucket"),
		Key:             stringPointer("raw/object.json.gz"),
		Body:            body,
		StorageClass:    types.StorageClass("STANDARD"),
		ContentEncoding: &encoding,
	})
	if err != nil {
		t.Fatalf("UploadObject returned an error: %v", err)
	}
	if api.input == nil || api.input.ContentLength == nil || *api.input.ContentLength != 9 {
		t.Fatalf("PutObject content length = %v, want 9", api.input.ContentLength)
	}
	if api.input.ContentEncoding == nil || *api.input.ContentEncoding != "gzip" {
		t.Fatalf("PutObject content encoding = %v, want gzip", api.input.ContentEncoding)
	}
	if _, ok := api.input.Body.(io.ReadSeeker); !ok {
		t.Fatalf("PutObject body type = %T, want io.ReadSeeker", api.input.Body)
	}
	payload, err := io.ReadAll(api.input.Body)
	if err != nil {
		t.Fatalf("read PutObject body: %v", err)
	}
	if string(payload) != "telemetry" {
		t.Fatalf("PutObject body = %q, want telemetry", payload)
	}
}

func stringPointer(value string) *string {
	return &value
}
