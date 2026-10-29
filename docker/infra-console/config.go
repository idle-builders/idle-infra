package main

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the service's whole configuration surface.
//
// Every upstream base URL is configuration rather than a constant, so a source
// that moves is a variable change rather than a rebuild.
type Config struct {
	ListenAddr string

	// Upstreams. An empty value means "not configured" — the corresponding
	// source reports itself unavailable instead of the process refusing to
	// start. A console that cannot boot because Cloudflare is unreachable is
	// worse than one that shows an unavailable panel.

	// Empty means the timeline renders from Helm alone.
	GitHubToken string

	CloudflareToken     string
	CloudflareAccountID string

	// Telemetry data still speaks S3, like the broker, backups, and state
	// backend. Exact tenant-bucket counters below use Swift metadata because S3
	// has no constant-time bucket statistics operation.
	S3Endpoint  string
	S3Region    string
	S3AccessKey string
	S3SecretKey string
	// Swift exposes exact container counters through a metadata HEAD. The
	// application credential is generated separately from the EC2 credential
	// because Keystone cannot exchange the latter for a Swift token here.
	SwiftAuthURL                 string
	SwiftEndpoint                string
	SwiftApplicationCredentialID string
	SwiftApplicationSecret       string
	// Telemetry queries execute the short-lived Arrow/Parquet helper bundled
	// in the console image. It uses the console's existing project-scoped S3
	// credential; Infomaniak cannot enforce bucket-scoped EC2 credentials.
	TelemetryBinary  string
	TelemetryBucket  string
	TelemetryTimeout time.Duration

	OIDCIssuer string
	// AuthDisabled must be set explicitly to run without an issuer; otherwise
	// a missing OIDC_ISSUER is a startup error, not open access.
	AuthDisabled bool

	// A PKCE public client: a secret in a browser app is not a secret.
	OIDCClientID string
	// Public origin, used to form the OIDC redirect URI registered on the app.
	ConsoleURL string

	// Service-user PAT. Presenting one is a bearer header; only verifying one
	// needs USERINFO, since PATs are opaque JWE.
	ZitadelURL   string
	ZitadelToken string

	// PostgreSQL, as platform_reader.
	PostgresHost     string
	PostgresPort     string
	PostgresDatabase string
	PostgresUser     string
	PostgresPassword string
	// verify-ca with a mounted CA, matching the tenant DATABASE_URL; pg_hba
	// enforces hostssl for every non-loopback connection.
	PostgresSSLMode string
	PostgresSSLRoot string

	// Defaults come from the read-source table in docs/plans/infra-console.md.
	TTLKubernetes  time.Duration
	TTLPostgres    time.Duration
	TTLZitadel     time.Duration
	TTLObjectStore time.Duration
	TTLCloudflare  time.Duration
	TTLGitHub      time.Duration

	// Enforced server-side; the spec's maxima are a courtesy, not the guard.
	MaxRowLimit int
	// A client-side deadline. The server-side guard is statement_timeout on
	// the platform_reader role, set in Terraform, not here.
	QueryTimeout time.Duration

	LogLevel string
}

// LoadConfig reads configuration from the environment, applying defaults.
//
// It fails only on values that are present but unusable. A missing optional
// upstream is not an error.
func LoadConfig() (*Config, error) {
	cfg := &Config{
		ListenAddr:                   env("LISTEN_ADDR", ":8080"),
		GitHubToken:                  env("GITHUB_TOKEN", ""),
		CloudflareToken:              env("CLOUDFLARE_API_TOKEN", ""),
		CloudflareAccountID:          env("CLOUDFLARE_ACCOUNT_ID", ""),
		S3Endpoint:                   strings.TrimRight(env("S3_ENDPOINT", ""), "/"),
		S3Region:                     env("S3_REGION", "us-east-1"),
		S3AccessKey:                  env("S3_ACCESS_KEY_ID", ""),
		S3SecretKey:                  env("S3_SECRET_ACCESS_KEY", ""),
		SwiftAuthURL:                 strings.TrimRight(env("SWIFT_AUTH_URL", ""), "/"),
		SwiftEndpoint:                strings.TrimRight(env("SWIFT_ENDPOINT", ""), "/"),
		SwiftApplicationCredentialID: env("SWIFT_APPLICATION_CREDENTIAL_ID", ""),
		SwiftApplicationSecret:       env("SWIFT_APPLICATION_CREDENTIAL_SECRET", ""),
		TelemetryBinary:              env("TELEMETRY_QUERY_BINARY", "/telemetry-data"),
		TelemetryBucket:              env("TELEMETRY_BUCKET", "infra-observability"),
		OIDCIssuer:                   strings.TrimRight(env("OIDC_ISSUER", ""), "/"),
		AuthDisabled:                 env("AUTH_DISABLED", "") == "1",
		OIDCClientID:                 env("OIDC_CLIENT_ID", ""),
		ConsoleURL:                   strings.TrimRight(env("CONSOLE_URL", ""), "/"),
		ZitadelURL:                   strings.TrimRight(env("ZITADEL_URL", ""), "/"),
		ZitadelToken:                 env("ZITADEL_TOKEN", ""),
		PostgresHost:                 env("POSTGRES_HOST", ""),
		PostgresPort:                 env("POSTGRES_PORT", "5432"),
		PostgresDatabase:             env("POSTGRES_DATABASE", "postgres"),
		PostgresUser:                 env("POSTGRES_USER", ""),
		PostgresPassword:             env("POSTGRES_PASSWORD", ""),
		PostgresSSLMode:              env("POSTGRES_SSLMODE", "verify-ca"),
		PostgresSSLRoot:              env("POSTGRES_SSLROOTCERT", "/etc/ssl/pg/ca.crt"),
		LogLevel:                     env("LOG_LEVEL", "info"),
	}

	var err error
	durations := []struct {
		key   string
		def   time.Duration
		field *time.Duration
	}{
		{"TTL_KUBERNETES", 15 * time.Second, &cfg.TTLKubernetes},
		{"TTL_POSTGRES", 60 * time.Second, &cfg.TTLPostgres},
		{"TTL_ZITADEL", 5 * time.Minute, &cfg.TTLZitadel},
		{"TTL_OBJECT_STORE", 5 * time.Minute, &cfg.TTLObjectStore},
		// Cloudflare and GitHub are rate limited, so they are polled on a
		// schedule rather than per pageview.
		{"TTL_CLOUDFLARE", 15 * time.Minute, &cfg.TTLCloudflare},
		{"TTL_GITHUB", 5 * time.Minute, &cfg.TTLGitHub},
		{"QUERY_TIMEOUT", 5 * time.Second, &cfg.QueryTimeout},
		{"TELEMETRY_QUERY_TIMEOUT", 45 * time.Second, &cfg.TelemetryTimeout},
	}
	for _, d := range durations {
		if *d.field, err = envDuration(d.key, d.def); err != nil {
			return nil, err
		}
	}

	if cfg.MaxRowLimit, err = envInt("MAX_ROW_LIMIT", 100); err != nil {
		return nil, err
	}
	if cfg.MaxRowLimit < 1 || cfg.MaxRowLimit > 1000 {
		return nil, fmt.Errorf("MAX_ROW_LIMIT must be between 1 and 1000, got %d", cfg.MaxRowLimit)
	}

	return cfg, nil
}

// PostgresDSN builds the connection string, or "" when unconfigured.
//
// sslmode=verify-ca with a mounted CA, matching the tenant DATABASE_URL. NOT
// require, which encrypts but verifies nothing and so accepts a man in the
// middle. Overridable only for a local database without TLS; never weaken it
// in a deployment.
func (c *Config) PostgresDSN() string {
	if c.PostgresHost == "" || c.PostgresUser == "" {
		return ""
	}
	dsn := fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=%s",
		url.QueryEscape(c.PostgresUser), url.QueryEscape(c.PostgresPassword),
		c.PostgresHost, c.PostgresPort, c.PostgresDatabase,
		url.QueryEscape(c.PostgresSSLMode))
	if c.PostgresSSLRoot != "" && strings.HasPrefix(c.PostgresSSLMode, "verify") {
		dsn += "&sslrootcert=" + url.QueryEscape(c.PostgresSSLRoot)
	}
	return dsn
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envDuration(key string, def time.Duration) (time.Duration, error) {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return def, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("%s must not be negative, got %s", key, d)
	}
	return d, nil
}

func envInt(key string, def int) (int, error) {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return n, nil
}
