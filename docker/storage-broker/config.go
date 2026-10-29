package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the broker's runtime configuration, sourced entirely from the
// environment (S3 credential + endpoint from the broker's mounted secret, the
// rest from the deployment). The per-tenant authorization policy is loaded
// separately from a mounted file (see LoadPolicy).
type Config struct {
	ListenAddr     string
	LakeListenAddr string

	// S3 (the ONLY place the credential lives — injected from the broker secret).
	S3Endpoint  string
	S3Region    string
	S3AccessKey string
	S3SecretKey string

	// Zitadel / OIDC user-token verification.
	// Accepted audiences are per tenant (NamespacePolicy.UserTokenClients), not global.
	OIDCIssuer string

	// Kubernetes app identity (TokenReview).
	TokenAudience string // audience the app's projected SA token must carry
	KubeAPIServer string
	KubeTokenPath string // broker's own SA token, used to call TokenReview
	KubeCAPath    string

	PolicyPath         string
	GovernedPolicyPath string
	DatabaseURL        string
	GovernedSchema     string
	MaxBodyBytes       int64
	DuckLakeRoot       string
	DuckLakeMaxPut     int64
	LakeOperationTTL   time.Duration
	LakeIOIdleTTL      time.Duration
	LakeMaxConcurrency int
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func getenvInt64(k string, def int64) (int64, error) {
	value := getenv(k, strconv.FormatInt(def, 10))
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", k)
	}
	return parsed, nil
}

func getenvDuration(k string, def time.Duration) (time.Duration, error) {
	value := getenv(k, def.String())
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration", k)
	}
	return parsed, nil
}

// LoadConfig reads configuration from the environment and validates that the
// required secrets/endpoints are present (fail fast at startup).
func LoadConfig() (*Config, error) {
	c := &Config{
		ListenAddr:         getenv("LISTEN_ADDR", ":8080"),
		LakeListenAddr:     getenv("LAKE_LISTEN_ADDR", ":8081"),
		S3Endpoint:         os.Getenv("S3_ENDPOINT"),
		S3Region:           getenv("S3_REGION", "us-east-1"),
		S3AccessKey:        os.Getenv("S3_ACCESS_KEY_ID"),
		S3SecretKey:        os.Getenv("S3_SECRET_ACCESS_KEY"),
		OIDCIssuer:         os.Getenv("OIDC_ISSUER"),
		TokenAudience:      getenv("TOKEN_AUDIENCE", "storage-broker"),
		KubeTokenPath:      getenv("KUBE_TOKEN_PATH", "/var/run/secrets/kubernetes.io/serviceaccount/token"),
		KubeCAPath:         getenv("KUBE_CA_PATH", "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"),
		PolicyPath:         getenv("POLICY_PATH", "/etc/storage-broker/policy.json"),
		GovernedPolicyPath: getenv("GOVERNED_POLICY_PATH", "/etc/storage-broker/governed-policy.json"),
		DatabaseURL:        os.Getenv("DATABASE_URL"),
		GovernedSchema:     getenv("GOVERNED_SCHEMA", "storage_broker"),
		MaxBodyBytes:       16 * 1024,
		DuckLakeRoot:       getenv("DUCKLAKE_ROOT", "ducklake/"),
	}
	var err error
	if c.DuckLakeMaxPut, err = getenvInt64("DUCKLAKE_MAX_PUT_BYTES", 96*1024*1024); err != nil {
		return nil, err
	}
	if c.LakeOperationTTL, err = getenvDuration("LAKE_OPERATION_TIMEOUT", 5*time.Minute); err != nil {
		return nil, err
	}
	if c.LakeIOIdleTTL, err = getenvDuration("LAKE_IO_IDLE_TIMEOUT", 30*time.Second); err != nil {
		return nil, err
	}
	maxConcurrency, err := getenvInt64("LAKE_MAX_CONCURRENCY", 4)
	if err != nil || maxConcurrency > 64 {
		return nil, fmt.Errorf("LAKE_MAX_CONCURRENCY must be between 1 and 64")
	}
	c.LakeMaxConcurrency = int(maxConcurrency)
	if err := validDuckLakeRoot(c.DuckLakeRoot); err != nil {
		return nil, fmt.Errorf("invalid DUCKLAKE_ROOT: %w", err)
	}

	// In-cluster API server address (kubelet injects host/port).
	if host := os.Getenv("KUBERNETES_SERVICE_HOST"); host != "" {
		c.KubeAPIServer = fmt.Sprintf("https://%s:%s", host, getenv("KUBERNETES_SERVICE_PORT", "443"))
	}
	c.KubeAPIServer = getenv("KUBE_API_SERVER", c.KubeAPIServer)

	var missing []string
	for k, v := range map[string]string{
		"S3_ENDPOINT":          c.S3Endpoint,
		"S3_ACCESS_KEY_ID":     c.S3AccessKey,
		"S3_SECRET_ACCESS_KEY": c.S3SecretKey,
		"OIDC_ISSUER":          c.OIDCIssuer,
		"DATABASE_URL":         c.DatabaseURL,
	} {
		if v == "" {
			missing = append(missing, k)
		}
	}
	if c.KubeAPIServer == "" {
		missing = append(missing, "KUBERNETES_SERVICE_HOST")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required configuration: %s", strings.Join(missing, ", "))
	}
	return c, nil
}
