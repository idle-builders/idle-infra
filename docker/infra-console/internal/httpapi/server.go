// Package httpapi implements the generated ServerInterface over the sources.
//
// Aggregates never fail whole: a dead backend becomes one unavailable section
// and the rest of the screen renders. Single-resource endpoints do fail whole,
// with 503 and a Problem naming the source.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"time"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/idle-builders/idle-infra/infra-console/internal/api"
	"github.com/idle-builders/idle-infra/infra-console/internal/auth"
	"github.com/idle-builders/idle-infra/infra-console/internal/source"
)

// TTLs groups the per-source cache lifetimes.
type TTLs struct {
	Kubernetes  time.Duration
	Postgres    time.Duration
	Zitadel     time.Duration
	ObjectStore time.Duration
	Cloudflare  time.Duration
	GitHub      time.Duration
}

// Server implements api.ServerInterface.
type Server struct {
	K8s *source.K8s
	// GitHub is optional. Without it the deployment timeline still renders,
	// reporting source "helm" and leaving actor and duration absent.
	GitHub      *source.GitHubClient
	Cloudflare  *source.CloudflareClient
	ObjectStore *source.ObjectStoreClient
	Telemetry   source.TelemetrySource
	Zitadel     *source.ZitadelClient
	// Postgres is optional and gated on the platform_reader grants existing.
	// Without it the database panel reports itself unavailable.
	Postgres *source.PostgresClient
	// BucketFor overrides bucket resolution. Normally nil: the mapping comes
	// from the storage broker's policy, which is the component that actually
	// signs URLs against these buckets.
	BucketFor func(ctx context.Context, project string) (string, error)
	Cache     *source.Cache
	TTL       TTLs
	Log       *slog.Logger
}

var _ api.ServerInterface = (*Server)(nil)

// ---------------------------------------------------------------- platform

func (s *Server) GetPlatform(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	nodes := source.Fetch(ctx, s.Cache, source.Kubernetes, source.Key(source.Kubernetes, "nodes"),
		s.TTL.Kubernetes, s.K8s.Nodes)
	capacity := source.Fetch(ctx, s.Cache, source.Kubernetes, source.Key(source.Kubernetes, "capacity"),
		s.TTL.Kubernetes, s.K8s.Capacity)
	services := source.Fetch(ctx, s.Cache, source.Kubernetes, source.Key(source.Kubernetes, "services"),
		s.TTL.Kubernetes, s.K8s.SharedServices)
	projects := source.Fetch(ctx, s.Cache, source.Kubernetes, source.Key(source.Kubernetes, "projects"),
		s.TTL.Kubernetes, s.K8s.Projects)

	backups := source.Fetch(ctx, s.Cache, source.Kubernetes,
		source.Key(source.Kubernetes, "backups"), s.TTL.Kubernetes, s.K8s.BackupStatus)

	out := map[string]any{
		"nodes":    sectionJSON(nodes),
		"capacity": sectionJSON(capacity),
		"services": sectionJSON(services),
		"projects": sectionJSON(projects),
		"backups":  sectionJSON(backups),
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) ListNodes(w http.ResponseWriter, r *http.Request) {
	nodes, err := s.K8s.Nodes(r.Context())
	if err != nil {
		s.sourceUnavailable(w, source.Kubernetes, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"nodes": nodes, "fetchedAt": time.Now().UTC()})
}

func (s *Server) ListSharedServices(w http.ResponseWriter, r *http.Request) {
	services, err := s.K8s.SharedServices(r.Context())
	if err != nil {
		s.sourceUnavailable(w, source.Kubernetes, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"services": services, "fetchedAt": time.Now().UTC()})
}

// ---------------------------------------------------------- observability

var telemetryFilter = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

func (s *Server) GetObservability(w http.ResponseWriter, r *http.Request, params api.GetObservabilityParams) {
	if s.Telemetry == nil || !s.Telemetry.Configured() {
		notImplemented(w, source.Telemetry)
		return
	}
	windowMinutes := 60
	if params.WindowMinutes != nil {
		windowMinutes = *params.WindowMinutes
	}
	limit := 50
	if params.Limit != nil {
		limit = *params.Limit
	}
	if windowMinutes < 5 || windowMinutes > 2880 || limit < 1 || limit > 100 {
		problem(w, http.StatusBadRequest, "Invalid observability query", "", "windowMinutes or limit is outside its allowed range")
		return
	}
	filter := func(value *string) (string, bool) {
		if value == nil || *value == "" {
			return "", true
		}
		return *value, len(*value) <= 253 && telemetryFilter.MatchString(*value)
	}
	namespace, namespaceOK := filter(params.Namespace)
	workload, workloadOK := filter(params.Workload)
	service, serviceOK := filter(params.Service)
	if !namespaceOK || !workloadOK || !serviceOK {
		problem(w, http.StatusBadRequest, "Invalid observability query", "", "namespace, workload, or service contains unsupported characters")
		return
	}

	to := time.Now().UTC()
	query := source.TelemetryQuery{
		From: to.Add(-time.Duration(windowMinutes) * time.Minute),
		To:   to, Namespace: namespace, Workload: workload, Service: service, Limit: limit,
	}
	out, err := s.Telemetry.Overview(r.Context(), query)
	if err != nil {
		s.sourceUnavailable(w, source.Telemetry, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// ---------------------------------------------------------------- projects

func (s *Server) ListProjects(w http.ResponseWriter, r *http.Request) {
	projects, err := s.K8s.Projects(r.Context())
	if err != nil {
		s.sourceUnavailable(w, source.Kubernetes, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"projects": projects, "fetchedAt": time.Now().UTC()})
}

func (s *Server) GetProject(w http.ResponseWriter, r *http.Request, project api.Project) {
	ctx := r.Context()
	name := string(project)
	if !s.projectExists(ctx, w, name) {
		return
	}

	workloads := source.Fetch(ctx, s.Cache, source.Kubernetes,
		source.Key(source.Kubernetes, "workloads", name), s.TTL.Kubernetes,
		func(ctx context.Context) ([]api.Workload, error) { return s.K8s.Workloads(ctx, name) })

	quota := source.Fetch(ctx, s.Cache, source.Kubernetes,
		source.Key(source.Kubernetes, "quota", name), s.TTL.Kubernetes,
		func(ctx context.Context) (api.Quota, error) { return s.K8s.Quota(ctx, name) })

	domains := source.Fetch(ctx, s.Cache, source.Kubernetes,
		source.Key(source.Kubernetes, "ingresses", name), s.TTL.Kubernetes,
		func(ctx context.Context) ([]api.Hostname, error) { return s.K8s.Ingresses(ctx, name) })

	database := source.UnavailableSection[api.DatabaseSummary](source.Postgres, errNotWired)
	if s.Postgres != nil && s.Postgres.Configured() {
		database = source.Fetch(ctx, s.Cache, source.Postgres,
			source.Key(source.Postgres, name), s.TTL.Postgres,
			func(ctx context.Context) (api.DatabaseSummary, error) {
				return s.Postgres.Summary(ctx, name)
			})
	}
	identity := source.UnavailableSection[api.AuthSummary](source.Zitadel, errNotWired)
	if s.Zitadel != nil && s.Zitadel.Configured() {
		identity = source.Fetch(ctx, s.Cache, source.Zitadel,
			source.Key(source.Zitadel, name), s.TTL.Zitadel,
			func(ctx context.Context) (api.AuthSummary, error) {
				return s.Zitadel.Summary(ctx, name)
			})
	}

	storage := source.UnavailableSection[api.StorageSummary](source.ObjectStore, errNotWired)
	if s.ObjectStore != nil && s.ObjectStore.Configured() {
		// Resolving the bucket can itself fail (no broker policy, or no entry
		// for this namespace). That is a degraded section, not a broken page.
		if bucket, err := s.bucketFor(ctx, name); err != nil {
			storage = source.UnavailableSection[api.StorageSummary](source.ObjectStore, err)
		} else {
			storage = source.Fetch(ctx, s.Cache, source.ObjectStore,
				source.Key(source.ObjectStore, bucket), s.TTL.ObjectStore,
				func(ctx context.Context) (api.StorageSummary, error) {
					return s.ObjectStore.Bucket(ctx, bucket)
				})
		}
	}

	out := map[string]any{
		"name":      name,
		"namespace": name,
		"workloads": sectionJSON(workloads),
		"quota":     sectionJSON(quota),
		"database":  sectionJSON(database),
		"storage":   sectionJSON(storage),
		"auth":      sectionJSON(identity),
		"domains":   sectionJSON(domains),
	}
	// The running image is a convenience header on the overview; take it from
	// the first workload that has one.
	if workloads.Data != nil {
		for _, wl := range *workloads.Data {
			if wl.Image != nil {
				out["image"] = *wl.Image
				break
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// --------------------------------------------------------------- workloads

func (s *Server) ListWorkloads(w http.ResponseWriter, r *http.Request, project api.Project) {
	ctx := r.Context()
	name := string(project)
	if !s.workloadNamespaceExists(ctx, w, name) {
		return
	}
	workloads, err := s.K8s.Workloads(ctx, name)
	if err != nil {
		s.sourceUnavailable(w, source.Kubernetes, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"workloads": workloads, "fetchedAt": time.Now().UTC()})
}

func (s *Server) ListReleases(w http.ResponseWriter, r *http.Request, project api.Project, params api.ListReleasesParams) {
	ctx := r.Context()
	name := string(project)
	if !s.projectExists(ctx, w, name) {
		return
	}

	limit := 20
	if params.Limit != nil {
		limit = *params.Limit
	}

	releases, err := s.K8s.ReleaseHistory(ctx, name, limit)
	if err != nil {
		s.sourceUnavailable(w, source.Kubernetes, err)
		return
	}

	s.enrichFromGitHub(ctx, name, releases)
	writeJSON(w, http.StatusOK, map[string]any{"releases": releases, "fetchedAt": time.Now().UTC()})
}

func (s *Server) GetWorkloadLogs(w http.ResponseWriter, r *http.Request, project api.Project, pod string, params api.GetWorkloadLogsParams) {
	ctx := r.Context()
	name := string(project)
	if !s.projectExists(ctx, w, name) {
		return
	}

	opts := source.LogOptions{TailLines: 200}
	if params.Container != nil {
		opts.Container = *params.Container
	}
	if params.TailLines != nil {
		opts.TailLines = *params.TailLines
	}
	if params.Previous != nil {
		opts.Previous = *params.Previous
	}

	logs, err := s.K8s.Logs(ctx, name, pod, opts)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			problem(w, http.StatusNotFound, "No such pod", source.Kubernetes, "")
			return
		}
		s.sourceUnavailable(w, source.Kubernetes, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(logs))
}

func (s *Server) ListProjectDomains(w http.ResponseWriter, r *http.Request, project api.Project) {
	ctx := r.Context()
	name := string(project)
	if !s.projectExists(ctx, w, name) {
		return
	}
	hostnames, err := s.K8s.Ingresses(ctx, name)
	if err != nil {
		s.sourceUnavailable(w, source.Kubernetes, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"hostnames": hostnames, "fetchedAt": time.Now().UTC()})
}

// ------------------------------------------------------------------- meta

// --------------------------------------------------- unconfigured sources

// errNotWired marks a source with no credential in this deployment: reported
// per-section on aggregates and as 501 on dedicated endpoints, so the gap is
// visible rather than silently empty.
var errNotWired = errors.New("source not configured")

func (s *Server) GetBackupStatus(w http.ResponseWriter, r *http.Request) {
	status, err := s.K8s.BackupStatus(r.Context())
	if err != nil {
		s.sourceUnavailable(w, source.Kubernetes, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}
func (s *Server) GetEdge(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if s.Cloudflare == nil || !s.Cloudflare.Configured() {
		notImplemented(w, source.Cloudflare)
		return
	}

	tunnel := source.Fetch(ctx, s.Cache, source.Cloudflare,
		source.Key(source.Cloudflare, "tunnel"), s.TTL.Cloudflare, s.Cloudflare.Tunnel)

	// Derive zones and the count INSIDE the fetch so they live in one Section.
	// Outside, a failed read still produced [] and 0 as bare fields — which
	// reads as "the account is empty", not "Cloudflare was unreachable".
	hostnames := source.Fetch(ctx, s.Cache, source.Cloudflare,
		source.Key(source.Cloudflare, "edge-hostnames"), s.TTL.Cloudflare,
		func(ctx context.Context) (api.EdgeHostnames, error) {
			// Records(), not Hostnames(): the edge view is the DNS estate, so
			// it must count and list MX and TXT too. Zone names come from the
			// records themselves, which carry the zone Cloudflare reported —
			// the old suffix-splitting guessed the apex from the host name.
			records, err := s.Cloudflare.Records(ctx)
			if err != nil {
				return api.EdgeHostnames{}, err
			}
			seen := map[string]bool{}
			for _, r := range records {
				seen[r.Zone] = true
			}
			zones := make([]string, 0, len(seen))
			for z := range seen {
				zones = append(zones, z)
			}
			sort.Strings(zones)
			return api.EdgeHostnames{
				Zones:       zones,
				RecordCount: len(records),
				Records:     records,
			}, nil
		})

	writeJSON(w, http.StatusOK, map[string]any{
		"tunnel":    sectionJSON(tunnel),
		"hostnames": sectionJSON(hostnames),
	})
}

func (s *Server) GetAuth(w http.ResponseWriter, r *http.Request, project api.Project) {
	ctx := r.Context()
	name := string(project)
	if !s.projectExists(ctx, w, name) {
		return
	}
	if s.Zitadel == nil || !s.Zitadel.Configured() {
		notImplemented(w, source.Zitadel)
		return
	}
	summary, err := s.Zitadel.Summary(ctx, name)
	if err != nil {
		// A tenant with no organization is a configuration gap, not an
		// outage. Answering 503 would send someone hunting a healthy Zitadel.
		if source.IsNoOrg(err) {
			problem(w, http.StatusNotFound, "No identity configured for this project",
				source.Zitadel, source.SafeError(err))
			return
		}
		s.sourceUnavailable(w, source.Zitadel, err)
		return
	}
	writeJSON(w, http.StatusOK, summary)
}
func (s *Server) GetDatabase(w http.ResponseWriter, r *http.Request, project api.Project) {
	ctx, name, ok := s.databaseScope(w, r, project)
	if !ok {
		return
	}
	summary, err := s.Postgres.Summary(ctx, name)
	if err != nil {
		s.sourceUnavailable(w, source.Postgres, err)
		return
	}
	writeJSON(w, http.StatusOK, summary)
}

func (s *Server) GetDatabaseActivity(w http.ResponseWriter, r *http.Request, project api.Project) {
	ctx, name, ok := s.databaseScope(w, r, project)
	if !ok {
		return
	}
	activity, err := s.Postgres.Activity(ctx, name)
	if err != nil {
		s.sourceUnavailable(w, source.Postgres, err)
		return
	}
	writeJSON(w, http.StatusOK, activity)
}

func (s *Server) ListTables(w http.ResponseWriter, r *http.Request, project api.Project) {
	ctx, name, ok := s.databaseScope(w, r, project)
	if !ok {
		return
	}
	tables, err := s.Postgres.Tables(ctx, name)
	if err != nil {
		s.sourceUnavailable(w, source.Postgres, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"schema": name, "tables": tables, "fetchedAt": time.Now().UTC(),
	})
}

// ListRows browses table contents. The schema comes from the validated project
// path parameter, never from the caller. Every call is logged.
func (s *Server) ListRows(w http.ResponseWriter, r *http.Request, project api.Project, table string, params api.ListRowsParams) {
	ctx, name, ok := s.databaseScope(w, r, project)
	if !ok {
		return
	}

	limit := 50
	if params.Limit != nil {
		limit = *params.Limit
	}
	after := ""
	if params.After != nil {
		after = *params.After
	}

	page, err := s.Postgres.Rows(ctx, name, table, limit, after)
	if err != nil {
		if source.IsNoSingleColumnPK(err) {
			problem(w, http.StatusConflict, "Table cannot be paged", source.Postgres,
				"the table has no single-column primary key, so there is no stable ordering to page by")
			return
		}
		s.sourceUnavailable(w, source.Postgres, err)
		return
	}

	// Every read reaches PostgreSQL as the shared platform_reader role, so
	// this line is the only place a row read is attributable to a person.
	if s.Log != nil {
		caller, email := "anonymous", ""
		if p, ok := auth.FromContext(ctx); ok {
			caller, email = p.Subject, p.Email
		}
		s.Log.Info("row read",
			"sub", caller,
			"email", email,
			"project", name,
			"table", table,
			"rows", len(page.Rows),
			"masked", len(page.MaskedColumns),
			"truncated", page.Truncated)
	}
	writeJSON(w, http.StatusOK, page)
}

// databaseScope resolves and guards a database request, writing the response
// itself when the project is unknown or the source is unconfigured.
func (s *Server) databaseScope(w http.ResponseWriter, r *http.Request, project api.Project) (context.Context, string, bool) {
	ctx := r.Context()
	name := string(project)
	if !s.projectExists(ctx, w, name) {
		return nil, "", false
	}
	if s.Postgres == nil || !s.Postgres.Configured() {
		notImplemented(w, source.Postgres)
		return nil, "", false
	}
	return ctx, name, true
}
func (s *Server) GetStorage(w http.ResponseWriter, r *http.Request, project api.Project) {
	ctx := r.Context()
	name := string(project)
	if !s.projectExists(ctx, w, name) {
		return
	}
	if s.ObjectStore == nil || !s.ObjectStore.Configured() {
		notImplemented(w, source.ObjectStore)
		return
	}

	bucket, err := s.bucketFor(ctx, name)
	if err != nil {
		s.sourceUnavailable(w, source.ObjectStore, err)
		return
	}
	summary, err := s.ObjectStore.Bucket(ctx, bucket)
	if err != nil {
		s.sourceUnavailable(w, source.ObjectStore, err)
		return
	}
	writeJSON(w, http.StatusOK, summary)
}

// bucketFor resolves a project's bucket from the storage broker's policy — the
// same mapping the broker enforces when signing. Never derived from the project
// name: real buckets are "<app>-files", so a guess would 404 or, worse, report
// another tenant's usage.
func (s *Server) bucketFor(ctx context.Context, project string) (string, error) {
	if s.BucketFor != nil {
		return s.BucketFor(ctx, project)
	}
	if s.K8s == nil {
		return "", fmt.Errorf("cannot resolve a bucket without the broker policy")
	}
	return s.K8s.BucketFor(ctx, project)
}
func (s *Server) GetStoragePolicy(w http.ResponseWriter, r *http.Request, project api.Project) {
	ctx := r.Context()
	name := string(project)
	if !s.projectExists(ctx, w, name) {
		return
	}
	policy, err := s.K8s.StoragePolicy(ctx, name)
	if err != nil {
		s.sourceUnavailable(w, source.Kubernetes, err)
		return
	}
	writeJSON(w, http.StatusOK, policy)
}

// enrichFromGitHub resolves each revision's commit to its workflow run for
// actor and duration, flipping Source to helm+github. The repository comes
// from the running image, so no per-tenant configuration is involved. Failures
// are silent per row: GitHub down degrades the timeline, never fails it.
func (s *Server) enrichFromGitHub(ctx context.Context, namespace string, releases []api.Release) {
	if s.GitHub == nil || !s.GitHub.Configured() || len(releases) == 0 {
		return
	}

	owner, repo, ok := s.repoForNamespace(ctx, namespace)
	if !ok {
		return
	}

	for i := range releases {
		if releases[i].Commit == nil {
			continue
		}
		sha := *releases[i].Commit

		run := source.Fetch(ctx, s.Cache, source.GitHub,
			source.Key(source.GitHub, owner, repo, sha), s.TTL.GitHub,
			func(ctx context.Context) (*source.Run, error) {
				return s.GitHub.RunForCommit(ctx, owner, repo, sha)
			})
		if run.Data == nil || *run.Data == nil {
			continue
		}

		got := *run.Data
		if got.Actor != "" {
			releases[i].Actor = ptr(got.Actor)
		}
		if d := got.Duration(); d > 0 {
			releases[i].DurationSeconds = ptr(int(d.Seconds()))
		}
		releases[i].Source = api.ReleaseSource("helm+github")
	}
}

// repoForNamespace derives the GitHub repository from a running workload's
// image, following the platform's ghcr.io/{github_org}/{app_name} convention.
func (s *Server) repoForNamespace(ctx context.Context, namespace string) (owner, repo string, ok bool) {
	workloads, err := s.K8s.Workloads(ctx, namespace)
	if err != nil {
		return "", "", false
	}
	for _, wl := range workloads {
		if wl.Image == nil {
			continue
		}
		if ref, ok := source.ParseImageRef(*wl.Image); ok {
			return ref.Owner, ref.Repo, true
		}
	}
	return "", "", false
}

func ptr[T any](v T) *T { return &v }

// ---------------------------------------------------------------- helpers

// projectExists writes a 404 and reports false when the name is not a tenant
// namespace, so an unknown project is distinguishable from an empty one.
// workloadNamespaceExists is projectExists plus the infra namespace, for the
// one operation where a platform-owned workload is still a workload.
func (s *Server) workloadNamespaceExists(ctx context.Context, w http.ResponseWriter, name string) bool {
	return s.namespaceGate(ctx, w, name, s.K8s.WorkloadNamespaceExists)
}

func (s *Server) projectExists(ctx context.Context, w http.ResponseWriter, name string) bool {
	return s.namespaceGate(ctx, w, name, s.K8s.NamespaceExists)
}

func (s *Server) namespaceGate(ctx context.Context, w http.ResponseWriter, name string,
	lookup func(context.Context, string) (bool, error)) bool {
	ok, err := lookup(ctx, name)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			problem(w, http.StatusNotFound, "No such project", source.Kubernetes, "")
			return false
		}
		s.sourceUnavailable(w, source.Kubernetes, err)
		return false
	}
	if !ok {
		problem(w, http.StatusNotFound, "No such project", source.Kubernetes, "")
		return false
	}
	return true
}

// sectionJSON renders a Section in the wire shape. Data is omitted entirely
// when the section carries none — that absence is the contract.
func sectionJSON[T any](s source.Section[T]) map[string]any {
	out := map[string]any{
		"status": string(s.Status),
		"source": string(s.Source),
	}
	if s.Data != nil {
		out["data"] = *s.Data
	}
	if s.Err != "" {
		out["error"] = s.Err
	}
	if !s.FetchedAt.IsZero() {
		out["fetchedAt"] = s.FetchedAt.UTC()
	}
	if s.Stale {
		out["stale"] = true
	}
	return out
}

func (s *Server) sourceUnavailable(w http.ResponseWriter, src source.Name, err error) {
	if s.Log != nil {
		s.Log.Warn("source unavailable", "source", string(src), "err", source.SafeError(err))
	}
	problem(w, http.StatusServiceUnavailable, "Source unavailable", src, source.SafeError(err))
}

func notImplemented(w http.ResponseWriter, src source.Name) {
	problem(w, http.StatusNotImplemented, "Not implemented yet", src,
		"this operation is declared in the contract but not served by this build")
}

// problem writes an RFC 9457 body. The detail is always run through
// source.SafeError upstream, so no credential reaches a response.
func problem(w http.ResponseWriter, status int, title string, src source.Name, detail string) {
	body := map[string]any{
		"type":   "about:blank",
		"title":  title,
		"status": status,
	}
	if src != "" {
		body["source"] = string(src)
	}
	if detail != "" {
		body["detail"] = detail
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// ConfiguredSources reports which systems this server can reach, derived from
// the wired clients rather than configuration — the two can disagree when a
// client is built and then never passed in. Startup logs it, so that plumbing
// mistake is visible immediately.
func (s *Server) ConfiguredSources() map[string]bool {
	return map[string]bool{
		"kubernetes":    s.K8s != nil,
		"postgres":      s.Postgres != nil && s.Postgres.Configured(),
		"zitadel":       s.Zitadel != nil && s.Zitadel.Configured(),
		"objectStorage": s.ObjectStore != nil && s.ObjectStore.Configured(),
		"cloudflare":    s.Cloudflare != nil && s.Cloudflare.Configured(),
		"github":        s.GitHub != nil && s.GitHub.Configured(),
		"telemetry":     s.Telemetry != nil && s.Telemetry.Configured(),
	}
}
