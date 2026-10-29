package source

import (
	"net/http"
	"strings"
	"testing"
)

func TestZitadelClassifiesMissingInventoryPermission(t *testing.T) {
	err := zitadelStatusError(http.StatusNotFound,
		strings.NewReader(`{"code":5,"message":"membership not found (AUTHZ-cdgFk)"}`))
	if err.Error() != "identity inventory unavailable" {
		t.Fatalf("status error = %q, want stable inventory classification", err)
	}
}

func TestZitadelDoesNotExposeUnexpectedErrorBodies(t *testing.T) {
	err := zitadelStatusError(http.StatusInternalServerError,
		strings.NewReader(`{"message":"internal detail must stay private"}`))
	if strings.Contains(err.Error(), "internal detail") {
		t.Fatalf("status error exposed upstream body: %q", err)
	}
	if !strings.Contains(err.Error(), "status 500") {
		t.Fatalf("status error = %q, want status code", err)
	}
}
