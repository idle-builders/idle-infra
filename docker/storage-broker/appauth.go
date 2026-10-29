package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// AppAuthenticator validates an app's projected ServiceAccount token via the
// Kubernetes TokenReview API and returns the namespace it belongs to. The
// namespace IS the app identity used to derive the allowed bucket — the request
// body never gets to name a bucket, so an app cannot reach another app's data.
//
// We call the API directly over HTTPS (rather than pulling in client-go) to keep
// the binary tiny and the dependency surface small for a credential-holding
// component. The broker authenticates the call with its OWN SA token; the token
// being reviewed is the app's, passed in the request body.
type AppAuthenticator struct {
	apiServer  string
	tokenPath  string
	audience   string
	httpClient *http.Client
}

func NewAppAuthenticator(c *Config) (*AppAuthenticator, error) {
	caPEM, err := os.ReadFile(c.KubeCAPath)
	if err != nil {
		return nil, fmt.Errorf("read kube CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("parse kube CA from %s", c.KubeCAPath)
	}
	return &AppAuthenticator{
		apiServer: c.KubeAPIServer,
		tokenPath: c.KubeTokenPath,
		audience:  c.TokenAudience,
		httpClient: &http.Client{
			Timeout: 5 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
			},
		},
	}, nil
}

type tokenReviewRequest struct {
	APIVersion string                 `json:"apiVersion"`
	Kind       string                 `json:"kind"`
	Spec       tokenReviewRequestSpec `json:"spec"`
}

type tokenReviewRequestSpec struct {
	Token     string   `json:"token"`
	Audiences []string `json:"audiences,omitempty"`
}

type tokenReviewResponse struct {
	Status struct {
		Authenticated bool     `json:"authenticated"`
		Audiences     []string `json:"audiences"`
		Error         string   `json:"error"`
		User          struct {
			Username string `json:"username"`
		} `json:"user"`
	} `json:"status"`
}

// Review validates the app token and returns its namespace and service account.
func (a *AppAuthenticator) Review(ctx context.Context, token string) (namespace, serviceAccount string, err error) {
	saToken, err := os.ReadFile(a.tokenPath)
	if err != nil {
		return "", "", fmt.Errorf("read broker SA token: %w", err)
	}

	reqBody, err := json.Marshal(tokenReviewRequest{
		APIVersion: "authentication.k8s.io/v1",
		Kind:       "TokenReview",
		Spec:       tokenReviewRequestSpec{Token: token, Audiences: []string{a.audience}},
	})
	if err != nil {
		return "", "", err
	}

	url := a.apiServer + "/apis/authentication.k8s.io/v1/tokenreviews"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(saToken)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("tokenreview call: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("tokenreview returned HTTP %d", resp.StatusCode)
	}

	var tr tokenReviewResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", "", fmt.Errorf("parse tokenreview response: %w", err)
	}
	if !tr.Status.Authenticated {
		return "", "", fmt.Errorf("app token not authenticated: %s", tr.Status.Error)
	}
	// The API server only confirms the requested audience if the token actually
	// carries it; require it explicitly so a default (kube-apiserver) token can't
	// be replayed against the broker.
	if !contains(tr.Status.Audiences, a.audience) {
		return "", "", fmt.Errorf("app token missing audience %q", a.audience)
	}

	// username form: system:serviceaccount:<namespace>:<name>
	parts := strings.Split(tr.Status.User.Username, ":")
	if len(parts) != 4 || parts[0] != "system" || parts[1] != "serviceaccount" {
		return "", "", fmt.Errorf("token is not a service account token")
	}
	if parts[2] == "" {
		return "", "", fmt.Errorf("service account has empty namespace")
	}
	return parts[2], parts[3], nil
}
