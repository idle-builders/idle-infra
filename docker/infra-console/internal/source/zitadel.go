package source

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/idle-builders/idle-infra/infra-console/internal/api"
)

// ZitadelClient reads identity state.
//
// Zitadel owns this state, so the console summarizes and deep-links rather
// than reimplementing its UI. Auth is a service-user PAT as a bearer header;
// note the asymmetry — verifying a Zitadel token needs USERINFO (PATs are
// opaque JWE), presenting one does not.
type ZitadelClient struct {
	base  string
	token string
	hc    *http.Client
}

func NewZitadelClient(baseURL, token string) *ZitadelClient {
	return &ZitadelClient{
		base:  strings.TrimRight(baseURL, "/"),
		token: token,
		hc:    &http.Client{Timeout: 15 * time.Second},
	}
}

func (z *ZitadelClient) Configured() bool {
	return z != nil && z.base != "" && z.token != ""
}

func zitadelStatusError(status int, body io.Reader) error {
	var problem struct {
		Message string `json:"message"`
	}
	if status == http.StatusNotFound {
		if err := json.NewDecoder(io.LimitReader(body, 4096)).Decode(&problem); err == nil &&
			strings.Contains(strings.ToLower(problem.Message), "membership not found") {
			return fmt.Errorf("identity inventory unavailable")
		}
	}
	return fmt.Errorf("zitadel returned status %d", status)
}

// post issues a Management API search. Zitadel's search endpoints are POSTs
// with a JSON body even though they are reads.
func (z *ZitadelClient) post(ctx context.Context, path, orgID string, body any, out any) error {
	if !z.Configured() {
		return fmt.Errorf("zitadel not configured")
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, z.base+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+z.token)
	req.Header.Set("Content-Type", "application/json")
	if orgID != "" {
		// Management API calls are org-scoped by this header.
		req.Header.Set("x-zitadel-orgid", orgID)
	}

	resp, err := z.hc.Do(req)
	if err != nil {
		return fmt.Errorf("zitadel request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return zitadelStatusError(resp.StatusCode, resp.Body)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

type orgSearchResponse struct {
	Result []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"result"`
}

type appSearchResponse struct {
	Details struct {
		TotalResult string `json:"totalResult"`
	} `json:"details"`
	Result []struct {
		Name       string `json:"name"`
		OIDCConfig *struct {
			ClientID     string   `json:"clientId"`
			RedirectURIs []string `json:"redirectUris"`
			AppType      string   `json:"appType"`
			GrantTypes   []string `json:"grantTypes"`
		} `json:"oidcConfig"`
		APIConfig *struct {
			ClientID string `json:"clientId"`
		} `json:"apiConfig"`
	} `json:"result"`
}

type countResponse struct {
	Details struct {
		TotalResult string `json:"totalResult"`
	} `json:"details"`
}

// authApp aliases the anonymous struct the generator emits for
// AuthSummary.Applications, so building one reads as a value rather than a
// repeated type literal.
type authApp = struct {
	ClientId     *string   `json:"clientId,omitempty"`
	Name         string    `json:"name"`
	RedirectUris *[]string `json:"redirectUris,omitempty"`
	Type         string    `json:"type"`
}

// OrgFor resolves a tenant to its Zitadel organization.
//
// Organizations are matched by name, case-insensitively: the platform's
// convention is one org per tenant, named after it. A tenant with no org is not
// an error — it simply has no identity configured yet.
func (z *ZitadelClient) OrgFor(ctx context.Context, tenant string) (id, name string, err error) {
	var out orgSearchResponse
	body := map[string]any{
		"query": map[string]any{"limit": 100},
	}
	// The ADMIN api, not management. Management is org-scoped by the
	// x-zitadel-orgid header and has no "list every org" call — asking it for
	// /management/v1/orgs/_search returns 404. Verified against Zitadel v4.14.0.
	if err := z.post(ctx, "/admin/v1/orgs/_search", "", body, &out); err != nil {
		return "", "", err
	}

	for _, org := range out.Result {
		if strings.EqualFold(org.Name, tenant) {
			return org.ID, org.Name, nil
		}
	}
	return "", "", nil
}

// NoOrgError means the tenant has no Zitadel organization.
//
// Distinct from an outage: Zitadel answered perfectly, this tenant simply has
// no identity configured. Reporting it as "source unavailable" would send
// someone looking for a broken Zitadel.
type NoOrgError struct{ Tenant string }

func (e *NoOrgError) Error() string {
	return fmt.Sprintf("no zitadel organization named %q", e.Tenant)
}

// IsNoOrg reports whether an error is the tenant-has-no-org case.
func IsNoOrg(err error) bool {
	var target *NoOrgError
	return errors.As(err, &target)
}

// Summary describes a tenant's identity configuration.
func (z *ZitadelClient) Summary(ctx context.Context, tenant string) (api.AuthSummary, error) {
	orgID, orgName, err := z.OrgFor(ctx, tenant)
	if err != nil {
		return api.AuthSummary{}, err
	}
	if orgID == "" {
		return api.AuthSummary{}, &NoOrgError{Tenant: tenant}
	}

	summary := api.AuthSummary{
		Org:   orgName,
		OrgId: &orgID,
		// Deep link rather than a rebuilt console: Zitadel owns this state.
		ConsoleUrl:   z.base + "/ui/console/org?id=" + orgID,
		Applications: []authApp{},
	}

	// Projects hold applications; walk each project's apps.
	var projects struct {
		Result []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"result"`
	}
	if err := z.post(ctx, "/management/v1/projects/_search", orgID,
		map[string]any{"query": map[string]any{"limit": 100}}, &projects); err != nil {
		return summary, err
	}

	for _, project := range projects.Result {
		var apps appSearchResponse
		path := "/management/v1/projects/" + project.ID + "/apps/_search"
		if err := z.post(ctx, path, orgID,
			map[string]any{"query": map[string]any{"limit": 100}}, &apps); err != nil {
			// One unreadable project must not blank the others.
			continue
		}
		for _, app := range apps.Result {
			entry := authApp{Name: app.Name, Type: "unknown"}

			switch {
			case app.OIDCConfig != nil:
				entry.Type = oidcType(app.OIDCConfig.AppType, app.OIDCConfig.GrantTypes)
				if app.OIDCConfig.ClientID != "" {
					id := app.OIDCConfig.ClientID
					entry.ClientId = &id
				}
				if len(app.OIDCConfig.RedirectURIs) > 0 {
					uris := app.OIDCConfig.RedirectURIs
					entry.RedirectUris = &uris
				}
			case app.APIConfig != nil:
				entry.Type = "API"
				if app.APIConfig.ClientID != "" {
					id := app.APIConfig.ClientID
					entry.ClientId = &id
				}
			}
			summary.Applications = append(summary.Applications, entry)
		}
	}

	sort.Slice(summary.Applications, func(i, j int) bool {
		return summary.Applications[i].Name < summary.Applications[j].Name
	})

	// User count comes from the search total rather than paging every user.
	//
	// Zitadel speaks proto3 JSON, which omits default values: an org with no
	// users returns no totalResult field at all. A successful search with an
	// empty total therefore means zero, not unknown — leaving it nil would
	// render "—" where the honest answer is "0".
	var users countResponse
	if err := z.post(ctx, "/management/v1/users/_search", orgID,
		map[string]any{"query": map[string]any{"limit": 1}}, &users); err == nil {
		count := 0
		if users.Details.TotalResult != "" {
			parsed, convErr := strconv.Atoi(users.Details.TotalResult)
			if convErr != nil {
				// An unparseable total is genuinely unknown; say nothing.
				return summary, nil
			}
			count = parsed
		}
		summary.UserCount = &count
	}

	return summary, nil
}

// oidcType renders an application's flow in the vocabulary an operator reads on
// the screen, rather than Zitadel's enum names.
func oidcType(appType string, grants []string) string {
	kind := strings.TrimPrefix(strings.ToUpper(appType), "OIDC_APP_TYPE_")
	switch kind {
	case "WEB":
		kind = "Web"
	case "USER_AGENT":
		kind = "SPA"
	case "NATIVE":
		kind = "Native"
	default:
		kind = "OIDC"
	}

	for _, grant := range grants {
		if strings.Contains(strings.ToUpper(grant), "AUTHORIZATION_CODE") {
			return kind + " / PKCE"
		}
	}
	return kind
}
