package source

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/idle-builders/idle-infra/infra-console/internal/api"
)

// CloudflareClient reads DNS and tunnel state.
//
// Cloudflare rate-limits aggressively, and this platform has 60+ records across
// four zones. Callers must go through the per-source cache with a long TTL
// rather than querying per pageview.
type CloudflareClient struct {
	token     string
	accountID string
	base      string
	hc        *http.Client
}

func NewCloudflareClient(token, accountID string) *CloudflareClient {
	return &CloudflareClient{
		token:     token,
		accountID: accountID,
		base:      "https://api.cloudflare.com/client/v4",
		hc:        &http.Client{Timeout: 15 * time.Second},
	}
}

func (c *CloudflareClient) Configured() bool { return c != nil && c.token != "" }

// SetBaseURL overrides the API endpoint.
//
// Every other client in this package takes its base URL from configuration;
// Cloudflare's is fixed because there is only one. This exists so the failure
// path can be exercised against a closed port — the degraded edge panel is a
// contract worth testing, and the alternative is a stub of Cloudflare, which
// would prove only that the client can call a stub.
func (c *CloudflareClient) SetBaseURL(u string) {
	if u != "" {
		c.base = strings.TrimRight(u, "/")
	}
}

type cfEnvelope struct {
	Success bool            `json:"success"`
	Errors  []cfError       `json:"errors"`
	Result  json.RawMessage `json:"result"`
	Info    map[string]any  `json:"result_info"`
}

type cfError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (c *CloudflareClient) get(ctx context.Context, path string, out any) error {
	if !c.Configured() {
		return fmt.Errorf("cloudflare token not configured")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")

	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("cloudflare request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusTooManyRequests {
		return fmt.Errorf("cloudflare rate limited")
	}

	var env cfEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return fmt.Errorf("decode cloudflare response: %w", err)
	}
	if !env.Success {
		if len(env.Errors) > 0 {
			return fmt.Errorf("%s (code %d)", env.Errors[0].Message, env.Errors[0].Code)
		}
		return fmt.Errorf("cloudflare returned status %d", resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(env.Result, out)
}

type cfZone struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Zones lists the zones the token can see.
func (c *CloudflareClient) Zones(ctx context.Context) ([]cfZone, error) {
	var zones []cfZone
	if err := c.get(ctx, "/zones?per_page=50", &zones); err != nil {
		return nil, err
	}
	sort.Slice(zones, func(i, j int) bool { return zones[i].Name < zones[j].Name })
	return zones, nil
}

type cfRecord struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	Content string `json:"content"`
	Proxied bool   `json:"proxied"`
	Comment string `json:"comment"`
}

// Records returns every DNS record in every visible zone, of every type.
//
// Hostnames() below keeps only the record types that can front a service. That
// is right for a per-tenant hostname list and wrong for the edge, where MX and
// TXT are most of the estate — counting only A/AAAA/CNAME reported a "DNS
// records" total that was quietly missing all the mail and verification
// plumbing.
//
// The zone name comes from the zone listing rather than from splitting the
// record name, which guesses wrong on any zone that is not exactly two labels.
func (c *CloudflareClient) Records(ctx context.Context) ([]api.DnsRecord, error) {
	zones, err := c.Zones(ctx)
	if err != nil {
		return nil, err
	}

	out := []api.DnsRecord{}
	for _, zone := range zones {
		var records []cfRecord
		path := fmt.Sprintf("/zones/%s/dns_records?per_page=200", url.PathEscape(zone.ID))
		if err := c.get(ctx, path, &records); err != nil {
			// One unreadable zone must not blank the others.
			continue
		}
		for _, rec := range records {
			r := api.DnsRecord{
				Zone:    zone.Name,
				Name:    rec.Name,
				Type:    rec.Type,
				Proxied: rec.Proxied,
			}
			if rec.Content != "" {
				content := rec.Content
				r.Content = &content
			}
			if rec.Comment != "" {
				comment := rec.Comment
				r.Comment = &comment
			}
			out = append(out, r)
		}
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Zone != out[j].Zone {
			return out[i].Zone < out[j].Zone
		}
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Type < out[j].Type
	})
	return out, nil
}

// Hostnames returns every DNS record across every visible zone, shaped for the
// contract.
//
// TLS is reported as cloudflare-universal for proxied records: those terminate
// at the edge and have no cert-manager Certificate, so rendering a Kubernetes
// expiry for them would be fiction. A cert-manager hostname is identified by
// the Ingress reader instead, and merged by the handler.
func (c *CloudflareClient) Hostnames(ctx context.Context) ([]api.Hostname, error) {
	zones, err := c.Zones(ctx)
	if err != nil {
		return nil, err
	}

	out := []api.Hostname{}
	for _, zone := range zones {
		var records []cfRecord
		path := fmt.Sprintf("/zones/%s/dns_records?per_page=200", url.PathEscape(zone.ID))
		if err := c.get(ctx, path, &records); err != nil {
			// One unreadable zone must not blank the others.
			continue
		}
		for _, rec := range records {
			// Only records that can front a service are hostnames a reader
			// cares about; MX/TXT are mail and verification plumbing.
			if rec.Type != "A" && rec.Type != "AAAA" && rec.Type != "CNAME" {
				continue
			}
			h := api.Hostname{
				Host:       rec.Name,
				RecordType: rec.Type,
				TlsSource:  api.HostnameTlsSource("none"),
			}
			if rec.Proxied {
				h.TlsSource = api.HostnameTlsSource("cloudflare-universal")
			}
			out = append(out, h)
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	return out, nil
}

type cfTunnel struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Conns  []struct {
		ID string `json:"id"`
	} `json:"connections"`
}

// Tunnel reports the Cloudflare Tunnel's health.
//
// The tunnel is the only origin path into this cluster, so its health is the
// difference between "the site is down" and "one app is down".
func (c *CloudflareClient) Tunnel(ctx context.Context) (api.TunnelStatus, error) {
	if c.accountID == "" {
		return api.TunnelStatus{}, fmt.Errorf("cloudflare account id not configured")
	}

	var tunnels []cfTunnel
	path := fmt.Sprintf("/accounts/%s/cfd_tunnel?is_deleted=false", url.PathEscape(c.accountID))
	if err := c.get(ctx, path, &tunnels); err != nil {
		return api.TunnelStatus{}, err
	}
	if len(tunnels) == 0 {
		return api.TunnelStatus{}, fmt.Errorf("no tunnels found for this account")
	}

	t := tunnels[0]
	status := api.TunnelStatus{
		// Cloudflare reports "healthy", "degraded", "down" or "inactive".
		Healthy:    strings.EqualFold(t.Status, "healthy"),
		Connectors: len(t.Conns),
	}
	if t.Name != "" {
		status.Name = &t.Name
	}
	return status, nil
}
