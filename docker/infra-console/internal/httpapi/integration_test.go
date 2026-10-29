// Handler tests against the real backing services.
//
// No fake clientset: the Server is wired to a real Kubernetes API server, a
// real PostgreSQL and the real Zitadel, exactly as main.go wires it. A fake
// would let a wrong field path or a missing wire pass — which is how three
// sources were once built, tested and then never actually passed to the
// Server, 501ing in production while every unit test stayed green.
package httpapi

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"github.com/idle-builders/idle-infra/infra-console/internal/auth"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	metricsv "k8s.io/metrics/pkg/client/clientset/versioned"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/idle-builders/idle-infra/infra-console/internal/api"
	"github.com/idle-builders/idle-infra/infra-console/internal/source"
	"github.com/idle-builders/idle-infra/infra-console/web"
)

func env(t *testing.T, key string) string {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		t.Skipf("%s not set; run integration/up.sh first", key)
	}
	return v
}

// realServer wires every source the way main.go does, so a source that is
// constructed but not passed in is caught here rather than in production.
func realServer(t *testing.T) (http.Handler, string) {
	t.Helper()
	srv, ns := realServerStruct(t)
	return api.HandlerFromMuxWithBaseURL(srv, http.NewServeMux(), "/v1"), ns
}

func realServerStruct(t *testing.T) (*Server, string) {
	t.Helper()
	ns := env(t, "INTEG_NAMESPACE")
	if os.Getenv("INTEG_KUBE") == "" {
		t.Skip("INTEG_KUBE not set")
	}

	cfg, err := clientcmd.BuildConfigFromFlags("", os.Getenv("KUBECONFIG"))
	if err != nil {
		t.Fatalf("kube config: %v", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("kube client: %v", err)
	}
	var mc metricsv.Interface
	if m, err := metricsv.NewForConfig(cfg); err == nil {
		mc = m
	}

	srv := &Server{
		K8s:   source.NewK8sWithClients(cs, mc),
		Cache: source.NewCache(),
		TTL:   TTLs{Kubernetes: time.Minute, Postgres: time.Minute, Zitadel: time.Minute, ObjectStore: time.Minute},
	}

	if dsn := os.Getenv("INTEG_POSTGRES_DSN"); dsn != "" {
		db, err := sql.Open("pgx", dsn)
		if err != nil {
			t.Fatalf("postgres: %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		srv.Postgres = source.NewPostgresClient(db, 100, 10*time.Second)
	}
	if url := os.Getenv("INTEG_ZITADEL_URL"); url != "" {
		srv.Zitadel = source.NewZitadelClient(url, os.Getenv("INTEG_ZITADEL_TOKEN"))
	}
	if ep := os.Getenv("INTEG_SWIFT_ENDPOINT"); ep != "" {
		srv.ObjectStore = source.NewObjectStoreClient(
			os.Getenv("INTEG_SWIFT_AUTH_URL"), ep, os.Getenv("INTEG_S3_ENDPOINT"),
			os.Getenv("INTEG_SWIFT_APPLICATION_CREDENTIAL_ID"),
			os.Getenv("INTEG_SWIFT_APPLICATION_CREDENTIAL_SECRET"),
		)
	}

	return srv, ns
}

func get(t *testing.T, h http.Handler, path string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	var body map[string]any
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
	}
	return rec.Code, body
}

// getAs issues a request carrying a verified principal, the way the auth
// middleware attaches one.
func getAs(t *testing.T, h http.Handler, path string, p auth.Principal) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req = req.WithContext(auth.WithPrincipal(req.Context(), p))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var body map[string]any
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
	}
	return rec.Code, body
}

func section(t *testing.T, body map[string]any, name string) map[string]any {
	t.Helper()
	s, ok := body[name].(map[string]any)
	if !ok {
		t.Fatalf("no %q section in %v", name, body)
	}
	return s
}

// Every source wired in main.go must actually be reachable through the
// handler. This is the test that would have caught Postgres, Zitadel and Logs
// being built but never passed in.
func TestEverySourceIsReachable(t *testing.T) {
	srv, ns := realServerStruct(t)
	h := api.HandlerFromMuxWithBaseURL(srv, http.NewServeMux(), "/v1")

	cases := []struct{ name, path string }{
		{"projects", "/v1/projects"},
		{"nodes", "/v1/platform/nodes"},
		{"services", "/v1/platform/services"},
		{"database", "/v1/projects/" + ns + "/database"},
		{"tables", "/v1/projects/" + ns + "/database/tables"},
		{"activity", "/v1/projects/" + ns + "/database/activity"},
		{"rows", "/v1/projects/" + ns + "/database/tables/players/rows"},
		{"releases", "/v1/projects/" + ns + "/releases"},
		{"workloads", "/v1/projects/" + ns + "/workloads"},
		{"domains", "/v1/projects/" + ns + "/domains"},
	}
	// Exact storage counters require a real Swift+Keystone pair. The local
	// harness has MinIO for S3 telemetry, but pretending it implements Swift
	// metadata would verify a protocol the production provider does not use.
	if srv.ObjectStore != nil {
		cases = append(cases, struct{ name, path string }{"storage", "/v1/projects/" + ns + "/storage"})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, body := get(t, h, tc.path)
			if code == http.StatusNotImplemented {
				t.Fatalf("GET %s returned 501 — the source is wired in main.go but not reachable through the handler", tc.path)
			}
			if code != http.StatusOK {
				t.Fatalf("GET %s = %d: %v", tc.path, code, body)
			}
		})
	}
}

// A shared service is a workload owned by infra rather than by a tenant, so
// infra is addressable for workloads. It is NOT a tenant — it has no schema,
// bucket or Zitadel org — and widening the workloads gate must not widen any
// other surface with it.
func TestInfraIsAWorkloadOwnerButNotATenant(t *testing.T) {
	h, _ := realServer(t)

	code, body := get(t, h, "/v1/projects/infra/workloads")
	if code != http.StatusOK {
		t.Fatalf("GET /v1/projects/infra/workloads = %d: %v — the platform's own workloads must be readable", code, body)
	}

	// Everything a tenant has and infra does not.
	for _, path := range []string{
		"/v1/projects/infra/database",
		"/v1/projects/infra/database/tables",
		"/v1/projects/infra/storage",
		"/v1/projects/infra/auth",
		"/v1/projects/infra/domains",
		"/v1/projects/infra/releases",
	} {
		t.Run(path, func(t *testing.T) {
			code, _ := get(t, h, path)
			if code != http.StatusNotFound {
				t.Errorf("GET %s = %d, want 404: infra is a workload owner, not a tenant", path, code)
			}
		})
	}

	// And it must not leak into the tenant list.
	code, body = get(t, h, "/v1/projects")
	if code != http.StatusOK {
		t.Fatalf("GET /v1/projects = %d", code)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal project list: %v", err)
	}
	if strings.Contains(string(raw), `"name":"infra"`) {
		t.Errorf("infra appears in the tenant list; it is a platform namespace, not a tenant")
	}
}

func TestProjectAggregateCarriesRealData(t *testing.T) {
	h, ns := realServer(t)

	code, body := get(t, h, "/v1/projects/"+ns)
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}

	// Kubernetes and PostgreSQL are both live here, so both must be ok with
	// data rather than degraded.
	for _, name := range []string{"workloads", "quota", "domains", "database"} {
		s := section(t, body, name)
		if s["status"] != "ok" {
			t.Errorf("%s status = %v (error: %v), want ok against live services", name, s["status"], s["error"])
		}
		if _, present := s["data"]; !present {
			t.Errorf("%s is ok but carries no data", name)
		}
	}

	db := section(t, body, "database")["data"].(map[string]any)
	if db["schema"] != ns {
		t.Errorf("schema = %v, want %q", db["schema"], ns)
	}
}

// A source that is genuinely unreachable degrades that section only; the rest
// of the aggregate still renders.
func TestAggregateDegradesWhenASourceIsUnreachable(t *testing.T) {
	_, ns := realServerStruct(t)

	// Point Zitadel at a closed port: a real connection failure, not a stub.
	srv := &Server{
		K8s:     mustK8s(t),
		Zitadel: source.NewZitadelClient("http://127.0.0.1:1", "token"),
		Cache:   source.NewCache(),
		TTL:     TTLs{Kubernetes: time.Minute, Zitadel: time.Minute},
	}
	handler := api.HandlerFromMuxWithBaseURL(srv, http.NewServeMux(), "/v1")

	code, body := get(t, handler, "/v1/projects/"+ns)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 despite an unreachable source", code)
	}
	auth := section(t, body, "auth")
	if auth["status"] != "unavailable" {
		t.Fatalf("auth status = %v, want unavailable", auth["status"])
	}
	if _, present := auth["data"]; present {
		t.Error("an unavailable section must omit data entirely")
	}
	if section(t, body, "workloads")["status"] != "ok" {
		t.Error("a healthy source must be unaffected by another's failure")
	}
}

func mustK8s(t *testing.T) *source.K8s {
	t.Helper()
	cfg, err := clientcmd.BuildConfigFromFlags("", os.Getenv("KUBECONFIG"))
	if err != nil {
		t.Fatalf("kube config: %v", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("kube client: %v", err)
	}
	return source.NewK8sWithClients(cs, nil)
}

func TestRowsEndpointMasksAndBounds(t *testing.T) {
	h, ns := realServer(t)

	code, body := get(t, h, "/v1/projects/"+ns+"/database/tables/players/rows?limit=2")
	if code != http.StatusOK {
		t.Fatalf("status = %d: %v", code, body)
	}
	// players carries table-level SELECT, so the owner withheld nothing and
	// the console must not invent a restriction.
	if masked, _ := body["maskedColumns"].([]any); len(masked) != 0 {
		t.Errorf("maskedColumns = %v, but the owner granted the whole table", masked)
	}
	rows, _ := body["rows"].([]any)
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want the requested 2", len(rows))
	}
	if body["truncated"] != true {
		t.Error("3 seeded rows with limit 2 should report truncated")
	}
	for _, r := range rows {
		row := r.(map[string]any)
		if row["email"] == nil || row["api_key"] == nil {
			t.Fatalf("a granted column came back NULL: %v", row)
		}
	}
}

// The same endpoint over a table whose owner withheld columns: those come back
// NULL, and the withholding is reported so the client can say why.
func TestRowsEndpointHonoursWithheldColumns(t *testing.T) {
	h, ns := realServer(t)

	code, body := get(t, h, "/v1/projects/"+ns+"/database/tables/accounts/rows?limit=5")
	if code != http.StatusOK {
		t.Fatalf("status = %d: %v", code, body)
	}
	masked, _ := body["maskedColumns"].([]any)
	got := map[string]bool{}
	for _, m := range masked {
		got[m.(string)] = true
	}
	for _, want := range []string{"metadata", "notes"} {
		if !got[want] {
			t.Errorf("maskedColumns = %v, missing the withheld %s", masked, want)
		}
	}
	raw, _ := json.Marshal(body)
	for _, secret := range []string{"sk_live_DEADBEEF", "123-45-6789"} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("withheld value %q reached the response", secret)
		}
	}
}

// A composite-key table cannot be paged stably; the contract declares 409.
func TestUnpageableTableIs409(t *testing.T) {
	h, ns := realServer(t)
	code, body := get(t, h, "/v1/projects/"+ns+"/database/tables/memberships/rows")
	if code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %v", code, body)
	}
}

func TestUnknownProjectIs404(t *testing.T) {
	h, _ := realServer(t)
	code, body := get(t, h, "/v1/projects/definitely-not-a-tenant")
	if code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", code)
	}
	if body["title"] == nil {
		t.Errorf("want an RFC 9457 problem body, got %v", body)
	}
}

// /platform/edge used to emit `zones` and `recordCount` as bare fields, so a
// failed Cloudflare read rendered "0 zones, 0 DNS records" — indistinguishable
// from an account that genuinely has none, and with no error anywhere in the
// response. Both now live inside one Section.
func TestEdgeDegradesWithoutAConfidentZero(t *testing.T) {
	// A closed port: a real connection failure, not a stub.
	srv := &Server{
		Cloudflare: source.NewCloudflareClient("token-shaped-value", "account"),
		Cache:      source.NewCache(),
		TTL:        TTLs{Cloudflare: time.Minute},
	}
	srv.Cloudflare.SetBaseURL("http://127.0.0.1:1")
	handler := api.HandlerFromMuxWithBaseURL(srv, http.NewServeMux(), "/v1")

	code, body := get(t, handler, "/v1/platform/edge")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 despite an unreachable edge", code)
	}

	hostnames := section(t, body, "hostnames")
	if hostnames["status"] != "unavailable" {
		t.Fatalf("hostnames status = %v, want unavailable", hostnames["status"])
	}
	if _, present := hostnames["data"]; present {
		t.Error("an unavailable section must omit data entirely")
	}
	if hostnames["error"] == nil || hostnames["error"] == "" {
		t.Error("an unavailable section must say why")
	}

	// The specific regression: no zero-valued count anywhere at the top level.
	for _, gone := range []string{"zones", "recordCount"} {
		if _, present := body[gone]; present {
			t.Errorf("%q is a bare top-level field again; it must live inside the hostnames Section", gone)
		}
	}
}

// The row browser is the most sensitive operation here, and every read reaches
// PostgreSQL as the shared platform_reader role. The audit line is therefore
// the only place a row read is attributable to a person — it carried no caller
// at all, contradicting the contract's own promise to log one.
func TestRowReadIsAttributedToTheCaller(t *testing.T) {
	srv, ns := realServerStruct(t)
	if srv.Postgres == nil {
		t.Skip("INTEG_POSTGRES_DSN not set")
	}

	var logged bytes.Buffer
	srv.Log = slog.New(slog.NewJSONHandler(&logged, nil))
	h := api.HandlerFromMuxWithBaseURL(srv, http.NewServeMux(), "/v1")

	code, _ := getAs(t, h, "/v1/projects/"+ns+"/database/tables/players/rows?limit=2",
		auth.Principal{Subject: "u-42", Email: "operator@example.com"})
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}

	out := logged.String()
	if !strings.Contains(out, `"msg":"row read"`) {
		t.Fatalf("no row-read audit line was emitted: %s", out)
	}
	if !strings.Contains(out, `"sub":"u-42"`) {
		t.Errorf("audit line does not name the caller: %s", out)
	}
	if !strings.Contains(out, `"email":"operator@example.com"`) {
		t.Errorf("audit line does not carry the caller's email: %s", out)
	}
}

// An unauthenticated visitor must land on sign-in, not on a shell that fails
// its own API calls — that reads as a broken console rather than as "you are
// not signed in".
func TestUnauthenticatedVisitorIsSentToSignIn(t *testing.T) {
	h := web.Handler("/auth/login")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302 to sign-in", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/auth/login" {
		t.Errorf("Location = %q, want /auth/login", loc)
	}

	// With a session cookie present the shell is served, so a signed-in user is
	// never bounced back to sign-in.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: "__Host-infra-console", Value: "opaque"})
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req)
	if rec2.Code != http.StatusOK {
		t.Errorf("status = %d with a session cookie, want 200", rec2.Code)
	}

	// Local development mode has no sign-in to redirect to.
	rec3 := httptest.NewRecorder()
	web.Handler("").ServeHTTP(rec3, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec3.Code != http.StatusOK {
		t.Errorf("status = %d with no sign-in configured, want 200", rec3.Code)
	}
}
