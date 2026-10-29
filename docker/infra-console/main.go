// Command infra-console serves the read-only console and MCP surface over the
// systems that own platform state.
//
// Contract: docs/api/console-v1.yaml. Design: docs/plans/infra-console.md.
// The service is stateless and writes nothing anywhere; audit goes to stdout.
package main

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver for PostgreSQL

	"github.com/idle-builders/idle-infra/infra-console/internal/api"
	"github.com/idle-builders/idle-infra/infra-console/internal/auth"
	"github.com/idle-builders/idle-infra/infra-console/internal/httpapi"
	"github.com/idle-builders/idle-infra/infra-console/internal/mcp"
	"github.com/idle-builders/idle-infra/infra-console/internal/source"
	"github.com/idle-builders/idle-infra/infra-console/web"
)

// version is stamped into the MCP server identity. Overridden at build time
// with -ldflags "-X main.version=...".
var version = "dev"

type server struct {
	cfg   *Config
	cache *source.Cache
	log   *slog.Logger
}

func main() {
	cfg, err := LoadConfig()
	if err != nil {
		// The logger needs the config for its level, so bootstrap a plain one
		// to report a config failure.
		slog.New(slog.NewJSONHandler(os.Stdout, nil)).Error("load config", "err", err.Error())
		os.Exit(1)
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel(cfg.LogLevel)}))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	s := &server{cfg: cfg, cache: source.NewCache(), log: log}

	// A cluster client is required: without it there is no console. Every
	// other source is optional and degrades to an unavailable Section.
	k8s, err := source.NewK8s()
	if err != nil {
		log.Error("kubernetes client", "err", err.Error())
		os.Exit(1)
	}

	// PostgreSQL is optional: the platform_reader grants are a separate
	// Terraform change, and the console must run usefully before they land.
	var pg *source.PostgresClient
	if dsn := cfg.PostgresDSN(); dsn != "" {
		db, err := sql.Open("pgx", dsn)
		if err != nil {
			log.Error("postgres pool", "err", err.Error())
			os.Exit(1)
		}
		db.SetMaxOpenConns(4)
		db.SetMaxIdleConns(2)
		db.SetConnMaxLifetime(30 * time.Minute)
		defer func() { _ = db.Close() }()
		pg = source.NewPostgresClient(db, cfg.MaxRowLimit, cfg.QueryTimeout)
	}

	handlers := &httpapi.Server{
		K8s:        k8s,
		GitHub:     source.NewGitHubClient(cfg.GitHubToken),
		Cloudflare: source.NewCloudflareClient(cfg.CloudflareToken, cfg.CloudflareAccountID),
		ObjectStore: source.NewObjectStoreClient(
			cfg.SwiftAuthURL, cfg.SwiftEndpoint, cfg.S3Endpoint,
			cfg.SwiftApplicationCredentialID, cfg.SwiftApplicationSecret,
		),
		Telemetry: source.NewTelemetryClient(
			cfg.TelemetryBinary, cfg.TelemetryBucket,
			cfg.S3Endpoint, cfg.S3Region, cfg.S3AccessKey, cfg.S3SecretKey,
			cfg.TelemetryTimeout,
		),
		Zitadel:  source.NewZitadelClient(cfg.ZitadelURL, cfg.ZitadelToken),
		Postgres: pg,
		Cache:    s.cache,
		Log:      log,
		TTL: httpapi.TTLs{
			Kubernetes:  cfg.TTLKubernetes,
			Postgres:    cfg.TTLPostgres,
			Zitadel:     cfg.TTLZitadel,
			ObjectStore: cfg.TTLObjectStore,
			Cloudflare:  cfg.TTLCloudflare,
			GitHub:      cfg.TTLGitHub,
		},
	}

	mux := http.NewServeMux()
	// Liveness and readiness stay off the versioned path so probes do not
	// depend on the API surface.
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)
	apiHandler := api.HandlerFromMuxWithBaseURL(handlers, http.NewServeMux(), "/v1")

	// Authentication wraps the API, not the health probes: a probe must not
	// depend on Zitadel being reachable.
	verifier := auth.NewVerifier(cfg.OIDCIssuer, log)
	if !verifier.Configured() {
		// Refuse to start rather than serve every tenant's database to
		// anyone who can reach the port. A warning is not enough: the most
		// likely reason an operator unsets OIDC_ISSUER is that authentication
		// is misbehaving, which is exactly when silently disabling it is worst.
		if !cfg.AuthDisabled {
			log.Error("OIDC_ISSUER is not set: refusing to start without authentication",
				"remedy", "set OIDC_ISSUER, or AUTH_DISABLED=1 to run locally without it")
			os.Exit(1)
		}
		log.Warn("authentication is DISABLED by AUTH_DISABLED — local development only")
	}
	protected := auth.Middleware(verifier, log)(apiHandler)
	mux.Handle("/v1/", protected)

	// Sign-in sits outside the authenticated API: requiring a session to obtain
	// a session would be a loop.
	login := auth.NewLogin(cfg.OIDCIssuer, cfg.OIDCClientID, cfg.ConsoleURL, log)
	login.Routes(mux)
	if verifier.Configured() && !login.Configured() {
		log.Warn("browser sign-in is unavailable — no OIDC_CLIENT_ID configured; bearer-token callers still work")
	}

	// MCP tools dispatch through the same protected handler as the browser, so
	// an agent is admitted on exactly the same terms. The adapter copies the
	// caller's own Authorization header onto the request it synthesizes, so
	// every tool call is authenticated as the caller who sent THAT call —
	// never as whoever opened the MCP session.
	mcpHandler := auth.Middleware(verifier, log)(mcp.Handler(protected, "/v1", version))
	mux.Handle("/mcp", mcpHandler)
	mux.Handle("/mcp/", mcpHandler)

	// The UI is served last, as the catch-all. It is a static shell; every
	// number it shows comes from the authenticated API above.
	// Only redirect to sign-in when there is a sign-in to redirect to.
	signIn := ""
	if verifier.Configured() && login.Configured() {
		signIn = "/auth/login"
	}
	mux.Handle("/", web.Handler(signIn))

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           s.withRequestLog(mux),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		// Generous: an aggregate fans out across several upstreams, and log
		// streaming holds the response open.
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  90 * time.Second,
	}

	go s.sweepCache(ctx)

	go func() {
		// Report what the HANDLER actually holds, never what configuration
		// intended. A source that is constructed but not wired in must show up
		// here as absent, or a plumbing mistake hides until someone opens the
		// panel and gets a 501.
		log.Info("infra-console listening",
			"addr", cfg.ListenAddr,
			"version", version,
			"mcpTools", len(mcp.Tools),
			"sources", handlers.ConfiguredSources(),
			"signIn", login.Configured(),
			"maxRowLimit", cfg.MaxRowLimit)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("serve", "err", err.Error())
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("shutdown", "err", err.Error())
	}
}

// sweepCache bounds cache memory for keys that stop being requested. The max
// age is far above any per-source TTL so the degraded path keeps its stale
// entries.
func (s *server) sweepCache(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if n := s.cache.Sweep(2 * time.Hour); n > 0 {
				s.log.Debug("cache swept", "removed", n, "remaining", s.cache.Len())
			}
		}
	}
}

// Liveness and readiness, mounted at the root: outside /v1 and outside auth,
// because a kubelet presents no credential and a probe that 401s takes the pod
// down. Readiness is the console's own ability to serve — a degraded upstream
// is an unavailable panel, never an unready pod.
func (s *server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (s *server) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ready"))
}

// withRequestLog emits one structured line per request. It never logs query
// strings or headers, both of which can carry credentials.
func (s *server) withRequestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		// Health probes would drown the log.
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			return
		}
		s.log.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"durationMs", time.Since(start).Milliseconds())
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

// Unwrap lets http.ResponseController reach the real writer. Without it
// rc.Flush() silently does nothing — the MCP SDK ignores the error, so a
// streaming response would hang rather than fail. Nothing streams today.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (r *statusRecorder) WriteHeader(code int) {
	if r.wroteHeader {
		return
	}
	r.status = code
	r.wroteHeader = true
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	return r.ResponseWriter.Write(b)
}

func logLevel(name string) slog.Level {
	switch strings.ToLower(name) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
