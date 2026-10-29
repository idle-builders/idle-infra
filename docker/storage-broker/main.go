package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

const metricsListenAddr = ":9090"

type presignRequest struct {
	Op          string `json:"op"`
	Key         string `json:"key"`
	ContentType string `json:"contentType,omitempty"`
}

type presignResponse struct {
	URL       string `json:"url"`
	Method    string `json:"method"`
	ExpiresAt string `json:"expiresAt"`
}

type server struct {
	cfg            *Config
	policy         *Policy
	appAuth        AppReviewer
	userAuth       *UserVerifier
	presigner      *Presigner
	objectCleaner  governedObjectCleaner
	governedPolicy *GovernedPolicy
	governedStore  GovernedStore
	governedDB     *sql.DB
	lakeBackend    lakeObjectBackend
	lakeSlots      chan struct{}
	log            *slog.Logger
	now            func() time.Time
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := LoadConfig()
	if err != nil {
		log.Error("load config", "err", err)
		os.Exit(1)
	}
	policy, err := LoadPolicy(cfg.PolicyPath)
	if err != nil {
		log.Error("load policy", "err", err)
		os.Exit(1)
	}
	governedPolicy, err := LoadGovernedPolicy(cfg.GovernedPolicyPath)
	if err != nil {
		log.Error("load governed policy", "err", err)
		os.Exit(1)
	}
	appAuth, err := NewAppAuthenticator(cfg)
	if err != nil {
		log.Error("init app authenticator", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	db, err := sql.Open("pgx", cfg.DatabaseURL)
	if err != nil {
		log.Error("open governed database", "err", err)
		os.Exit(1)
	}
	// Keep every open connection reusable. With fewer idle slots than open
	// connections, each burst closed the surplus and the next request dialed a
	// fresh backend (fork, TLS and SCRAM authentication on the shared server).
	db.SetMaxOpenConns(16)
	db.SetMaxIdleConns(16)
	db.SetConnMaxIdleTime(2 * time.Minute)
	db.SetConnMaxLifetime(30 * time.Minute)
	databaseCtx, cancelDatabase := context.WithTimeout(ctx, 10*time.Second)
	defer cancelDatabase()
	if err := db.PingContext(databaseCtx); err != nil {
		log.Error("connect governed database", "err", err)
		_ = db.Close()
		os.Exit(1)
	}
	governedStore, err := NewPostgresGovernedStore(db, cfg.GovernedSchema)
	if err != nil {
		log.Error("configure governed store", "err", err)
		_ = db.Close()
		os.Exit(1)
	}
	if err := governedStore.Migrate(databaseCtx); err != nil {
		log.Error("migrate governed store", "err", err)
		_ = db.Close()
		os.Exit(1)
	}
	namespaces := make([]string, 0, len(policy.Namespaces))
	for namespace := range policy.Namespaces {
		namespaces = append(namespaces, namespace)
	}
	sort.Strings(namespaces)
	for _, namespace := range namespaces {
		bucket := policy.Namespaces[namespace].Bucket
		if bucket == "" {
			continue
		}
		if err := governedStore.ClaimBucketOwnership(databaseCtx, namespace, bucket); err != nil {
			log.Error("claim configured bucket ownership", "namespace", namespace, "bucket", bucket, "err", err)
			_ = db.Close()
			os.Exit(1)
		}
	}

	s := &server{
		cfg:            cfg,
		policy:         policy,
		appAuth:        newCachedAppReviewer(appAuth),
		userAuth:       NewUserVerifier(ctx, cfg),
		presigner:      NewPresigner(cfg),
		governedPolicy: governedPolicy,
		governedStore:  governedStore,
		governedDB:     db,
		lakeBackend:    nil,
		lakeSlots:      make(chan struct{}, cfg.LakeMaxConcurrency),
		log:            log,
	}
	s.objectCleaner = s.presigner
	s.lakeBackend = s.presigner

	metrics := newBrokerMetrics()
	mux := http.NewServeMux()
	mux.Handle("POST /v1/presign", metrics.instrument(operationPresign, http.HandlerFunc(s.handlePresign)))
	mux.Handle("POST /v1/multipart/create", metrics.instrument(operationMultipartCreate, http.HandlerFunc(s.handleMultipartCreate)))
	mux.Handle("POST /v1/multipart/part", metrics.instrument(operationMultipartPart, http.HandlerFunc(s.handleMultipartPart)))
	mux.Handle("POST /v1/multipart/complete", metrics.instrument(operationMultipartComplete, http.HandlerFunc(s.handleMultipartComplete)))
	mux.Handle("POST /v1/multipart/abort", metrics.instrument(operationMultipartAbort, http.HandlerFunc(s.handleMultipartAbort)))
	registerGovernedRoutes(mux, metrics, s)
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		// Governed commits stream the uploaded object once to verify its declared
		// size and SHA-256 before activating the authorization row.
		WriteTimeout: 5 * time.Minute,
		IdleTimeout:  60 * time.Second,
	}
	metricsMux := http.NewServeMux()
	metricsMux.HandleFunc("GET /metrics", metrics.serveHTTP)
	lakeMux := http.NewServeMux()
	lakeMux.Handle("/", instrumentLakeRequest(metrics, http.HandlerFunc(s.handleLakeS3)))
	lakeSrv := &http.Server{
		Addr:              cfg.LakeListenAddr,
		Handler:           lakeMux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       0,
		WriteTimeout:      0,
		IdleTimeout:       60 * time.Second,
	}
	metricsSrv := &http.Server{
		Addr:              metricsListenAddr,
		Handler:           metricsMux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		log.Info("storage-broker lake endpoint listening", "addr", cfg.LakeListenAddr)
		if err := lakeSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("serve lake endpoint", "err", err)
			os.Exit(1)
		}
	}()
	go func() {
		log.Info("storage-broker listening", "addr", cfg.ListenAddr, "issuer", cfg.OIDCIssuer)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("serve", "err", err)
			os.Exit(1)
		}
	}()
	go func() {
		log.Info("storage-broker metrics listening", "addr", metricsListenAddr)
		if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("serve metrics", "err", err)
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = metricsSrv.Shutdown(shutdownCtx)
	_ = lakeSrv.Shutdown(shutdownCtx)
	_ = srv.Shutdown(shutdownCtx)
	_ = db.Close()
}

func (s *server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (s *server) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	if !s.userAuth.Ready() {
		http.Error(w, "jwks not loaded", http.StatusServiceUnavailable)
		return
	}
	if s.governedDB != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := s.governedDB.PingContext(ctx); err != nil {
			http.Error(w, "governed database unavailable", http.StatusServiceUnavailable)
			return
		}
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ready"))
}

func (s *server) handlePresign(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// 1. App identity (Kubernetes) -> namespace -> bucket.
	appTok := bearer(r.Header.Get("Authorization"))
	if appTok == "" {
		writeJSON(w, http.StatusUnauthorized, errorBody("missing app token"))
		return
	}
	namespace, sa, err := s.appAuth.Review(ctx, appTok)
	if err != nil {
		s.log.Warn("app auth failed", "err", err.Error())
		writeJSON(w, http.StatusUnauthorized, errorBody("invalid app token"))
		return
	}

	// 2. Request.
	var req presignRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, s.cfg.MaxBodyBytes))
	dec.DisallowUnknownFields() // reject e.g. a "bucket" field — the bucket is never caller-chosen
	if err := dec.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody("invalid request body"))
		return
	}
	op := strings.ToLower(strings.TrimSpace(req.Op))

	// 3. User identity (Zitadel), optional depending on policy.
	var sub string
	if userTok := bearer(r.Header.Get("X-User-Authorization")); userTok != "" {
		sub, err = s.userAuth.Verify(ctx, userTok, s.policy.UserTokenClients(namespace))
		if err != nil {
			s.log.Warn("user auth failed", "namespace", namespace, "err", err.Error())
			writeJSON(w, http.StatusUnauthorized, errorBody("invalid user token"))
			return
		}
	}

	// 4. Authorize: bucket from verified identity, key within an allowed prefix.
	decision, err := s.policy.Authorize(namespace, op, req.Key, sub)
	if err != nil {
		reason := "denied"
		var de *DenyError
		if errors.As(err, &de) {
			reason = de.Reason
		}
		s.log.Info("grant denied",
			"namespace", namespace, "sa", sa, "sub", sub, "op", op, "key", req.Key, "reason", reason)
		writeJSON(w, http.StatusForbidden, errorBody(reason))
		return
	}
	if err := s.governedStore.ClaimBucketOwnership(ctx, namespace, decision.Bucket); err != nil {
		s.log.Error("bucket ownership rejected", "namespace", namespace, "bucket", decision.Bucket, "err", err)
		if errors.Is(err, ErrGovernedConflict) {
			writeJSON(w, http.StatusConflict, errorBody("bucket ownership conflict"))
		} else {
			writeJSON(w, http.StatusInternalServerError, errorBody("storage authorization unavailable"))
		}
		return
	}

	// 5. Sign.
	res, err := s.presigner.Presign(ctx, op, decision.Bucket, req.Key, req.ContentType,
		time.Duration(decision.TTLSeconds)*time.Second)
	if err != nil {
		s.log.Error("presign failed", "namespace", namespace, "op", op, "err", err.Error())
		writeJSON(w, http.StatusInternalServerError, errorBody("presign failed"))
		return
	}

	// Audit log — identity, object, op, decision. Never the signed URL (it carries
	// the signature) or any token.
	s.log.Info("grant allowed",
		"namespace", namespace, "sa", sa, "sub", sub,
		"bucket", decision.Bucket, "op", op, "key", req.Key,
		"prefix", decision.Prefix, "ttl", decision.TTLSeconds)

	writeJSON(w, http.StatusOK, presignResponse{
		URL:       res.URL,
		Method:    res.Method,
		ExpiresAt: res.ExpiresAt.UTC().Format(time.RFC3339),
	})
}

func bearer(h string) string {
	const prefix = "Bearer "
	if len(h) >= len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
		return strings.TrimSpace(h[len(prefix):])
	}
	return ""
}

func errorBody(msg string) map[string]string { return map[string]string{"error": msg} }

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
