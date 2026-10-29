package main

import (
	"bytes"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"
)

type brokerOperation uint8

const (
	operationPresign brokerOperation = iota
	operationMultipartCreate
	operationMultipartPart
	operationMultipartComplete
	operationMultipartAbort
	operationGovernedReserveObject
	operationGovernedCommitObject
	operationGovernedDeleteObject
	operationGovernedCleanupReservations
	operationGovernedCleanupOwner
	operationGovernedCleanupWorkspace
	operationGovernedRegisterObject
	operationGovernedIssueGrant
	operationGovernedObjectTicket
	operationGovernedReserveOutput
	operationGovernedCommitOutput
	operationGovernedRevokeGrant
	operationLakeInventory
	operationLakePut
	operationLakeGet
	operationLakeHead
	operationLakeDelete
	operationLakeDeleteObjects
	operationCount
)

var operationNames = [operationCount]string{
	"presign",
	"multipart_create",
	"multipart_part",
	"multipart_complete",
	"multipart_abort",
	"governed_reserve_object",
	"governed_commit_object",
	"governed_delete_object",
	"governed_cleanup_reservations",
	"governed_cleanup_owner",
	"governed_cleanup_workspace",
	"governed_register_object",
	"governed_issue_grant",
	"governed_object_ticket",
	"governed_reserve_output",
	"governed_commit_output",
	"governed_revoke_grant",
	"lake_inventory",
	"lake_put",
	"lake_get",
	"lake_head",
	"lake_delete",
	"lake_delete_objects",
}

func instrumentLakeRequest(metrics *brokerMetrics, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		operation := operationLakeGet
		switch r.Method {
		case http.MethodPut:
			operation = operationLakePut
		case http.MethodHead:
			operation = operationLakeHead
		case http.MethodDelete:
			operation = operationLakeDelete
		case http.MethodPost:
			if r.URL.Query().Has("delete") {
				operation = operationLakeDeleteObjects
			}
		}
		metrics.instrument(operation, next).ServeHTTP(w, r)
	})
}

type brokerOutcome uint8

const (
	outcomeSuccess brokerOutcome = iota
	outcomeInvalid
	outcomeUnauthenticated
	outcomeDenied
	outcomeRejected
	outcomeError
	outcomeCount
)

var outcomeNames = [outcomeCount]string{
	"success",
	"invalid",
	"unauthenticated",
	"denied",
	"rejected",
	"error",
}

var requestDurationBuckets = [...]float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5}

type metricCell struct {
	mu            sync.Mutex
	count         uint64
	durationNanos uint64
	buckets       [len(requestDurationBuckets)]uint64
}

type metricSnapshot struct {
	count         uint64
	durationNanos uint64
	buckets       [len(requestDurationBuckets)]uint64
}

type brokerMetrics struct {
	cells [operationCount][outcomeCount]metricCell
}

func newBrokerMetrics() *brokerMetrics { return &brokerMetrics{} }

func (m *brokerMetrics) instrument(operation brokerOperation, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		status := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(status, r)
		m.record(operation, outcomeForStatus(status.status), time.Since(started))
	})
}

func (m *brokerMetrics) record(operation brokerOperation, outcome brokerOutcome, duration time.Duration) {
	cell := &m.cells[operation][outcome]
	cell.mu.Lock()
	defer cell.mu.Unlock()
	cell.count++
	cell.durationNanos += uint64(duration)
	seconds := duration.Seconds()
	for index, boundary := range requestDurationBuckets {
		if seconds <= boundary {
			cell.buckets[index]++
		}
	}
}

func (c *metricCell) snapshot() metricSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	return metricSnapshot{count: c.count, durationNanos: c.durationNanos, buckets: c.buckets}
}

func (m *brokerMetrics) serveHTTP(w http.ResponseWriter, _ *http.Request) {
	var body bytes.Buffer
	body.WriteString("# HELP storage_broker_requests_total Broker HTTP requests by bounded operation and outcome.\n")
	body.WriteString("# TYPE storage_broker_requests_total counter\n")
	for operation := brokerOperation(0); operation < operationCount; operation++ {
		for outcome := brokerOutcome(0); outcome < outcomeCount; outcome++ {
			snapshot := m.cells[operation][outcome].snapshot()
			if snapshot.count == 0 {
				continue
			}
			fmt.Fprintf(&body, "storage_broker_requests_total{operation=%q,outcome=%q} %d\n",
				operationNames[operation], outcomeNames[outcome], snapshot.count)
		}
	}

	body.WriteString("# HELP storage_broker_request_duration_seconds Broker HTTP request duration by bounded operation and outcome.\n")
	body.WriteString("# TYPE storage_broker_request_duration_seconds histogram\n")
	for operation := brokerOperation(0); operation < operationCount; operation++ {
		for outcome := brokerOutcome(0); outcome < outcomeCount; outcome++ {
			snapshot := m.cells[operation][outcome].snapshot()
			if snapshot.count == 0 {
				continue
			}
			for index, boundary := range requestDurationBuckets {
				fmt.Fprintf(&body, "storage_broker_request_duration_seconds_bucket{operation=%q,outcome=%q,le=%q} %d\n",
					operationNames[operation], outcomeNames[outcome], strconv.FormatFloat(boundary, 'g', -1, 64), snapshot.buckets[index])
			}
			fmt.Fprintf(&body, "storage_broker_request_duration_seconds_bucket{operation=%q,outcome=%q,le=%q} %d\n",
				operationNames[operation], outcomeNames[outcome], "+Inf", snapshot.count)
			fmt.Fprintf(&body, "storage_broker_request_duration_seconds_sum{operation=%q,outcome=%q} %s\n",
				operationNames[operation], outcomeNames[outcome], strconv.FormatFloat(float64(snapshot.durationNanos)/float64(time.Second), 'g', -1, 64))
			fmt.Fprintf(&body, "storage_broker_request_duration_seconds_count{operation=%q,outcome=%q} %d\n",
				operationNames[operation], outcomeNames[outcome], snapshot.count)
		}
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body.Bytes())
}

func outcomeForStatus(status int) brokerOutcome {
	switch status {
	case http.StatusBadRequest:
		return outcomeInvalid
	case http.StatusUnauthorized:
		return outcomeUnauthenticated
	case http.StatusForbidden:
		return outcomeDenied
	case http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusRequestEntityTooLarge:
		return outcomeRejected
	default:
		if status >= 500 {
			return outcomeError
		}
		if status >= 400 {
			return outcomeRejected
		}
		return outcomeSuccess
	}
}

type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *statusRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *statusRecorder) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}
