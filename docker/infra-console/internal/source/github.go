package source

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// GitHubClient resolves a commit to the workflow run that deployed it, for
// actor and duration. No tenant CI callback: deploys set
// image.tag=${{ github.sha }}, so the image tag IS the run's head_sha.
type GitHubClient struct {
	token string
	base  string
	hc    *http.Client
}

// NewGitHub builds a client. An empty token yields a client that reports itself
// unconfigured rather than making unauthenticated (heavily rate-limited) calls.
func NewGitHubClient(token string) *GitHubClient {
	return &GitHubClient{
		token: token,
		base:  "https://api.github.com",
		hc:    &http.Client{Timeout: 10 * time.Second},
	}
}

// Configured reports whether the client can make authenticated calls.
func (g *GitHubClient) Configured() bool { return g != nil && g.token != "" }

// Run is the subset of a workflow run the console shows.
type Run struct {
	Actor       string
	Conclusion  string
	StartedAt   time.Time
	CompletedAt time.Time
}

// Duration is the run's wall time, or zero when it has not finished.
func (r Run) Duration() time.Duration {
	if r.StartedAt.IsZero() || r.CompletedAt.IsZero() || r.CompletedAt.Before(r.StartedAt) {
		return 0
	}
	return r.CompletedAt.Sub(r.StartedAt)
}

type runsResponse struct {
	WorkflowRuns []struct {
		Actor struct {
			Login string `json:"login"`
		} `json:"actor"`
		Conclusion string `json:"conclusion"`
		RunStarted string `json:"run_started_at"`
		CreatedAt  string `json:"created_at"`
		UpdatedAt  string `json:"updated_at"`
	} `json:"workflow_runs"`
}

// RunForCommit returns the most recent workflow run for a commit, or
// (nil, nil) when there is none — images built elsewhere are not an error, and
// the caller renders the deployment unenriched.
func (g *GitHubClient) RunForCommit(ctx context.Context, owner, repo, sha string) (*Run, error) {
	if !g.Configured() {
		return nil, fmt.Errorf("github token not configured")
	}
	if owner == "" || repo == "" || sha == "" {
		return nil, fmt.Errorf("owner, repo and sha are all required")
	}

	endpoint := fmt.Sprintf("%s/repos/%s/%s/actions/runs?head_sha=%s&per_page=1",
		g.base, url.PathEscape(owner), url.PathEscape(repo), url.QueryEscape(sha))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Authorization", "Bearer "+g.token)

	resp, err := g.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		// An unknown repo is a mapping problem, not an outage. Treat it the
		// same as "no run": the timeline still renders.
		return nil, nil
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests:
		return nil, fmt.Errorf("github rate limited (status %d)", resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("github returned status %d", resp.StatusCode)
	}

	var body runsResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("decode github response: %w", err)
	}
	if len(body.WorkflowRuns) == 0 {
		return nil, nil
	}

	raw := body.WorkflowRuns[0]
	run := &Run{Actor: raw.Actor.Login, Conclusion: raw.Conclusion}
	// run_started_at is the accurate start; created_at is when the run was
	// queued and overstates duration by the queue wait.
	if t, err := time.Parse(time.RFC3339, raw.RunStarted); err == nil {
		run.StartedAt = t
	} else if t, err := time.Parse(time.RFC3339, raw.CreatedAt); err == nil {
		run.StartedAt = t
	}
	if t, err := time.Parse(time.RFC3339, raw.UpdatedAt); err == nil {
		run.CompletedAt = t
	}
	return run, nil
}

// ImageRef is a parsed container reference.
type ImageRef struct {
	Registry string
	Owner    string
	Repo     string
	Tag      string
}

// ParseImageRef splits a container reference. The GHCR convention is
// ghcr.io/{org}/{app}, so the repository is derivable from a running image and
// needs no per-tenant configuration.
func ParseImageRef(image string) (ImageRef, bool) {
	if image == "" {
		return ImageRef{}, false
	}

	// Strip a digest; a tag and a digest can both be present.
	if at := strings.Index(image, "@"); at >= 0 {
		image = image[:at]
	}

	var tag string
	// Only treat a colon after the last slash as a tag separator, so a
	// registry port (registry:5000/x) is not mistaken for one.
	if slash := strings.LastIndex(image, "/"); slash >= 0 {
		if colon := strings.LastIndex(image[slash:], ":"); colon >= 0 {
			tag = image[slash+colon+1:]
			image = image[:slash+colon]
		}
	} else if colon := strings.LastIndex(image, ":"); colon >= 0 {
		tag = image[colon+1:]
		image = image[:colon]
	}

	parts := strings.Split(image, "/")
	if len(parts) < 3 {
		// No registry host: not a GHCR path we can map to a repository.
		return ImageRef{Tag: tag}, false
	}
	return ImageRef{
		Registry: parts[0],
		Owner:    parts[1],
		// Nested paths (ghcr.io/org/team/app) keep everything after the owner.
		Repo: strings.Join(parts[2:], "/"),
		Tag:  tag,
	}, true
}
