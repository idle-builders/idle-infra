package httpapi

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/idle-builders/idle-infra/infra-console/internal/api"
	"github.com/idle-builders/idle-infra/infra-console/internal/source"
)

type fakeTelemetry struct {
	query source.TelemetryQuery
}

func (f *fakeTelemetry) Configured() bool { return true }

func (f *fakeTelemetry) Overview(_ context.Context, query source.TelemetryQuery) (api.ObservabilityOverview, error) {
	f.query = query
	return api.ObservabilityOverview{
		SchemaVersion:   1,
		Fidelity:        "complete",
		PolicyVersion:   "o1-initial-v1",
		RequestedWindow: api.ObservabilityWindow{From: query.From, To: query.To},
		Coverage: []api.SignalCoverage{
			{Signal: api.Logs, Status: api.SignalCoverageStatusOk, Rows: 5},
			{Signal: api.Metrics, Status: api.SignalCoverageStatusOk, Rows: 10},
			{Signal: api.Traces, Status: api.SignalCoverageStatusGap, Rows: 0},
		},
		Warnings:  []api.ObservabilityWarning{},
		Workloads: []api.ObservedWorkload{},
		Nodes:     []api.ObservedNode{},
		Databases: []api.ObservedDatabaseEndpoint{{
			Workload: "postgresql-primary", Role: api.ObservedDatabaseEndpointRolePrimary,
			ObservedFrom: query.From, SampledAt: query.To,
		}},
		StorageBroker: &api.ObservedStorageBroker{
			ObservedFrom: query.From, SampledAt: query.To,
			TargetsObservedInWindow: 2, TargetsCurrent: 2, TargetsScrapeable: 2,
		},
		Identity: &api.ObservedIdentity{
			ObservedFrom: query.From, SampledAt: query.To,
			TargetsObservedInWindow: 2, TargetsCurrent: 2, TargetsScrapeable: 2,
			Routes: []api.ObservedIdentityRoute{{Route: api.Token}},
		},
	}, nil
}

func TestObservabilityEndpointPassesBoundedFilters(t *testing.T) {
	fake := &fakeTelemetry{}
	srv := &Server{Telemetry: fake}
	handler := api.HandlerFromMuxWithBaseURL(srv, http.NewServeMux(), "/v1")

	code, body := get(t, handler, "/v1/observability?windowMinutes=30&namespace=acme&workload=worker&service=acme-api&limit=12")
	if code != http.StatusOK {
		t.Fatalf("status = %d: %v", code, body)
	}
	if got := fake.query.To.Sub(fake.query.From); got != 30*time.Minute {
		t.Errorf("window = %s, want 30m", got)
	}
	if fake.query.Namespace != "acme" || fake.query.Workload != "worker" || fake.query.Service != "acme-api" || fake.query.Limit != 12 {
		t.Errorf("query = %+v", fake.query)
	}
	coverage, ok := body["coverage"].([]any)
	if !ok || len(coverage) != 3 {
		t.Fatalf("coverage = %#v, want all three signals", body["coverage"])
	}
	databases, ok := body["databases"].([]any)
	if !ok || len(databases) != 1 {
		t.Fatalf("databases = %#v, want one endpoint", body["databases"])
	}
	storage, ok := body["storageBroker"].(map[string]any)
	if !ok || storage["targetsScrapeable"] != float64(2) {
		t.Fatalf("storageBroker = %#v, want two scrapeable targets", body["storageBroker"])
	}
	identity, ok := body["identity"].(map[string]any)
	if !ok || identity["targetsScrapeable"] != float64(2) {
		t.Fatalf("identity = %#v, want two scrapeable targets", body["identity"])
	}
}

func TestObservabilityEndpointRejectsUnboundedWindow(t *testing.T) {
	srv := &Server{Telemetry: &fakeTelemetry{}}
	handler := api.HandlerFromMuxWithBaseURL(srv, http.NewServeMux(), "/v1")

	code, _ := get(t, handler, "/v1/observability?windowMinutes=2881")
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}
}
