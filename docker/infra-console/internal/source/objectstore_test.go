package source

import (
	"net/http"
	"testing"
)

func TestStorageSummaryUsesExactSwiftMetadata(t *testing.T) {
	headers := make(http.Header)
	headers.Set("X-Container-Object-Count", "3256079")
	headers.Set("X-Container-Bytes-Used", "102325924290")

	got, err := storageSummary("nimmly-files", "https://objects.example/v1/AUTH_project", headers)
	if err != nil {
		t.Fatal(err)
	}
	if got.ObjectCount != 3256079 || got.BytesUsed != 102325924290 {
		t.Fatalf("summary = %+v, want exact provider counters", got)
	}
	if got.Endpoint == nil || *got.Endpoint != "https://objects.example/v1/AUTH_project" {
		t.Fatalf("endpoint = %v", got.Endpoint)
	}
}

func TestStorageSummaryRejectsMissingOrMalformedCounters(t *testing.T) {
	for name, headers := range map[string]http.Header{
		"missing": {},
		"bad count": {
			"X-Container-Object-Count": []string{"many"},
			"X-Container-Bytes-Used":   []string{"1024"},
		},
		"negative bytes": {
			"X-Container-Object-Count": []string{"1"},
			"X-Container-Bytes-Used":   []string{"-1"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := storageSummary("bucket", "https://objects.example", headers); err == nil {
				t.Fatal("storageSummary succeeded with invalid provider metadata")
			}
		})
	}
}
