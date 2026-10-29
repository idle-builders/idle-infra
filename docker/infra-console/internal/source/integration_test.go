// Integration tests for every source, against the real software.
//
// There are no stubs here by policy. Each source is exercised against the
// actual service the platform runs — a real Kubernetes API server, a real
// PostgreSQL, a real Zitadel and a real MinIO — because
// a hand-written fake only ever proves that the code calls the fake. Two real
// defects were found the first time these ran: Zitadel serves org listing from
// the ADMIN api (management returns 404), and it resolves its instance by
// hostname, so 127.0.0.1 fails where localhost succeeds.
//
// Bring the environment up with integration/up.sh, which prints the env below.
// Tests skip cleanly when it is absent, so `go test ./...` still works alone.
package source

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	metricsv "k8s.io/metrics/pkg/client/clientset/versioned"

	"github.com/idle-builders/idle-infra/infra-console/internal/api"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func env(t *testing.T, key string) string {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		t.Skipf("%s not set; run integration/up.sh first", key)
	}
	return v
}

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return c
}

// --------------------------------------------------------------- kubernetes

// realK8s builds a client against the cluster in KUBECONFIG. Not a fake
// clientset: a fake accepts whatever field paths the code asks for, which is
// precisely the class of mistake worth catching.
func realK8s(t *testing.T) (*K8s, string) {
	t.Helper()
	ns := env(t, "INTEG_NAMESPACE")
	if os.Getenv("INTEG_KUBE") == "" {
		t.Skip("INTEG_KUBE not set")
	}

	cfg, err := restConfig()
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
	return NewK8sWithClients(cs, mc), ns
}

func TestRealProjectsDiscoversTheSeededTenant(t *testing.T) {
	k, ns := realK8s(t)

	got, err := k.Projects(ctx(t))
	if err != nil {
		t.Fatalf("Projects: %v", err)
	}

	var found *api0Project
	for i := range got {
		if got[i].Name == ns {
			found = &api0Project{Name: got[i].Name, Schema: got[i].Schema, Health: string(got[i].Health)}
		}
	}
	if found == nil {
		t.Fatalf("seeded namespace %q not discovered by the platform.infra/tenant label; got %d projects", ns, len(got))
	}
	if found.Schema == nil || *found.Schema != ns {
		t.Errorf("schema = %v, want %q", found.Schema, ns)
	}
	if found.Health == "" || found.Health == "unknown" {
		t.Errorf("health = %q, want a real roll-up", found.Health)
	}
}

type api0Project struct {
	Name   string
	Schema *string
	Health string
}

func TestRealNodesMatchTheCluster(t *testing.T) {
	k, _ := realK8s(t)

	got, err := k.Nodes(ctx(t))
	if err != nil {
		t.Fatalf("Nodes: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("no nodes returned from a live cluster")
	}

	// Cross-check against the API server directly, so a wrong field path in
	// the reader cannot agree with itself.
	list, err := k.cs.CoreV1().Nodes().List(ctx(t), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list nodes: %v", err)
	}
	if len(got) != len(list.Items) {
		t.Fatalf("reader returned %d nodes, API server has %d", len(got), len(list.Items))
	}
	for i, n := range got {
		if n.Name != list.Items[i].Name {
			t.Errorf("node %d name = %q, want %q", i, n.Name, list.Items[i].Name)
		}
		if n.KubeletVersion != list.Items[i].Status.NodeInfo.KubeletVersion {
			t.Errorf("kubelet version = %q, want %q", n.KubeletVersion, list.Items[i].Status.NodeInfo.KubeletVersion)
		}
	}
}

func TestRealWorkloadsAndQuota(t *testing.T) {
	k, ns := realK8s(t)

	workloads, err := k.Workloads(ctx(t), ns)
	if err != nil {
		t.Fatalf("Workloads: %v", err)
	}
	var app *string
	var pods []api.Pod
	for _, w := range workloads {
		if w.Name == "app" && string(w.Kind) == "Deployment" {
			app = w.Image
			if w.Pods != nil {
				pods = *w.Pods
			}
		}
	}
	if app == nil {
		t.Fatalf("seeded Deployment not found among %d workloads", len(workloads))
	}
	if *app != env(t, "INTEG_SEED_IMAGE") {
		t.Errorf("image = %q, want the seeded image", *app)
	}

	// ready/replicas says how many pods there should be; this says which exist.
	// A controller with no pods listed is exactly the situation the field is
	// there to make visible, so an empty list must fail rather than pass quietly.
	if len(pods) == 0 {
		t.Fatalf("Deployment app reported no pods; the seeded replica must appear")
	}
	for _, pd := range pods {
		if !strings.HasPrefix(pd.Name, "app-") {
			t.Errorf("pod %q is not owned by the app Deployment", pd.Name)
		}
		if pd.Phase == "" {
			t.Errorf("pod %q has no phase", pd.Name)
		}
		if pd.Node == nil || *pd.Node == "" {
			t.Errorf("pod %q reports no node", pd.Name)
		}
	}

	quota, err := k.Quota(ctx(t), ns)
	if err != nil {
		t.Fatalf("Quota: %v", err)
	}
	// The seeded ResourceQuota is 2 CPU hard; the Deployment requests 50m.
	if quota.CpuHard == nil {
		t.Fatal("quota should report a hard CPU limit from the real ResourceQuota")
	}
	if quota.CpuUsedRatio == nil {
		t.Fatal("quota should report a used ratio")
	}
	if *quota.CpuUsedRatio <= 0 || *quota.CpuUsedRatio > 1 {
		t.Errorf("cpu used ratio = %v, want a fraction of the seeded quota", *quota.CpuUsedRatio)
	}
}

func TestRealIngressesAndTLSSource(t *testing.T) {
	k, ns := realK8s(t)

	got, err := k.Ingresses(ctx(t), ns)
	if err != nil {
		t.Fatalf("Ingresses: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d hostnames, want the 1 seeded Ingress", len(got))
	}
	if got[0].Host != "beta.example.com" {
		t.Errorf("host = %q", got[0].Host)
	}
	// No TLS block on the seeded Ingress, so the edge terminates it.
	if string(got[0].TlsSource) != "cloudflare-universal" {
		t.Errorf("tlsSource = %q, want cloudflare-universal for an Ingress with no TLS block", got[0].TlsSource)
	}
	if got[0].RoutesTo == nil || *got[0].RoutesTo != ns+"/app" {
		t.Errorf("routesTo = %v, want %s/app", got[0].RoutesTo, ns)
	}
}

// Helm's real storage encoding, written by kubectl into a real ConfigMap.
func TestRealHelmReleaseDecodes(t *testing.T) {
	k, ns := realK8s(t)

	releases, err := k.HelmRevisions(ctx(t), ns)
	if err != nil {
		t.Fatalf("Releases: %v", err)
	}
	if len(releases) != 1 {
		t.Fatalf("got %d releases, want 1", len(releases))
	}
	r := releases[0]
	if r.Revision != 41 || r.Status != "deployed" {
		t.Errorf("got revision %d status %q", r.Revision, r.Status)
	}
	if r.ChartVersion != "0.4.0" || r.AppVersion != "1.3.0" {
		t.Errorf("chart/app = %q/%q", r.ChartVersion, r.AppVersion)
	}
	if r.ImageTag != env(t, "INTEG_SEED_SHA") {
		t.Errorf("image tag = %q, want the commit SHA", r.ImageTag)
	}
	if r.UpdatedAt.IsZero() {
		t.Error("UpdatedAt should decode from info.last_deployed")
	}
}

func TestRealNamespaceExistsRejectsNonTenants(t *testing.T) {
	k, ns := realK8s(t)
	c := ctx(t)

	ok, err := k.NamespaceExists(c, ns)
	if err != nil || !ok {
		t.Fatalf("seeded namespace: ok=%v err=%v", ok, err)
	}
	// kube-system exists but carries no tenant label.
	ok, err = k.NamespaceExists(c, "kube-system")
	if err != nil {
		t.Fatalf("kube-system: %v", err)
	}
	if ok {
		t.Error("kube-system must not be treated as a tenant project")
	}
}

// ----------------------------------------------------------------- postgres

func realPostgres(t *testing.T) (*PostgresClient, string) {
	t.Helper()
	dsn := env(t, "INTEG_POSTGRES_DSN")
	schema := env(t, "INTEG_POSTGRES_SCHEMA")

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.PingContext(ctx(t)); err != nil {
		t.Fatalf("ping: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewPostgresClient(db, 100, 10*time.Second), schema
}

// The seeded schema name contains a hyphen on purpose: tenant schemas are
// Kubernetes namespaces, DNS labels allow hyphens, and the contract's project
// pattern permits them. An identifier check that rejected hyphens would make
// any such tenant unbrowsable while the API still accepted the project.
func TestRealHyphenatedSchemaIsBrowsable(t *testing.T) {
	p, schema := realPostgres(t)

	page, err := p.Rows(ctx(t), schema, "players", 10, "")
	if err != nil {
		t.Fatalf("Rows on hyphenated schema %q: %v", schema, err)
	}
	if len(page.Rows) != 3 {
		t.Fatalf("got %d rows, want 3", len(page.Rows))
	}
}

func TestRealMaskingHappensInSQL(t *testing.T) {
	p, schema := realPostgres(t)

	page, err := p.Rows(ctx(t), schema, "players", 10, "")
	if err != nil {
		t.Fatalf("Rows: %v", err)
	}
	masked := map[string]bool{}
	for _, c := range page.MaskedColumns {
		masked[c] = true
	}
	if len(masked) != 0 {
		t.Errorf("maskedColumns = %v, but the owner granted the whole table", page.MaskedColumns)
	}
	for _, row := range page.Rows {
		if row["id"] == nil {
			t.Error("the primary key must not be masked")
		}
	}

	// The values are still in the database — masking is a projection, not a
	// mutation.
	var email string
	if err := p.db.QueryRowContext(ctx(t),
		`SELECT email FROM "`+schema+`".players WHERE id='a1'`).Scan(&email); err != nil {
		t.Fatalf("direct read: %v", err)
	}
	if email != "nord@example.com" {
		t.Fatalf("database value = %q, want it untouched", email)
	}
}

func TestRealKeysetPagingHasNoOverlap(t *testing.T) {
	p, schema := realPostgres(t)
	c := ctx(t)

	first, err := p.Rows(c, schema, "players", 2, "")
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if !first.Truncated || first.NextCursor == nil {
		t.Fatalf("first page should be truncated with a cursor: %+v", first.Truncated)
	}
	second, err := p.Rows(c, schema, "players", 2, *first.NextCursor)
	if err != nil {
		t.Fatalf("second page: %v", err)
	}
	seen := map[any]bool{}
	for _, r := range first.Rows {
		seen[r["id"]] = true
	}
	for _, r := range second.Rows {
		if seen[r["id"]] {
			t.Fatalf("row %v appeared on both pages", r["id"])
		}
	}
}

func TestRealCompositeKeyTableRefusesPaging(t *testing.T) {
	p, schema := realPostgres(t)
	if _, err := p.Rows(ctx(t), schema, "memberships", 10, ""); !IsNoSingleColumnPK(err) {
		t.Fatalf("error = %v, want the unpageable-table error", err)
	}
}

func TestRealInjectionAttemptsLeaveDataIntact(t *testing.T) {
	p, schema := realPostgres(t)
	c := ctx(t)

	for _, bad := range []string{
		`players"; DROP TABLE players; --`,
		"players; SELECT 1",
		"pg_catalog.pg_shadow",
		"../players",
		"",
	} {
		if _, err := p.Rows(c, schema, bad, 10, ""); err == nil {
			t.Errorf("table %q was accepted", bad)
		}
	}

	var n int
	if err := p.db.QueryRowContext(c, `SELECT count(*) FROM "`+schema+`".players`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 3 {
		t.Fatalf("row count = %d, want 3 — an injection attempt altered data", n)
	}
}

func TestRealCatalogNumbersMatchPostgres(t *testing.T) {
	p, schema := realPostgres(t)
	c := ctx(t)

	summary, err := p.Summary(c, schema)
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}

	var wantTables int
	var wantBytes int64
	if err := p.db.QueryRowContext(c, `
		SELECT count(*), COALESCE(SUM(pg_total_relation_size(c.oid)),0)::bigint
		FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
		WHERE n.nspname=$1 AND c.relkind='r'`, schema).Scan(&wantTables, &wantBytes); err != nil {
		t.Fatalf("ground truth: %v", err)
	}
	if summary.TableCount != wantTables {
		t.Errorf("tableCount = %d, want %d", summary.TableCount, wantTables)
	}
	if summary.SizeBytes != wantBytes {
		t.Errorf("sizeBytes = %d, want %d", summary.SizeBytes, wantBytes)
	}

	tables, err := p.Tables(c, schema)
	if err != nil {
		t.Fatalf("Tables: %v", err)
	}
	if len(tables) != wantTables {
		t.Fatalf("got %d tables, want %d", len(tables), wantTables)
	}
	for _, tbl := range tables {
		if tbl.Name == "memberships" && (tbl.HasSingleColumnPrimaryKey == nil || *tbl.HasSingleColumnPrimaryKey) {
			t.Error("memberships has a composite key and must be flagged unpageable")
		}
		if tbl.Name == "sessions" && tbl.RowsEstimate < 100 {
			t.Errorf("sessions rowsEstimate = %d, want ~120 after ANALYZE", tbl.RowsEstimate)
		}
	}
}

// ------------------------------------------------------------------ zitadel

func TestRealZitadelSummary(t *testing.T) {
	base := env(t, "INTEG_ZITADEL_URL")
	token := env(t, "INTEG_ZITADEL_TOKEN")
	org := env(t, "INTEG_ZITADEL_ORG")

	z := NewZitadelClient(base, token)
	got, err := z.Summary(ctx(t), org)
	if err != nil {
		t.Fatalf("Summary against real Zitadel: %v", err)
	}
	if got.Org != org {
		t.Errorf("org = %q, want %q", got.Org, org)
	}
	if got.OrgId == nil || *got.OrgId == "" {
		t.Error("orgId should come from the real org record")
	}
	// The harness registers one PKCE public client in this org.
	if len(got.Applications) != 1 {
		t.Fatalf("got %d applications, want the 1 seeded OIDC app", len(got.Applications))
	}
	app := got.Applications[0]
	if app.Name != "web" {
		t.Errorf("application name = %q, want the seeded app", app.Name)
	}
	if app.Type == "" {
		t.Errorf("application type is empty: %+v", app)
	}
	if app.RedirectUris == nil || len(*app.RedirectUris) != 1 ||
		(*app.RedirectUris)[0] != "https://beta.example.com/callback" {
		t.Errorf("redirectUris = %v, want the registered one", app.RedirectUris)
	}
	// Two: the seeded human, plus the machine user the harness creates to
	// prove a valid non-operator credential is refused.
	if got.UserCount == nil || *got.UserCount != 2 {
		t.Errorf("userCount = %v, want the 2 seeded users", got.UserCount)
	}
	if got.ConsoleUrl == "" {
		t.Error("consoleUrl should deep-link into Zitadel")
	}
}

// proto3 JSON omits zero values, so an org with no users comes back with no
// totalResult field at all. That must read as 0, not as "unknown" — a missing
// user count and a count of zero mean very different things to an operator.
func TestRealZitadelEmptyOrgReportsZeroNotUnknown(t *testing.T) {
	z := NewZitadelClient(env(t, "INTEG_ZITADEL_URL"), env(t, "INTEG_ZITADEL_TOKEN"))

	got, err := z.Summary(ctx(t), env(t, "INTEG_ZITADEL_EMPTY_ORG"))
	if err != nil {
		t.Fatalf("Summary for an empty org: %v", err)
	}
	if got.UserCount == nil {
		t.Fatal("userCount is absent for an org with no users; it must be 0")
	}
	if *got.UserCount != 0 {
		t.Errorf("userCount = %d, want 0", *got.UserCount)
	}
}

func TestRealZitadelUnknownOrg(t *testing.T) {
	z := NewZitadelClient(env(t, "INTEG_ZITADEL_URL"), env(t, "INTEG_ZITADEL_TOKEN"))
	if _, err := z.Summary(ctx(t), "no-such-org-here"); err == nil {
		t.Fatal("an unknown org must be a clear error, not an empty success")
	}
}

// -------------------------------------------------------------------- github

// The real GitHub API, against an explicitly selected fixture repository.
//
// GitHub is a hosted service with no self-hostable server, so this is one of
// two sources that cannot be stood up locally. It runs against the real API
// whenever a token is present and skips only when there is none — never
// against a recorded response.
//
// The fixture is a real merge commit on main with a real workflow run. That is
// exactly the lookup the console performs: a Helm release carries the image
// tag, the image tag IS the commit SHA (docs/tenant-deployment-guide.md sets
// image.tag=${github.sha}), and the SHA resolves to the run that built it.
func TestRealGitHubResolvesACommitToItsRun(t *testing.T) {
	token := os.Getenv("INTEG_GITHUB_TOKEN")
	if token == "" {
		t.Skip("INTEG_GITHUB_TOKEN not set: GitHub has no local equivalent, so the deployment timeline's actor and duration are UNVERIFIED until a token is supplied (gh auth token)")
	}

	g := NewGitHubClient(token)
	if !g.Configured() {
		t.Fatal("a non-empty token must produce a configured client")
	}

	// The same commit the harness seeds as the image tag, so the UI and this
	// test exercise one chain rather than two.
	sha := env(t, "INTEG_SEED_SHA")

	owner, repo := os.Getenv("INTEG_GITHUB_OWNER"), os.Getenv("INTEG_GITHUB_REPO")
	if owner == "" || repo == "" {
		t.Fatal("INTEG_GITHUB_OWNER and INTEG_GITHUB_REPO must identify the repository containing INTEG_SEED_SHA")
	}
	run, err := g.RunForCommit(ctx(t), owner, repo, sha)
	if err != nil {
		t.Fatalf("RunForCommit against the real API: %v", err)
	}
	if run == nil {
		t.Fatal("no run found for a commit that has one; the console would show a deployment with no provenance")
	}
	if run.Actor == "" {
		t.Error("actor is empty; the timeline's \"who deployed this\" column comes from here")
	}
	if run.Conclusion == "" {
		t.Error("conclusion is empty; a finished run has one")
	}
	// durationSeconds on the Deployment comes from these two timestamps; a
	// finished run must produce a positive wall time.
	if run.Duration() <= 0 {
		t.Errorf("duration = %s, want the real run's wall time (started %s, completed %s)",
			run.Duration(), run.StartedAt, run.CompletedAt)
	}
}

// -------------------------------------------------------------- object store

// Exact object-store metadata is verified against a real Swift+Keystone pair
// when those credentials are supplied. MinIO remains in the local harness for
// S3 telemetry, but it cannot honestly stand in for this protocol.
func TestRealObjectStoreMetadata(t *testing.T) {
	authURL := env(t, "INTEG_SWIFT_AUTH_URL")
	endpoint := env(t, "INTEG_SWIFT_ENDPOINT")
	bucket := env(t, "INTEG_SWIFT_BUCKET")

	o := NewObjectStoreClient(authURL, endpoint, os.Getenv("INTEG_S3_ENDPOINT"),
		env(t, "INTEG_SWIFT_APPLICATION_CREDENTIAL_ID"),
		env(t, "INTEG_SWIFT_APPLICATION_CREDENTIAL_SECRET"))

	got, err := o.Bucket(ctx(t), bucket)
	if err != nil {
		t.Fatalf("Bucket against real Swift metadata: %v", err)
	}
	if got.Bucket != bucket {
		t.Errorf("bucket = %q, want %q", got.Bucket, bucket)
	}
	if got.ObjectCount < 1 || got.BytesUsed < 1 {
		t.Errorf("provider counters = %d objects / %d bytes, want non-empty live bucket",
			got.ObjectCount, got.BytesUsed)
	}
}

func TestObjectStoreUnconfiguredReportsItself(t *testing.T) {
	o := NewObjectStoreClient("", "", "", "", "")
	if o.Configured() {
		t.Error("no endpoint or credentials must not count as configured")
	}
	if _, err := o.Bucket(context.Background(), "b"); err == nil {
		t.Error("unconfigured client must error so the section degrades")
	}
}

// ---------------------------------------------------------------- cloudflare

// Cloudflare is a hosted service with no self-hostable API server, so this is
// the one source that cannot be stood up locally. Rather than fake it, the
// test runs against the REAL Cloudflare API when a token is present and skips
// otherwise — so the client is verified the moment a credential exists, and
// never gives false confidence before that.
func TestRealCloudflare(t *testing.T) {
	token := os.Getenv("INTEG_CLOUDFLARE_TOKEN")
	if token == "" {
		t.Skip("INTEG_CLOUDFLARE_TOKEN not set: Cloudflare has no local equivalent, so this client is UNVERIFIED until a real read-only token is supplied")
	}

	c := NewCloudflareClient(token, os.Getenv("INTEG_CLOUDFLARE_ACCOUNT"))
	hostnames, err := c.Hostnames(ctx(t))
	if err != nil {
		t.Fatalf("Hostnames against real Cloudflare: %v", err)
	}
	for _, h := range hostnames {
		if h.Host == "" || h.RecordType == "" {
			t.Errorf("hostname decoded with empty fields: %+v", h)
		}
	}
	// Records() must cover the whole estate, not just the record types that
	// can front a service. The edge page counts these, and a count that
	// silently omits MX and TXT is wrong while looking authoritative.
	records, err := c.Records(ctx(t))
	if err != nil {
		t.Fatalf("Records against real Cloudflare: %v", err)
	}
	if len(records) < len(hostnames) {
		t.Fatalf("Records returned %d < Hostnames %d: the full estate cannot be smaller than the subset that fronts services",
			len(records), len(hostnames))
	}
	kinds := map[string]int{}
	for _, r := range records {
		if r.Zone == "" || r.Name == "" || r.Type == "" {
			t.Errorf("record decoded with empty fields: %+v", r)
		}
		kinds[r.Type]++
	}
	// This platform's zones carry mail and verification records. If none come
	// back, the type filter is still in the path somewhere.
	if kinds["TXT"] == 0 && kinds["MX"] == 0 {
		t.Errorf("no TXT or MX records across %d records; the edge estate is being filtered", len(records))
	}
	t.Logf("cloudflare estate: %d records by type %v", len(records), kinds)

	if os.Getenv("INTEG_CLOUDFLARE_ACCOUNT") != "" {
		if _, err := c.Tunnel(ctx(t)); err != nil {
			t.Errorf("Tunnel against real Cloudflare: %v", err)
		}
	}
}

// The broker policy is the authority for namespace -> bucket. Deriving the
// name from the namespace would 404 against the platform's real buckets
// ("<app>-files"), or worse, name a different tenant's bucket.
func TestRealBucketForComesFromTheBrokerPolicy(t *testing.T) {
	k, ns := realK8s(t)
	want := env(t, "INTEG_S3_BUCKET")

	got, err := k.BucketFor(ctx(t), ns)
	if err != nil {
		t.Fatalf("BucketFor: %v", err)
	}
	if got != want {
		t.Errorf("bucket = %q, want %q from the broker policy", got, want)
	}
	if got == ns || got == ns+"-app" {
		t.Errorf("bucket %q looks derived from the namespace rather than read from the policy", got)
	}
}

func TestRealBucketForRefusesAnUnmappedNamespace(t *testing.T) {
	k, _ := realK8s(t)

	// kube-system is a real namespace with no storage grant. Returning a
	// derived name here would point the storage panel at a bucket nobody
	// granted this namespace.
	if got, err := k.BucketFor(ctx(t), "kube-system"); err == nil {
		t.Errorf("BucketFor returned %q for an unmapped namespace; it must error", got)
	}
}

// A primary key must never be masked. It is the pagination cursor, read back
// out of the projected row — masked, it is NULL, so the page returns
// truncated with no nextCursor and the browser dead-ends with nothing
// explaining why. "hash" is a real primary key on content-addressed tables and
// matches a masking token.
func TestRealRowsPageAcrossAMaskingNamedPrimaryKey(t *testing.T) {
	p, schema := realPostgres(t)

	page, err := p.Rows(ctx(t), schema, "artifacts", 50, "")
	if err != nil {
		t.Fatalf("Rows: %v", err)
	}
	if !page.Truncated {
		t.Fatalf("120 seeded rows with limit 50 should truncate")
	}
	for _, c := range page.Columns {
		if c.Name == "hash" && c.Masked {
			t.Error("the primary key was masked; its cursor cannot survive the projection")
		}
	}
	if page.NextCursor == nil || *page.NextCursor == "" {
		t.Fatal("a truncated page must carry a nextCursor, or the browser cannot page")
	}

	// And the cursor must actually advance.
	next, err := p.Rows(ctx(t), schema, "artifacts", 50, *page.NextCursor)
	if err != nil {
		t.Fatalf("second page: %v", err)
	}
	if len(next.Rows) == 0 {
		t.Fatal("the second page is empty; the cursor did not advance")
	}
	if fmt.Sprint(next.Rows[0]["hash"]) == fmt.Sprint(page.Rows[0]["hash"]) {
		t.Error("the second page repeats the first")
	}
}

// Masking is the owner's declaration, enforced by PostgreSQL: a column is
// withheld exactly when platform_reader has no SELECT privilege on it. The
// harness grants SELECT (id, login_count, created_at) on accounts and NOT the
// table, so metadata and notes are unreadable.
func TestRealRowsMaskExactlyWhatTheOwnerWithheld(t *testing.T) {
	p, schema := realPostgres(t)

	page, err := p.Rows(ctx(t), schema, "accounts", 10, "")
	if err != nil {
		t.Fatalf("Rows: %v", err)
	}
	if len(page.Rows) == 0 {
		t.Fatal("expected the seeded row")
	}

	masked := map[string]bool{}
	for _, c := range page.Columns {
		masked[c.Name] = c.Masked
	}
	for _, name := range []string{"metadata", "notes"} {
		if !masked[name] {
			t.Errorf("%s was not granted and must be masked", name)
		}
		if page.Rows[0][name] != nil {
			t.Errorf("%s leaked a value: %v", name, page.Rows[0][name])
		}
	}
	// Granted columns must come through, or the panel is useless.
	for _, name := range []string{"id", "login_count", "created_at"} {
		if masked[name] {
			t.Errorf("%s was granted and must not be masked", name)
		}
	}
	if page.Rows[0]["login_count"] == nil {
		t.Error("a granted column lost its value")
	}

	body, _ := json.Marshal(page.Rows)
	for _, secret := range []string{"sk_live_DEADBEEF", "123-45-6789"} {
		if strings.Contains(string(body), secret) {
			t.Errorf("secret %q reached the response", secret)
		}
	}
}

// The console must not withhold anything the owner did grant. players carries
// table-level SELECT, so every column is readable — including ones whose names
// look sensitive. Guessing from names is exactly what this replaced.
func TestRealRowsWithholdNothingTheOwnerGranted(t *testing.T) {
	p, schema := realPostgres(t)

	page, err := p.Rows(ctx(t), schema, "players", 10, "")
	if err != nil {
		t.Fatalf("Rows: %v", err)
	}
	if len(page.MaskedColumns) != 0 {
		t.Errorf("maskedColumns = %v, but the owner granted the whole table",
			page.MaskedColumns)
	}
	for _, row := range page.Rows {
		for _, name := range []string{"email", "api_key", "display_name"} {
			if row[name] == nil {
				t.Errorf("%s was granted but came back NULL", name)
			}
		}
	}
}

// platform_reader cannot see state or query for sessions it does not own, so
// the counts would be permanent zeros. That must degrade, not lie.
//
// The session below is owned by a role named after the schema, which is how
// the platform names tenant roles — exactly the case the handler filters on.
func TestRealActivityDegradesUnderRestrictedRole(t *testing.T) {
	p, schema := realPostgres(t)

	admin, err := sql.Open("pgx", env(t, "INTEG_POSTGRES_SUPERUSER_DSN"))
	if err != nil {
		t.Fatalf("open superuser: %v", err)
	}
	defer func() { _ = admin.Close() }()

	// The harness already creates a role named after the schema, owning it —
	// the shape modules/tenant produces. Use it rather than making another.

	// A live session owned by that role, so there is something to be blind to.
	dsn := strings.Replace(env(t, "INTEG_POSTGRES_SUPERUSER_DSN"),
		"postgres:integration@", schema+":tenant-integration@", 1)
	tenant, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open tenant: %v", err)
	}
	defer func() { _ = tenant.Close() }()
	if err := tenant.PingContext(ctx(t)); err != nil {
		t.Fatalf("tenant session: %v", err)
	}

	if _, err := p.Activity(ctx(t), schema); err == nil {
		t.Fatal("Activity returned counts it could not actually see; " +
			"platform_reader cannot read state for another role's sessions, " +
			"so those zeros would be fabricated")
	}
}

// AC4, and the regression guard it lacked: platform_reader must see a table
// created AFTER the role existed, with no new grant.
//
// The harness reproduces production's shape — ALTER DEFAULT PRIVILEGES FOR ROLE
// <tenant>, matching Terraform's owner = each.value — but nothing asserted on
// it, so deleting the FOR ROLE clause (the exact regression just fixed) would
// have left the suite green.
func TestRealDefaultPrivilegesCoverALaterTable(t *testing.T) {
	p, schema := realPostgres(t)

	admin, err := sql.Open("pgx", env(t, "INTEG_POSTGRES_SUPERUSER_DSN"))
	if err != nil {
		t.Fatalf("open superuser: %v", err)
	}
	// t.Cleanup, not defer: cleanups run AFTER the function's deferred calls,
	// so a deferred Close here would shut the pool before a t.Cleanup drop
	// could use it — the table would survive and the next run of this test
	// would fail with "already exists".
	t.Cleanup(func() { _ = admin.Close() })

	// Created BY THE TENANT ROLE, because default privileges are owner-scoped:
	// a table created by anyone else is not covered by FOR ROLE <tenant>.
	const name = "ac4_later"
	if _, err := admin.ExecContext(ctx(t),
		`SET ROLE `+pgQuote(schema)+`; CREATE TABLE `+pgQuote(schema)+`.`+name+
			` (id bigserial PRIMARY KEY, note text); RESET ROLE`); err != nil {
		t.Fatalf("create table as the tenant role: %v", err)
	}
	// Registered after the CREATE succeeded and before Close is reached, and
	// the error is checked: silently discarding it is how the leak hid.
	t.Cleanup(func() {
		if _, err := admin.ExecContext(context.Background(),
			`DROP TABLE IF EXISTS `+pgQuote(schema)+`.`+name); err != nil {
			t.Errorf("could not drop %s, the next run will fail: %v", name, err)
		}
	})

	// No grant is issued here on purpose. If default privileges are wrong, this
	// is where it shows.
	if _, err := p.Rows(ctx(t), schema, name, 5, ""); err != nil {
		t.Fatalf("platform_reader cannot read a table created after the role "+
			"existed — default privileges are not owner-scoped as production "+
			"declares them: %v", err)
	}
}

// SELECT-only is a property of the role, not of the console's SQL.
func TestRealPlatformReaderCannotWrite(t *testing.T) {
	p, schema := realPostgres(t)

	for _, stmt := range []string{
		`INSERT INTO ` + pgQuote(schema) + `.players (id, display_name) VALUES ('zz','x')`,
		`CREATE TABLE ` + pgQuote(schema) + `.zz_should_not_exist (id int)`,
		`DELETE FROM ` + pgQuote(schema) + `.players`,
	} {
		if _, err := p.db.ExecContext(ctx(t), stmt); err == nil {
			t.Errorf("platform_reader executed a write: %s", stmt)
		}
	}
}

func pgQuote(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }
