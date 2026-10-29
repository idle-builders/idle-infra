package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestBrokerMetricsRecordsOnlyBoundedOperationAndOutcome(t *testing.T) {
	metrics := newBrokerMetrics()
	handler := metrics.instrument(operationPresign, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/allowed/private/customer-42.txt":
			w.WriteHeader(http.StatusOK)
		case "/denied/private/customer-42.txt":
			w.WriteHeader(http.StatusForbidden)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))

	for _, path := range []string{
		"/allowed/private/customer-42.txt",
		"/denied/private/customer-42.txt",
		"/failed/private/customer-42.txt",
	} {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, path, nil))
	}

	recorder := httptest.NewRecorder()
	metrics.serveHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := recorder.Body.String()

	if recorder.Code != http.StatusOK {
		t.Fatalf("metrics status = %d, want 200", recorder.Code)
	}
	for _, want := range []string{
		`storage_broker_requests_total{operation="presign",outcome="success"} 1`,
		`storage_broker_requests_total{operation="presign",outcome="denied"} 1`,
		`storage_broker_requests_total{operation="presign",outcome="error"} 1`,
		`storage_broker_request_duration_seconds_count{operation="presign",outcome="success"} 1`,
		`storage_broker_request_duration_seconds_count{operation="presign",outcome="denied"} 1`,
		`storage_broker_request_duration_seconds_count{operation="presign",outcome="error"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q\n%s", want, body)
		}
	}
	if strings.Contains(body, "customer-42") || strings.Contains(body, "private") {
		t.Fatalf("metrics leaked request-derived labels: %s", body)
	}
	if got := recorder.Header().Get("Content-Type"); got != "text/plain; version=0.0.4; charset=utf-8" {
		t.Fatalf("Content-Type = %q", got)
	}
}

func TestBrokerMetricsSnapshotsStayInternallyConsistent(t *testing.T) {
	metrics := newBrokerMetrics()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 10_000 {
			metrics.record(operationPresign, outcomeSuccess, time.Millisecond)
		}
	}()

	for {
		snapshot := metrics.cells[operationPresign][outcomeSuccess].snapshot()
		if snapshot.durationNanos != snapshot.count*uint64(time.Millisecond) {
			t.Fatalf("inconsistent snapshot: count=%d duration=%d", snapshot.count, snapshot.durationNanos)
		}
		for index, count := range snapshot.buckets {
			if count != snapshot.count {
				t.Fatalf("bucket %d = %d, want count %d", index, count, snapshot.count)
			}
		}
		select {
		case <-done:
			return
		default:
		}
	}
}

func TestBrokerMetricsTreatsImplicitStatusAsSuccess(t *testing.T) {
	metrics := newBrokerMetrics()
	handler := metrics.instrument(operationMultipartPart, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/anything", nil))

	recorder := httptest.NewRecorder()
	metrics.serveHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if body := recorder.Body.String(); !strings.Contains(body,
		`storage_broker_requests_total{operation="multipart_part",outcome="success"} 1`) {
		t.Fatalf("implicit 200 was not counted as success:\n%s", body)
	}
}

func TestBrokerMetricsClassifiesDeleteObjectsSeparately(t *testing.T) {
	metrics := newBrokerMetrics()
	handler := instrumentLakeRequest(metrics, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/nimmly-files?delete", nil))

	recorder := httptest.NewRecorder()
	metrics.serveHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if body := recorder.Body.String(); !strings.Contains(body,
		`storage_broker_requests_total{operation="lake_delete_objects",outcome="success"} 1`) {
		t.Fatalf("DeleteObjects was not classified separately:\n%s", body)
	}
}
