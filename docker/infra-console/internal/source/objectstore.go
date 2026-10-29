package source

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/idle-builders/idle-infra/infra-console/internal/api"
)

// ObjectStoreClient reads exact bucket usage from Swift container metadata.
// Unlike S3 ListObjects, this is a constant-time provider counter and remains
// useful for buckets containing millions of objects.
type ObjectStoreClient struct {
	authURL        string
	swiftEndpoint  string
	publicEndpoint string
	credentialID   string
	secret         string
	hc             *http.Client
}

func NewObjectStoreClient(authURL, swiftEndpoint, publicEndpoint, credentialID, secret string) *ObjectStoreClient {
	return &ObjectStoreClient{
		authURL:        strings.TrimRight(authURL, "/"),
		swiftEndpoint:  strings.TrimRight(swiftEndpoint, "/"),
		publicEndpoint: strings.TrimRight(publicEndpoint, "/"),
		credentialID:   credentialID,
		secret:         secret,
		hc:             &http.Client{Timeout: 15 * time.Second},
	}
}

func (o *ObjectStoreClient) Configured() bool {
	return o != nil && o.authURL != "" && o.swiftEndpoint != "" &&
		o.credentialID != "" && o.secret != "" && o.hc != nil
}

// Bucket reports the provider's exact object count and byte total.
func (o *ObjectStoreClient) Bucket(ctx context.Context, name string) (api.StorageSummary, error) {
	if !o.Configured() {
		return api.StorageSummary{}, fmt.Errorf("object storage metadata credentials not configured")
	}
	if name == "" {
		return api.StorageSummary{}, fmt.Errorf("bucket name is required")
	}

	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	token, err := o.token(ctx)
	if err != nil {
		return api.StorageSummary{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, o.swiftEndpoint+"/"+name, nil)
	if err != nil {
		return api.StorageSummary{}, fmt.Errorf("build object storage metadata request: %w", err)
	}
	req.Header.Set("X-Auth-Token", token)
	resp, err := o.hc.Do(req)
	if err != nil {
		return api.StorageSummary{}, fmt.Errorf("read bucket metadata: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		return api.StorageSummary{}, fmt.Errorf("object storage metadata returned status %d", resp.StatusCode)
	}
	return storageSummary(name, o.publicEndpoint, resp.Header)
}

func (o *ObjectStoreClient) token(ctx context.Context) (string, error) {
	payload := struct {
		Auth struct {
			Identity struct {
				Methods               []string `json:"methods"`
				ApplicationCredential struct {
					ID     string `json:"id"`
					Secret string `json:"secret"`
				} `json:"application_credential"`
			} `json:"identity"`
		} `json:"auth"`
	}{}
	payload.Auth.Identity.Methods = []string{"application_credential"}
	payload.Auth.Identity.ApplicationCredential.ID = o.credentialID
	payload.Auth.Identity.ApplicationCredential.Secret = o.secret
	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode object storage authentication: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.authURL+"/auth/tokens", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build object storage authentication: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := o.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("authenticate object storage metadata: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("object storage authentication returned status %d", resp.StatusCode)
	}
	token := resp.Header.Get("X-Subject-Token")
	if token == "" {
		return "", fmt.Errorf("object storage authentication returned no token")
	}
	return token, nil
}

func storageSummary(name, endpoint string, headers http.Header) (api.StorageSummary, error) {
	count, err := nonNegativeHeader(headers, "X-Container-Object-Count")
	if err != nil {
		return api.StorageSummary{}, err
	}
	used, err := nonNegativeHeader(headers, "X-Container-Bytes-Used")
	if err != nil {
		return api.StorageSummary{}, err
	}
	return api.StorageSummary{
		Bucket: name, ObjectCount: count, BytesUsed: used, Endpoint: &endpoint,
	}, nil
}

func nonNegativeHeader(headers http.Header, name string) (int64, error) {
	raw := headers.Get(name)
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("object storage metadata returned invalid %s", name)
	}
	return n, nil
}
