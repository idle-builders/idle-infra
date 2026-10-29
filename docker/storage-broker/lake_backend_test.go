package main

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

type nonSeekableReader struct{ io.Reader }

func TestLakeBackendUsesStreamingSingleObjectS3Operations(t *testing.T) {
	payload := []byte("streamed parquet bytes")
	var putBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost && r.URL.Path != "/acme-files/ducklake/main/t/file.parquet" {
			t.Errorf("path = %q", r.URL.Path)
		}
		switch r.Method {
		case http.MethodPut:
			if r.ContentLength != int64(len(payload)) {
				t.Errorf("Content-Length = %d", r.ContentLength)
			}
			putBody, _ = io.ReadAll(r.Body)
			w.Header().Set("ETag", `"upstream-etag"`)
			w.WriteHeader(http.StatusOK)
		case http.MethodHead:
			w.Header().Set("Content-Length", "22")
			w.Header().Set("Content-Type", "application/vnd.apache.parquet")
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			if r.Header.Get("Range") != "bytes=2-5" {
				t.Errorf("Range = %q", r.Header.Get("Range"))
			}
			w.Header().Set("Content-Length", "4")
			w.Header().Set("Content-Range", "bytes 2-5/22")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(payload[2:6])
		case http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		case http.MethodPost:
			if r.URL.Path != "/acme-files" || !r.URL.Query().Has("delete") {
				t.Errorf("DeleteObjects target = %s?%s", r.URL.Path, r.URL.RawQuery)
			}
			body, _ := io.ReadAll(r.Body)
			checksum := md5.Sum(body)
			wantMD5 := base64.StdEncoding.EncodeToString(checksum[:])
			if gotMD5 := r.Header.Get("Content-MD5"); gotMD5 != wantMD5 {
				t.Errorf("DeleteObjects Content-MD5 = %q, want %q", gotMD5, wantMD5)
			}
			var request struct {
				Objects []struct {
					Key string `xml:"Key"`
				} `xml:"Object"`
			}
			if err := xml.Unmarshal(body, &request); err != nil || len(request.Objects) != 2 {
				t.Errorf("DeleteObjects body = %q, err = %v", body, err)
			}
			w.Header().Set("Content-Type", "application/xml")
			_, _ = io.WriteString(w, `<DeleteResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Deleted><Key>ducklake/main/t/a.parquet</Key></Deleted><Error><Key>ducklake/main/t/b.parquet</Key><Code>InternalError</Code><Message>temporary</Message></Error></DeleteResult>`)
		default:
			t.Errorf("method = %s", r.Method)
		}
	}))
	defer server.Close()

	backend := NewPresigner(&Config{
		S3Endpoint: server.URL, S3Region: "us-east-1",
		S3AccessKey: "broker", S3SecretKey: "secret",
	})
	ctx := context.Background()
	if err := backend.Put(ctx, "acme-files", "ducklake/main/t/file.parquet",
		nonSeekableReader{Reader: bytes.NewReader(payload)}, int64(len(payload)), "application/vnd.apache.parquet"); err != nil {
		t.Fatalf("streaming Put: %v", err)
	}
	if !bytes.Equal(putBody, payload) {
		t.Fatalf("PUT body = %q", putBody)
	}
	metadata, err := backend.Head(ctx, "acme-files", "ducklake/main/t/file.parquet")
	if err != nil || metadata.ContentLength != int64(len(payload)) {
		t.Fatalf("Head = %#v, %v", metadata, err)
	}
	metadata, err = backend.Get(ctx, "acme-files", "ducklake/main/t/file.parquet", "bytes=2-5")
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(metadata.Body)
	_ = metadata.Body.Close()
	if err != nil || string(got) != string(payload[2:6]) || metadata.ContentRange != "bytes 2-5/22" {
		t.Fatalf("Get = %q, %#v, %v", got, metadata, err)
	}
	if err := backend.Delete(ctx, "acme-files", "ducklake/main/t/file.parquet"); err != nil {
		t.Fatal(err)
	}
	outcomes, err := backend.DeleteObjects(ctx, "acme-files", []string{
		"ducklake/main/t/a.parquet", "ducklake/main/t/b.parquet",
	})
	if err != nil || len(outcomes) != 2 || !outcomes[0].Deleted || outcomes[0].Key != "ducklake/main/t/a.parquet" ||
		outcomes[1].Code != "InternalError" || outcomes[1].Message != "temporary" {
		t.Fatalf("DeleteObjects = %#v, %v", outcomes, err)
	}
}
