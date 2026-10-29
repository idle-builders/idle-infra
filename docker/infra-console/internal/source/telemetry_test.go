package source

import (
	"slices"
	"testing"
	"time"
)

func TestTelemetryQueryEnvironmentIncludesBoundedServiceFilter(t *testing.T) {
	client := NewTelemetryClient("query", "bucket", "endpoint", "region", "key", "secret", time.Second)
	environment := client.queryEnvironment(TelemetryQuery{
		From: time.Unix(1, 0), To: time.Unix(2, 0), Service: "acme-api", Limit: 10,
	})
	if !slices.Contains(environment, "QUERY_SERVICE=acme-api") {
		t.Fatalf("environment does not include service filter: %v", environment)
	}
}
