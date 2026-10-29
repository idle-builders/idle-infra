package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTelemetryLivesInItsOwningView(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	res := httptest.NewRecorder()
	Handler("").ServeHTTP(res, req)

	body, err := io.ReadAll(res.Result().Body)
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)

	for _, standalone := range []string{
		`id: "observability"`,
		`views.observability =`,
		`<h1>Observability</h1>`,
	} {
		if strings.Contains(html, standalone) {
			t.Errorf("standalone observability surface still contains %q", standalone)
		}
	}

	for _, workloadIntegration := range []string{
		`renderNodeEvidence`,
		`renderWorkloadTable`,
		`hydrateEvidence`,
		`workloadEvidenceByNamespace`,
		`views.nodes =`,
		`views.workloads =`,
		`workload-view-`,
	} {
		if !strings.Contains(html, workloadIntegration) {
			t.Errorf("workload evidence integration %q is missing", workloadIntegration)
		}
	}

	for _, domainRuntime := range []string{
		`isDatabaseRuntime`,
		`isStorageRuntime`,
		`Recent database runtime`,
		`Recent storage runtime`,
	} {
		if strings.Contains(html, domainRuntime) {
			t.Errorf("generic workload evidence leaked into a domain view through %q", domainRuntime)
		}
	}

	for _, databaseIntegration := range []string{
		`renderDatabaseEvidence`,
		`databaseEvidenceBadge`,
		`evidence-database`,
		`PostgreSQL service`,
		`Current activity`,
		`/database/activity`,
	} {
		if !strings.Contains(html, databaseIntegration) {
			t.Errorf("database evidence integration %q is missing", databaseIntegration)
		}
	}

	for _, storageIntegration := range []string{
		`renderStorageBrokerEvidence`,
		`evidence-storage-broker`,
		`Storage broker`,
		`metrics endpoints reporting`,
		`does not test request or S3 availability`,
	} {
		if !strings.Contains(html, storageIntegration) {
			t.Errorf("storage evidence integration %q is missing", storageIntegration)
		}
	}

	for _, identityIntegration := range []string{
		`renderIdentityEvidence`,
		`evidence-identity`,
		`Zitadel authentication`,
		`Rejected responses are 4xx`,
		`authentication or database availability`,
	} {
		if !strings.Contains(html, identityIntegration) {
			t.Errorf("identity evidence integration %q is missing", identityIntegration)
		}
	}

	for _, gap := range []string{
		`instrumentationPartial("Database"`,
		`instrumentationPartial("Storage"`,
		`instrumentationPartial("Identity"`,
		`observability — not instrumented`,
	} {
		if !strings.Contains(html, gap) {
			t.Errorf("explicit domain coverage gap %q is missing", gap)
		}
	}
}

func TestApplicationsListServicesAndKeepEvidenceInServiceDetails(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	res := httptest.NewRecorder()
	Handler("").ServeHTTP(res, req)
	body, err := io.ReadAll(res.Result().Body)
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)

	for _, behavior := range []string{
		`{ grp: "Applications" }`,
		`id: "services", name: "Services", title: "Applications"`,
		`renderApplicationEvidence`,
		`data-service-open`,
		`id="service-detail"`,
		`Recent execution samples`,
		`Complete operation totals`,
		`sampleReasonLabel`,
		`slowest in interval`,
		`representative sample`,
	} {
		if !strings.Contains(html, behavior) {
			t.Errorf("application observability behavior %q is missing", behavior)
		}
	}
}

func TestWorkloadsUseOneReadableListWithDetails(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	res := httptest.NewRecorder()
	Handler("").ServeHTTP(res, req)

	body, err := io.ReadAll(res.Result().Body)
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)

	for _, behavior := range []string{
		`data-workload-filter="active"`,
		`data-workload-filter="all"`,
		`data-workload-open`,
		`id="workload-detail"`,
		`showModal()`,
		`Recent warnings`,
		`w.kind === "CronJob" && w.ready > 0`,
	} {
		if !strings.Contains(html, behavior) {
			t.Errorf("joined workload interaction %q is missing", behavior)
		}
	}

	for _, duplicate := range []string{
		`Recent workload resources`,
		`Recent workload warnings`,
		`table class="wl"`,
	} {
		if strings.Contains(html, duplicate) {
			t.Errorf("old duplicate workload presentation still contains %q", duplicate)
		}
	}
}

func TestDatabaseAndStorageShowOnlyUsefulFacts(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	res := httptest.NewRecorder()
	Handler("").ServeHTTP(res, req)

	body, err := io.ReadAll(res.Result().Body)
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)

	for _, rawBrowser := range []string{
		`>Browse</button>`,
		`function browseRows(`,
		`data-table=`,
	} {
		if strings.Contains(html, rawBrowser) {
			t.Errorf("database still exposes unhelpful raw row browsing through %q", rawBrowser)
		}
	}

	for _, databaseSummary := range []string{
		`Largest tables first`,
		`sort(function (a, b) { return b.sizeBytes - a.sizeBytes; })`,
	} {
		if !strings.Contains(html, databaseSummary) {
			t.Errorf("database size summary %q is missing", databaseSummary)
		}
	}

	for _, partialStorage := range []string{
		`s.partial === true`,
		`Objects scanned`,
		`Size scanned`,
		`Actual bucket totals are larger`,
		`Partial S3 scan`,
	} {
		if strings.Contains(html, partialStorage) {
			t.Errorf("storage still exposes a misleading lower bound through %q", partialStorage)
		}
	}

	for _, exactStorage := range []string{
		`card("Objects"`,
		`card("Size"`,
		`while (v >= 1000`,
	} {
		if !strings.Contains(html, exactStorage) {
			t.Errorf("exact storage fact %q is missing", exactStorage)
		}
	}
}

func TestUnavailableInventoryDoesNotBecomeDeadNavigationOrRawError(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	res := httptest.NewRecorder()
	Handler("").ServeHTTP(res, req)

	body, err := io.ReadAll(res.Result().Body)
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)

	for _, deadSurface := range []string{
		`id: "edge"`,
		`views.edge =`,
		`IAM_OWNER_VIEWER`,
	} {
		if strings.Contains(html, deadSurface) {
			t.Errorf("UI still exposes dead or internal surface %q", deadSurface)
		}
	}

	for _, explanation := range []string{
		`Identity inventory unavailable`,
		`Authentication metrics above still work`,
	} {
		if !strings.Contains(html, explanation) {
			t.Errorf("identity inventory explanation %q is missing", explanation)
		}
	}
}
