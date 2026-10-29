package source

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"time"

	"github.com/idle-builders/idle-infra/infra-console/internal/api"
)

// TelemetryQuery is the whole caller-controlled query surface. It contains no
// backend expression, object key, file path, or SQL fragment.
type TelemetryQuery struct {
	From      time.Time
	To        time.Time
	Namespace string
	Workload  string
	Service   string
	Limit     int
}

// TelemetrySource lets the HTTP layer stay independent from process execution
// and keeps its contract testable without S3.
type TelemetrySource interface {
	Configured() bool
	Overview(context.Context, TelemetryQuery) (api.ObservabilityOverview, error)
}

// TelemetryClient starts the Arrow/Parquet query helper only when evidence is
// requested. A one-slot semaphore ensures two browser refreshes cannot double
// the query CPU/memory envelope on a small cluster.
type TelemetryClient struct {
	binary    string
	bucket    string
	endpoint  string
	region    string
	accessKey string
	secretKey string
	timeout   time.Duration
	querySlot chan struct{}
}

func NewTelemetryClient(binary, bucket, endpoint, region, accessKey, secretKey string, timeout time.Duration) *TelemetryClient {
	if timeout <= 0 {
		timeout = 45 * time.Second
	}
	return &TelemetryClient{
		binary: binary, bucket: bucket, endpoint: endpoint, region: region,
		accessKey: accessKey, secretKey: secretKey, timeout: timeout,
		querySlot: make(chan struct{}, 1),
	}
}

func (t *TelemetryClient) Configured() bool {
	return t != nil && t.binary != "" && t.bucket != "" && t.endpoint != "" &&
		t.accessKey != "" && t.secretKey != ""
}

func (t *TelemetryClient) Overview(ctx context.Context, query TelemetryQuery) (api.ObservabilityOverview, error) {
	if !t.Configured() {
		return api.ObservabilityOverview{}, fmt.Errorf("telemetry query is not configured")
	}
	select {
	case t.querySlot <- struct{}{}:
		defer func() { <-t.querySlot }()
	case <-ctx.Done():
		return api.ObservabilityOverview{}, ctx.Err()
	}

	ctx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, t.binary, "query")
	// Give the helper only the values it needs. This also avoids duplicate
	// environment keys making an inherited value win over the configured one.
	cmd.Env = t.queryEnvironment(query)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return api.ObservabilityOverview{}, ctx.Err()
		}
		return api.ObservabilityOverview{}, fmt.Errorf("telemetry query failed: %s", SafeError(fmt.Errorf("%s", stderr.String())))
	}
	if stdout.Len() > 2*1024*1024 {
		return api.ObservabilityOverview{}, fmt.Errorf("telemetry query response exceeds 2 MiB")
	}
	var out api.ObservabilityOverview
	decoder := json.NewDecoder(&stdout)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&out); err != nil {
		return api.ObservabilityOverview{}, fmt.Errorf("decode telemetry query: %w", err)
	}
	if out.SchemaVersion != 1 || len(out.Coverage) != 3 {
		return api.ObservabilityOverview{}, fmt.Errorf("telemetry query returned an incompatible schema")
	}
	return out, nil
}

func (t *TelemetryClient) queryEnvironment(query TelemetryQuery) []string {
	return []string{
		"S3_ENDPOINT=" + t.endpoint,
		"S3_REGION=" + t.region,
		"S3_BUCKET=" + t.bucket,
		"S3_ACCESS_KEY_ID=" + t.accessKey,
		"S3_SECRET_ACCESS_KEY=" + t.secretKey,
		"QUERY_FROM=" + query.From.UTC().Format(time.RFC3339),
		"QUERY_TO=" + query.To.UTC().Format(time.RFC3339),
		"QUERY_NAMESPACE=" + query.Namespace,
		"QUERY_WORKLOAD=" + query.Workload,
		"QUERY_SERVICE=" + query.Service,
		"QUERY_LIMIT=" + strconv.Itoa(query.Limit),
	}
}
